// Package gateway brokers a session's model traffic through a loopback proxy.
//
// It exists because of a measured property of Claude Code: the Bash tool is a
// child process, so it inherits the full environment. Verified against 2.1.220 --
// AWS_ACCESS_KEY_ID, AWS_SESSION_TOKEN, AWS_CONTAINER_CREDENTIALS_RELATIVE_URI
// and ANTHROPIC_AUTH_TOKEN all reach a Bash command intact. A gateway key placed
// in the session's environment is therefore a key handed to every command the
// agent runs, and the agent runs LLM-generated code.
//
// So the key never enters the session at all:
//
//	claude (ANTHROPIC_BASE_URL=http://127.0.0.1:<port>)
//	  └─> supervisor's proxy   adds Authorization + attribution headers
//	        └─> the customer's gateway
//
// One listener per session, which is what makes attribution unforgeable: the
// port-to-session mapping is the supervisor's own bookkeeping, not something the
// agent can influence. A per-session header would be forgeable, because it would
// have to live in the session's environment to get there.
//
// What this does NOT do is stop the session using the proxy. ANTHROPIC_BASE_URL
// is visible to every process in the sandbox, so a Bash command can call this
// port and get a completion. That is deliberate rather than a gap: the agent
// legitimately has inference and legitimately runs arbitrary code, so it could
// always just ask Claude Code for one. The property being bought is narrower and
// still worth having -- an exfiltratable, unattributable capability becomes a
// non-exfiltratable, always-attributed one. A loopback port on an ephemeral
// number is useless outside the container and dies with the session, and every
// call through it is tagged whoever makes it. Spend is bounded by a per-workspace
// budget at the gateway, not by this proxy.
package gateway

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Options configures the broker for one workspace.
type Options struct {
	// Upstream is the customer's gateway, e.g. https://litellm.internal:4000.
	Upstream string
	// APIKey is the gateway credential. It is held in this process only and is
	// never written into a session's environment.
	APIKey string
	// WorkspaceID and UserID are injected for cost attribution. UserID may be
	// empty until there is a user model (Phase 3); workspace and session
	// attribution do not depend on it.
	WorkspaceID string
	UserID      string
}

// Broker owns the per-session listeners for one workspace.
type Broker struct {
	opt    Options
	target *url.URL

	mu       sync.Mutex
	sessions map[string]*SessionProxy
}

func New(o Options) (*Broker, error) {
	if o.Upstream == "" {
		return nil, fmt.Errorf("no gateway upstream configured")
	}
	u, err := url.Parse(o.Upstream)
	if err != nil {
		return nil, fmt.Errorf("parse gateway upstream: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("gateway upstream must be an absolute URL, got %q", o.Upstream)
	}
	return &Broker{opt: o, target: u, sessions: make(map[string]*SessionProxy)}, nil
}

// SessionProxy is one session's loopback listener.
type SessionProxy struct {
	SessionID string
	// BaseURL is what the session's ANTHROPIC_BASE_URL is set to.
	BaseURL string

	ln  net.Listener
	srv *http.Server
}

// Open starts a listener for a session. Binding to 127.0.0.1 keeps it
// unreachable from outside the task; binding to port 0 lets the OS allocate,
// so nothing has to manage a port range.
func (b *Broker) Open(sessionID string) (*SessionProxy, error) {
	b.mu.Lock()
	if sp, ok := b.sessions[sessionID]; ok {
		b.mu.Unlock()
		return sp, nil
	}
	b.mu.Unlock()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	sp := &SessionProxy{
		SessionID: sessionID,
		BaseURL:   "http://" + ln.Addr().String(),
		ln:        ln,
	}
	sp.srv = &http.Server{Handler: b.handler(sessionID)}
	go func() { _ = sp.srv.Serve(ln) }()

	b.mu.Lock()
	b.sessions[sessionID] = sp
	b.mu.Unlock()
	return sp, nil
}

// Close stops a session's listener.
func (b *Broker) Close(sessionID string) {
	b.mu.Lock()
	sp, ok := b.sessions[sessionID]
	delete(b.sessions, sessionID)
	b.mu.Unlock()
	if ok {
		_ = sp.srv.Close()
	}
}

func (b *Broker) CloseAll() {
	b.mu.Lock()
	all := make([]*SessionProxy, 0, len(b.sessions))
	for _, sp := range b.sessions {
		all = append(all, sp)
	}
	b.sessions = make(map[string]*SessionProxy)
	b.mu.Unlock()
	for _, sp := range all {
		_ = sp.srv.Close()
	}
}

func (b *Broker) handler(sessionID string) http.Handler {
	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(b.target)
			r.Out.Host = b.target.Host

			// Whatever the session sent as credentials is discarded. Claude Code
			// is given a placeholder token precisely so that nothing of value is
			// ever in its environment; forwarding it would defeat the point.
			r.Out.Header.Del("Authorization")
			r.Out.Header.Del("x-api-key")
			r.Out.Header.Set("Authorization", "Bearer "+b.opt.APIKey)

			// Attribution the session cannot forge: these are set here, from the
			// supervisor's own mapping, and any same-named header the session
			// sent has already been dropped by SetURL/Del below.
			r.Out.Header.Del("x-litellm-tags")
			r.Out.Header.Del("x-litellm-end-user-id")
			tags := []string{
				"workspace:" + b.opt.WorkspaceID,
				"session:" + sessionID,
			}
			if b.opt.UserID != "" {
				tags = append(tags, "user:"+b.opt.UserID)
				r.Out.Header.Set("x-litellm-end-user-id", b.opt.UserID)
			}
			r.Out.Header.Set("x-litellm-tags", strings.Join(tags, ","))
		},

		// Negative means flush immediately, which is what SSE needs. Claude Code
		// streams in interactive use, so any buffering here would show up as the
		// UI freezing until a response completed.
		FlushInterval: -1,

		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			// Shaped like an Anthropic error so Claude Code renders something
			// meaningful rather than choking on an unexpected body.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprintf(w, `{"type":"error","error":{"type":"api_error","message":%q}}`,
				"gateway unreachable: "+err.Error())
		},

		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			MaxIdleConnsPerHost: 8,
			// No response header timeout: a long agent turn can take minutes
			// before the first byte, and cutting it off would look like a crash.
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
	return rp
}

// PlaceholderToken is what a session's ANTHROPIC_AUTH_TOKEN is set to.
//
// Claude Code requires an auth value to be present, but the proxy replaces it,
// so this is deliberately something that is obviously not a secret if it ever
// shows up in a log, a screenshot or a pasted environment dump.
const PlaceholderToken = "sk-lemul-loopback-not-a-secret"
