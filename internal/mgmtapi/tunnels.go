package mgmtapi

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/lufzle/lemul/internal/agent"
	"github.com/lufzle/lemul/internal/directory"
	"github.com/lufzle/lemul/internal/registry"
	"github.com/lufzle/lemul/internal/store"
	"github.com/lufzle/lemul/internal/tunnel"
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

// checkProtocol refuses an agent whose wire format this control plane cannot
// speak, and says which side is stale.
//
// Checked at the upgrade rather than on the first frame: a peer we cannot
// understand should never reach a yamux session, and after the handshake there
// is no good place to notice. The message names the actual fix, because the
// symptoms of skew -- decode errors, unhandled message types, streams that open
// and close -- all read like an application bug (section 13).
//
// Runs AFTER the credential check on both tunnels, deliberately. These messages
// are diagnostics for an operator whose deployment is mismatched, and an
// unauthenticated caller is owed a flat 401 rather than a reading of which
// versions we speak.
func (s *Server) checkProtocol(w http.ResponseWriter, r *http.Request) bool {
	raw := r.URL.Query().Get(tunnel.ProtocolParam)
	if raw == "" {
		// No version at all means an agent from before this existed.
		http.Error(w, fmt.Sprintf(
			"this agent announces no protocol version; it predates protocol %d. Roll the workspace image forward.",
			tunnel.MinProtocolVersion), http.StatusBadRequest)
		return false
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		http.Error(w, "malformed protocol version", http.StatusBadRequest)
		return false
	}
	switch {
	case v < tunnel.MinProtocolVersion:
		s.log.Warn("refused agent: protocol too old",
			"agent_protocol", v, "minimum", tunnel.MinProtocolVersion)
		http.Error(w, fmt.Sprintf(
			"agent protocol %d is older than the minimum %d. Roll the workspace image forward.",
			v, tunnel.MinProtocolVersion), http.StatusBadRequest)
		return false
	case v > tunnel.ProtocolVersion:
		// The control plane is the stale one. Worth distinguishing: rolling the
		// image forward is the wrong advice here and would waste a deploy.
		s.log.Warn("refused agent: protocol newer than this control plane",
			"agent_protocol", v, "ours", tunnel.ProtocolVersion)
		http.Error(w, fmt.Sprintf(
			"agent protocol %d is newer than this control plane's %d. Update the control plane.",
			v, tunnel.ProtocolVersion), http.StatusBadRequest)
		return false
	}
	return true
}

// handleRunnerTunnel accepts a runner's outbound connection.
//
// Runner tunnels are held as a SET per organization (section 2.8): dispatch
// picks any live one, and a tunnel dropping is itself the failover signal. We
// deploy one replica; nothing here assumes it.
//
// The credential is DERIVED FOR THE ORGANIZATION the runner claims, which is
// what makes the claim mean anything. It used to be one shared secret for the
// whole fleet, with the organization taken from a query parameter nobody
// checked -- so any runner could register as any organization and receive its
// workspace placements, including the credentials to reach them. Invisible with
// one tenant; the first real hole with two.
func (s *Server) handleRunnerTunnel(w http.ResponseWriter, r *http.Request) {
	tenant := r.URL.Query().Get("tenant")
	if tenant == "" {
		http.Error(w, "missing tenant", http.StatusBadRequest)
		return
	}
	if !s.signer.VerifyRunner(tenant, bearer(r)) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !s.checkProtocol(w, r) {
		return
	}
	id := r.URL.Query().Get("runner_id")

	sess, ok := s.upgradeToYamux(w, r)
	if !ok {
		return
	}
	t := registry.NewTunnel(id, tenant, "", r.RemoteAddr, sess)
	s.reg.AddRunner(t)
	s.log.Info("runner tunnel up",
		"tenant", tenant, "runner", id, "from", r.RemoteAddr, "runners", s.reg.RunnerCount(tenant))
	defer func() {
		s.reg.RemoveRunner(t)
		s.log.Info("runner tunnel down",
			"tenant", tenant, "runner", id, "runners", s.reg.RunnerCount(tenant))
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
	// The credential is derived from the workspace's CURRENT generation, so the
	// record is the authority on what is valid -- there is no separate map to
	// consult, and a restarted control plane recomputes the same answer its
	// predecessor would have given.
	//
	// A task holding a previous generation's credential fails here, which is
	// exactly right: it has been replaced. That is also why the generation must
	// only advance on a real placement (see ensureWorkspace) -- advancing it
	// because a tunnel was briefly missing would lock out a task that is alive.
	//
	// Scoped to the organization the task announced -- and since the credential
	// covers that organization too, a task cannot look up a workspace in one
	// organization and authenticate with another's credential.
	sc := scope{Scope: store.Scope{TenantID: tenant}, Org: directory.Org{ID: tenant}}
	ws, err := s.getWorkspaceByName(r.Context(), sc, wid)
	if err != nil || !s.signer.VerifyWorkspace(tenant, wid, uint64(ws.Generation), bearer(r)) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !s.checkProtocol(w, r) {
		return
	}

	sess, ok := s.upgradeToYamux(w, r)
	if !ok {
		return
	}
	t := registry.NewTunnel(wid, tenant, wid, r.RemoteAddr, sess)
	s.reg.AddWorkspace(t)
	s.log.Info("workspace tunnel up", "workspace", wid, "from", r.RemoteAddr)
	defer func() {
		s.reg.RemoveWorkspace(t)
		// Drop the verdict with the task that produced it: a replacement task
		// must be judged on its own check, not the previous one's. Access is
		// often granted after a failure, and that is the recovery path.
		s.preflights.forget(wsRef{tenant, wid})
		// Same for the activity report, and here it is load-bearing rather
		// than tidy. A report left behind describes sessions that died with
		// the task, so it would read as four conditions all satisfied -- the
		// sweeper's staleness rule already stops it acting on that, and
		// dropping it means the rule never has to.
		s.activity.forget(wsRef{tenant, wid})
		s.log.Info("workspace tunnel down", "workspace", wid)
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
		s.log.Error("yamux session setup failed", "error", err)
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
		// Filed under the TUNNEL's organization rather than anything in the
		// report, for the reason the preflight case below spells out: the
		// workspace id is the task's own claim, while the tunnel's tenant was
		// established by a credential.
		if h.WorkspaceID == "" {
			h.WorkspaceID = t.WorkspaceName
		}
		s.activity.put(t.TenantID, h, time.Now())
		s.log.Debug("headroom reported",
			"workspace", h.WorkspaceID, "mem_free_mb", h.MemFreeMB,
			"mem_limit_mb", h.MemLimitMB, "sessions", h.Sessions,
			"activity", len(h.Activity))
	case tunnel.MsgPreflight:
		var rep tunnel.PreflightReport
		if err := env.Decode(&rep); err != nil {
			return
		}
		if rep.WorkspaceID == "" {
			rep.WorkspaceID = t.WorkspaceName
		}
		// Filed under the tunnel's organization, not the report's -- the report
		// is a task telling us about itself, and which organization it belongs
		// to was settled by the credential it presented at the upgrade.
		s.preflights.put(t.TenantID, rep)
		switch {
		case rep.Skipped:
			s.log.Info("bedrock preflight skipped: not using bedrock", "workspace", rep.WorkspaceID)
		case rep.Blocking:
			m, _ := rep.FirstBlocker()
			s.log.Error("bedrock preflight FAILED",
				"workspace", rep.WorkspaceID, "model", m.ModelID,
				"code", m.ErrorCode, "advice", m.Advice)
		default:
			s.log.Info("bedrock preflight ok", "workspace", rep.WorkspaceID, "region", rep.Region)
		}
	case tunnel.MsgSessionExited:
		var e tunnel.SessionExited
		if err := env.Decode(&e); err != nil {
			return
		}
		s.log.Info("session exited",
			"workspace", e.WorkspaceID, "session", e.SessionID, "exit_code", e.ExitCode)
	default:
		s.log.Warn("unhandled event", "tunnel", t.ID, "type", env.Type)
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

// newSessionID mints a session id as a v4 UUID.
//
// The format is not cosmetic. Claude Code's --session-id requires a valid UUID,
// and adopting it makes our session id BE the conversation id rather than
// something mapped to one. Three things fall out: resume needs no lookup table;
// the id survives a resume (2.1.220 reuses it -- only --fork-session mints a new
// one), which section 2.4 wants for vertical migration; and the workaround in
// section 12.4 goes away, since the gateway's own Session ID field and OTel's
// session.id now carry our id directly.
//
// Hand-rolled rather than pulling in a dependency for sixteen bytes.
func newSessionID() string { return newUUID() }

// newUUID mints a v4 UUID. Hand-rolled rather than pulling in a dependency for
// sixteen bytes, and shared by session ids (where the format is required, see
// newSessionID) and workspace ids (where it is merely right).
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant RFC 4122
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
