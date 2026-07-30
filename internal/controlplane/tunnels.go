package controlplane

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/lufzle/lemul-cc/internal/agent"
	"github.com/lufzle/lemul-cc/internal/registry"
	"github.com/lufzle/lemul-cc/internal/tunnel"
)

// bearer extracts the token an agent presents at the HTTP upgrade.
//
// Authenticating here rather than in a first control frame means an
// unauthenticated peer never gets a yamux session at all.
func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if after, ok := strings.CutPrefix(h, "Bearer "); ok {
		return after
	}
	return ""
}

func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// handleRunnerTunnel accepts a runner's outbound connection.
//
// Runner tunnels are held as a SET per tenant (section 2.8): dispatch picks any
// live one, and a tunnel dropping is itself the failover signal. We deploy one
// replica; nothing here assumes it.
func (s *Server) handleRunnerTunnel(w http.ResponseWriter, r *http.Request) {
	if !constantTimeEqual(bearer(r), s.opt.AgentToken) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	tenant := r.URL.Query().Get("tenant")
	if tenant == "" {
		http.Error(w, "missing tenant", http.StatusBadRequest)
		return
	}
	id := r.URL.Query().Get("runner_id")

	sess, ok := s.upgradeToYamux(w, r)
	if !ok {
		return
	}
	t := registry.NewTunnel(id, tenant, "", r.RemoteAddr, sess)
	s.reg.AddRunner(t)
	log.Printf("runner tunnel up: tenant=%s id=%s from=%s (now %d)",
		tenant, id, r.RemoteAddr, s.reg.RunnerCount(tenant))
	defer func() {
		s.reg.RemoveRunner(t)
		log.Printf("runner tunnel down: tenant=%s id=%s (now %d)", tenant, id, s.reg.RunnerCount(tenant))
	}()

	s.drainEvents(t)
}

// handleWorkspaceTunnel accepts a workspace task's outbound connection.
//
// The credential is workspace-scoped and was minted when the runner was told to
// place this task, so presenting it is what binds this tunnel to that workspace
// (decision #6). A task cannot register as a workspace it was not started for.
func (s *Server) handleWorkspaceTunnel(w http.ResponseWriter, r *http.Request) {
	tenant := r.URL.Query().Get("tenant")
	wid := r.URL.Query().Get("workspace")
	if tenant == "" || wid == "" {
		http.Error(w, "missing tenant or workspace", http.StatusBadRequest)
		return
	}
	if !s.creds.checkWorkspace(wid, bearer(r)) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	sess, ok := s.upgradeToYamux(w, r)
	if !ok {
		return
	}
	t := registry.NewTunnel(wid, tenant, wid, r.RemoteAddr, sess)
	s.reg.AddWorkspace(t)
	log.Printf("workspace tunnel up: %s from=%s", wid, r.RemoteAddr)
	defer func() {
		s.reg.RemoveWorkspace(t)
		// Drop the verdict with the task that produced it: a replacement task
		// must be judged on its own check, not the previous one's. Access is
		// often granted after a failure, and that is the recovery path.
		s.preflights.forget(wid)
		log.Printf("workspace tunnel down: %s", wid)
	}()

	s.drainEvents(t)
}

func (s *Server) upgradeToYamux(w http.ResponseWriter, r *http.Request) (*yamux.Session, bool) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return nil, false
	}
	// The control plane is the yamux server on both tunnels: it opens command
	// streams, the agent accepts them. The agent opens exactly one stream back,
	// for events.
	sess, err := yamux.Server(tunnel.NewWSConn(ws), agent.YamuxConfig())
	if err != nil {
		log.Printf("yamux: %v", err)
		_ = ws.Close()
		return nil, false
	}
	return sess, true
}

// drainEvents accepts the agent's single event stream and consumes it until the
// tunnel closes. Blocking here is what keeps the handler (and thus the tunnel)
// alive for the connection's lifetime.
func (s *Server) drainEvents(t *registry.Tunnel) {
	stream, err := t.Accept()
	if err != nil {
		<-t.Closed()
		return
	}
	defer func() { _ = stream.Close() }()

	for {
		env, err := tunnel.ReadMsg(stream)
		if err != nil {
			return
		}
		s.handleEvent(t, env)
	}
}

func (s *Server) handleEvent(t *registry.Tunnel, env tunnel.Envelope) {
	switch env.Type {
	case tunnel.MsgHeadroom:
		var h tunnel.Headroom
		if err := env.Decode(&h); err != nil {
			return
		}
		// Admission control consumes this in the lifecycle increment; logging it
		// now proves the reporter works end to end.
		log.Printf("headroom %s: %d/%d MB free, %d session(s)",
			h.WorkspaceID, h.MemFreeMB, h.MemLimitMB, h.Sessions)
	case tunnel.MsgPreflight:
		var rep tunnel.PreflightReport
		if err := env.Decode(&rep); err != nil {
			return
		}
		if rep.WorkspaceID == "" {
			rep.WorkspaceID = t.WorkspaceID
		}
		s.preflights.put(rep)
		switch {
		case rep.Skipped:
			log.Printf("workspace %s: bedrock preflight skipped (not using bedrock)", rep.WorkspaceID)
		case rep.Blocking:
			m, _ := rep.FirstBlocker()
			log.Printf("workspace %s: BEDROCK PREFLIGHT FAILED -- %s (%s): %s",
				rep.WorkspaceID, m.ModelID, m.ErrorCode, m.Advice)
		default:
			log.Printf("workspace %s: bedrock preflight ok (%s)", rep.WorkspaceID, rep.Region)
		}
	case tunnel.MsgSessionExited:
		var e tunnel.SessionExited
		if err := env.Decode(&e); err != nil {
			return
		}
		log.Printf("session %s exited (code %d)", e.SessionID, e.ExitCode)
	default:
		log.Printf("unhandled event from %s: %s", t.ID, env.Type)
	}
}

// command opens a stream to an agent, sends one message and reads one reply.
// A command stream carries exactly one exchange, which is why no message needs
// a correlation ID.
func command(t *registry.Tunnel, typ string, req any, timeout time.Duration) (tunnel.Envelope, error) {
	stream, err := t.Open()
	if err != nil {
		return tunnel.Envelope{}, err
	}
	defer func() { _ = stream.Close() }()

	_ = stream.SetDeadline(time.Now().Add(timeout))
	if err := tunnel.WriteMsg(stream, typ, req); err != nil {
		return tunnel.Envelope{}, err
	}
	return tunnel.ReadMsg(stream)
}

// credentials issues the short-lived secrets on both hops: workspace tunnel
// credentials (runner -> task -> relay) and one-shot client attach credentials
// handed out by endpoint negotiation.
type credentials struct {
	mu        sync.Mutex
	workspace map[string]string    // workspace id -> current credential
	attach    map[string]attachCre // credential -> session
}

type attachCre struct {
	sessionID string
	expires   time.Time
}

func newCredentials() *credentials {
	return &credentials{
		workspace: make(map[string]string),
		attach:    make(map[string]attachCre),
	}
}

func newSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// mintWorkspace replaces the workspace's credential. Minting on every placement
// means a credential from a previous generation cannot register the new task.
func (c *credentials) mintWorkspace(wid string) string {
	s := newSecret()
	c.mu.Lock()
	c.workspace[wid] = s
	c.mu.Unlock()
	return s
}

func (c *credentials) checkWorkspace(wid, tok string) bool {
	c.mu.Lock()
	want, ok := c.workspace[wid]
	c.mu.Unlock()
	return ok && tok != "" && constantTimeEqual(want, tok)
}

func (c *credentials) mintAttach(sessionID string, ttl time.Duration) string {
	s := newSecret()
	c.mu.Lock()
	c.attach[s] = attachCre{sessionID: sessionID, expires: time.Now().Add(ttl)}
	// Opportunistic sweep: the map only grows on endpoint negotiation, so there
	// is no need for a dedicated janitor.
	for k, v := range c.attach {
		if time.Now().After(v.expires) {
			delete(c.attach, k)
		}
	}
	c.mu.Unlock()
	return s
}

// redeemAttach consumes a credential. Single-use: a leaked URL cannot be
// replayed to attach a second time.
func (c *credentials) redeemAttach(tok string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.attach[tok]
	if !ok || time.Now().After(v.expires) {
		delete(c.attach, tok)
		return "", false
	}
	delete(c.attach, tok)
	return v.sessionID, true
}
