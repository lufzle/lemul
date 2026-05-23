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
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
	"sync"
	"sync/atomic"
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

	// AllowModelDiscovery opens GET /v1/models, which Claude Code uses to
	// populate /model when CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY is on.
	// Off by default for the same reason the managed setting is: a shared key
	// would otherwise show every user every model that key can reach.
	AllowModelDiscovery bool
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

	// lastRequest is when this session last made a model call, in unix nanos --
	// section 2.4's third idle condition, and the reason the supervisor needs no
	// OTel to answer "is the agent working".
	//
	// Atomic rather than mutex-guarded: it is written on the hot path of a
	// streaming proxy and read once per reporting tick, so a lock would be
	// contention bought for nothing.
	lastRequest atomic.Int64
}

// LastRequest is when this session last had a request FORWARDED upstream.
//
// Denied requests deliberately do not count. The condition is "no model
// requests", and a retry loop hammering a non-inference route is not a working
// agent -- counting it would let a wedged session hold a task open forever,
// which is the failure idle detection exists to end.
func (sp *SessionProxy) LastRequest() time.Time {
	return time.Unix(0, sp.lastRequest.Load())
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

	// The listener lives as long as the session, and is closed by Close/CloseAll
	// rather than by a context. A ListenConfig bound to a request context would
	// take the session's inference endpoint away mid-turn.
	//nolint:noctx // the listener's lifetime is the session's, not a request's
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	sp := &SessionProxy{
		SessionID: sessionID,
		BaseURL:   "http://" + ln.Addr().String(),
		ln:        ln,
	}
	// Seeded to now rather than left zero, for the same reason the PTY's
	// last-output is: "has made no model call yet" and "has been quiet since it
	// started" are the same fact, and a zero here would read as idle since 1970.
	sp.lastRequest.Store(time.Now().UnixNano())
	sp.srv = &http.Server{
		Handler: b.handler(sp),
		// Without this a peer can hold a connection open indefinitely by
		// dribbling headers. It is loopback-only, so the attacker would have to
		// be inside the sandbox -- which is exactly the party this proxy exists
		// to constrain, and a session that can wedge its own inference endpoint
		// is a self-inflicted outage worth not offering.
		ReadHeaderTimeout: 15 * time.Second,
	}
	go func() { _ = sp.srv.Serve(ln) }()

	b.mu.Lock()
	b.sessions[sessionID] = sp
	b.mu.Unlock()
	return sp, nil
}

// LastRequest reports when a session last reached the gateway, and whether this
// broker has a listener for it at all.
//
// The second return matters: a workspace running without a gateway configured
// has no proxy for any session, and the caller must read that as "this signal
// is unavailable" rather than as "this session has never made a model call".
// The two look identical in a bare timestamp and mean opposite things for idle
// detection.
func (b *Broker) LastRequest(sessionID string) (time.Time, bool) {
	b.mu.Lock()
	sp, ok := b.sessions[sessionID]
	b.mu.Unlock()
	if !ok {
		return time.Time{}, false
	}
	return sp.LastRequest(), true
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

// allows reports whether a cleaned request path is one this broker will sign.
//
// The credential is out of the session's reach, but the CAPABILITY is not: the
// proxy attaches the real key to whatever it forwards, so without this the
// session directs a fully authenticated request at any route the gateway
// exposes. On LiteLLM that includes /key/generate and /spend/logs -- key
// minting and every other tenant's spend history -- which turns "the session can
// spend, always attributed" into "the session can do whatever the key can".
//
// So the surface is an allowlist of inference, not a denylist of known-dangerous
// routes. A denylist would need updating every time the gateway grew an endpoint,
// and would fail open in the meantime.
func (b *Broker) allows(method, cleanPath string) bool {
	switch cleanPath {
	case "/v1/messages", "/v1/messages/count_tokens":
		return method == http.MethodPost
	case "/v1/models":
		return b.opt.AllowModelDiscovery && method == http.MethodGet
	}
	// /v1/models/<id>, the detail form of the same read.
	if b.opt.AllowModelDiscovery && method == http.MethodGet &&
		strings.HasPrefix(cleanPath, "/v1/models/") {
		return true
	}
	return false
}

func (b *Broker) handler(sp *SessionProxy) http.Handler {
	sessionID := sp.SessionID
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

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Decide on the CLEANED path, and forward the cleaned path, so the two
		// cannot disagree. Go does not resolve dot segments in a bare handler's
		// URL, so "/v1/messages/../key/generate" would otherwise pass a literal
		// comparison here and still be routed to /key/generate by the gateway.
		clean := path.Clean(r.URL.Path)
		if !b.allows(r.Method, clean) {
			// Loud, because the failure mode of an allowlist is a legitimate
			// endpoint nobody thought of: this line is what turns that into a
			// one-line diagnosis instead of a mystery 404 inside the TUI.
			slog.Warn("gateway request denied: not an inference route",
				"session", sessionID, "method", r.Method, "path", clean)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, `{"type":"error","error":{"type":"not_found_error","message":%q}}`,
				"this proxy forwards inference requests only")
			return
		}
		r.URL.Path = clean
		// Recorded before the round trip, not after. A streaming turn can take
		// minutes to complete, and marking activity only on completion would
		// leave the whole turn looking idle to section 2.4's third condition.
		sp.lastRequest.Store(time.Now().UnixNano())
		rp.ServeHTTP(w, r)
	})
}

// PlaceholderToken is what a session's ANTHROPIC_AUTH_TOKEN is set to.
//
// Claude Code requires an auth value to be present, but the proxy replaces it,
// so this is deliberately something that is obviously not a secret if it ever
// shows up in a log, a screenshot or a pasted environment dump.
const PlaceholderToken = "sk-lemul-loopback-not-a-secret"
