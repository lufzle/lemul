// Package supervisor runs inside one workspace task and owns that workspace's
// PTYs -- one per session (section 2.3).
//
// It is the data-plane half of the runner/supervisor split (decision #6,
// task-dials-out). It holds its OWN outbound tunnel rather than being proxied
// by the runner, which is what keeps the runner off the data path: the runner
// can fail, redeploy or auto-update without touching a running session.
package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lufzle/lemul/internal/agent"
	"github.com/lufzle/lemul/internal/bedrock"
	"github.com/lufzle/lemul/internal/gateway"
	"github.com/lufzle/lemul/internal/logging"
	"github.com/lufzle/lemul/internal/ptysession"
	"github.com/lufzle/lemul/internal/tunnel"
)

type Options struct {
	// ControlPlane is the Management API, reached on the CONTROL tunnel: list,
	// stop, delete, the explorer verbs, preflight and headroom.
	ControlPlane string
	// Relay is the session relay, reached on the DATA tunnel: attach streams and
	// nothing else. Empty falls back to ControlPlane, which is what keeps a
	// single-process development setup working.
	//
	// Two tunnels rather than one is what lets the relay see only opaque bytes.
	// A single tunnel carries directory listings, process lists and command
	// lines beside the PTY stream, so no service on it could ever claim to hold
	// nothing that decrypts a session (section 2.7).
	Relay       string
	TenantID    string
	WorkspaceID string
	// Generation is announced on the DATA tunnel, because the relay holds no
	// database and cannot look it up. See the relay's tunnel handler for why
	// that is sound only alongside the control tunnel's check.
	Generation uint64
	Token      string
	// DefaultCmd runs when an attach asks to create a session without naming one.
	DefaultCmd []string
	// ConfigDir is Claude Code's CLAUDE_CONFIG_DIR, where conversations are
	// filed. Empty resolves it the way Claude Code does. It has to sit on the
	// workspace volume -- that is what lets a conversation outlive its process
	// and makes resume mean anything (section 2.4).
	ConfigDir  string
	Term       string
	RingBytes  int
	NudgeDelay time.Duration
	// HeadroomInterval is how often resource headroom is reported upward.
	HeadroomInterval time.Duration

	// GatewayURL, when set, brokers all model traffic through a per-session
	// loopback proxy the supervisor owns (decision #12). This is the supported
	// inference path; direct-to-Bedrock is a draft.
	GatewayURL string
	// GatewayKey is the credential for that gateway. It is held in this process
	// only and is stripped from every session's environment.
	GatewayKey string
	// UserID is injected for gateway cost attribution. Optional until there is a
	// user model (Phase 3).
	UserID string

	// BedrockPreflight enables the model check at task start. Off means the
	// workspace is not using Bedrock -- local development against a host login --
	// and the report goes up marked skipped rather than being withheld.
	BedrockPreflight bool
	// Region for the Bedrock check; empty uses the environment's.
	Region string
	// Pins to check; empty uses bedrock.DefaultPins.
	Pins []bedrock.Pin

	// WorkspaceRoot is what the explorer may list, and the boundary it may not
	// cross. Empty defaults to the image's /workspace. It is a jail rather than
	// a convenience: this process runs as root, so nothing below it enforces
	// containment (fsjail.go).
	WorkspaceRoot string

	// SessionUID and SessionGID run sessions at a different uid from the
	// supervisor, which is what stops a session recovering GatewayKey from
	// /proc/<supervisor>/environ (sessionuid.go, section 12.6). Zero disables
	// the boundary; it is also ignored when this process is not root, which is
	// the local driver and the e2e suite.
	SessionUID uint32
	SessionGID uint32
}

// Supervisor implements agent.Handler.
type Supervisor struct {
	opt Options
	mgr *ptysession.Manager

	mu     sync.Mutex
	events net.Conn // nil while disconnected

	preflightOnce sync.Once
	preflight     tunnel.PreflightReport

	broker *gateway.Broker

	// sessionCred is nil when sessions run as this process, which is the local
	// driver and the e2e suite. In the image it is the unprivileged uid that
	// keeps GatewayKey out of a session's reach (sessionuid.go).
	//
	// It is the FLOOR rather than the answer: a workspace is shared, so each
	// member's sessions run at their own uid on top of this (identity.go). This
	// remains what an unprivileged supervisor uses, where there is no boundary
	// to draw in the first place.
	sessionCred *syscall.Credential

	// ids maps a session to the member who owns it, filled by the control tunnel
	// before anything can fork that session's PTY.
	ids *identities

	// activity remembers when each session last had a tool running, which is the
	// one of section 2.4's four conditions that can only be sampled (activity.go).
	activity *activityTracker

	// log carries this workspace on every line, so nothing below has to repeat
	// it and a multi-tenant log group can be filtered by it.
	log *slog.Logger

	samples  *sampler
	stopOnce sync.Once
	stopped  chan struct{}
	// samplerDone closes when the sampler goroutine has actually returned, which
	// is what makes Close synchronous. Signalling the stop is not enough: the
	// sampler can still be inside a procRoot read when close() returns.
	samplerDone chan struct{}
}

func New(o Options) *Supervisor {
	if len(o.DefaultCmd) == 0 {
		o.DefaultCmd = []string{"claude"}
	}
	if o.Term == "" {
		o.Term = "xterm-256color"
	}
	if o.HeadroomInterval <= 0 {
		o.HeadroomInterval = 30 * time.Second
	}
	if o.WorkspaceRoot == "" {
		o.WorkspaceRoot = defaultWorkspaceRoot()
	}
	s := &Supervisor{
		opt:      o,
		ids:      newIdentities(),
		activity: newActivityTracker(),
		stopped:  make(chan struct{}),
		log:      slog.Default().With("workspace", o.WorkspaceID),
	}

	// Resolved once, at startup, so a misconfiguration is a boot-time log rather
	// than a surprise on somebody's first attach.
	cred, err := sessionCredential(o.SessionUID, o.SessionGID)
	if err != nil {
		logging.Fatal("resolving the session uid", "error", err)
	}
	s.sessionCred = cred
	if cred != nil {
		s.log.Info("session uid boundary active",
			"session_uid", cred.Uid, "session_gid", cred.Gid, "supervisor_uid", os.Geteuid())
		for _, d := range []string{o.WorkspaceRoot, o.ConfigDir} {
			if err := checkSessionDir(d, cred); err != nil {
				s.log.Warn("session directory may be unusable", "error", err)
			}
		}
	} else if o.GatewayKey != "" && os.Geteuid() == 0 {
		// Root, holding a real credential, and no uid boundary: say so plainly.
		// This is the shape that leaks (section 12.6).
		s.log.Warn("sessions share this process's uid while it holds the gateway " +
			"credential; set -session-uid so they cannot read it from /proc")
	}

	// Sampling starts with the process, not with the first request: CPU and
	// network are rates, so the first caller would otherwise get a zero and a
	// wrong one at that.
	s.samples = newSampler(o.WorkspaceRoot)
	s.samplerDone = make(chan struct{})
	go func() {
		defer close(s.samplerDone)
		s.samples.run(s.stopped)
	}()
	if o.GatewayURL != "" {
		b, err := gateway.New(gateway.Options{
			Upstream:    o.GatewayURL,
			APIKey:      o.GatewayKey,
			WorkspaceID: o.WorkspaceID,
			UserID:      o.UserID,
			// Read from the same variable image/entrypoint.sh uses to turn on
			// CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY, so the setting and the
			// route it needs cannot be enabled independently of each other.
			AllowModelDiscovery: os.Getenv("LEMUL_GATEWAY_MODEL_DISCOVERY") == "1",
		})
		if err != nil {
			logging.Fatal("building the gateway broker", "error", err)
		}
		s.broker = b
		s.log.Info("brokering model traffic", "upstream", redactURL(o.GatewayURL))
	}
	s.mgr = ptysession.NewManager(ptysession.Options{
		RingBytes:  o.RingBytes,
		NudgeDelay: o.NudgeDelay,
		OnExit:     s.onSessionExit,
	})
	return s
}

// Sessions exposes the manager for tests and for a future local status command.
func (s *Supervisor) Sessions() *ptysession.Manager { return s.mgr }

// Close stops the background sampler.
//
// Production never calls it -- the supervisor's lifetime is the task's, which is
// why the sampler takes a channel nobody closes. A test that constructs one must,
// though: the sampler reads procRoot on a ticker, so a goroutine outliving its
// test races the next test that points procRoot at a fixture tree.
func (s *Supervisor) Close() {
	s.stopOnce.Do(func() { close(s.stopped) })
	<-s.samplerDone
}

// Run dials BOTH tunnels and serves until ctx is cancelled or the control
// tunnel gives up.
//
// The control tunnel is the one that decides this task's fate. Its 401 comes
// from the Management API, which read the workspace record, so it means this
// task has been replaced -- retrying cannot fix that, and a task that keeps
// trying holds the volume and bills forever. The data tunnel's 401 means only
// that the relay would not take the credential, which a redeploy produces, so
// it retries and the PTYs keep running unattached until it comes back.
func (s *Supervisor) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	relay := s.opt.Relay
	if relay == "" {
		relay = s.opt.ControlPlane
	}

	// The data tunnel runs until the context goes. It never returns a reason to
	// stop the task, so its error is logged rather than raced against the
	// control tunnel's.
	go func() {
		err := agent.Run(ctx, agent.Config{
			URL:   strings.TrimRight(relay, "/") + "/v1/tunnel/data",
			Token: s.opt.Token,
			Params: map[string]string{
				"tenant":     s.opt.TenantID,
				"workspace":  s.opt.WorkspaceID,
				"generation": strconv.FormatUint(s.opt.Generation, 10),
			},
			// See agent.Config: a relay that will not take our credential is not
			// a reason to end somebody's session.
			FatalOnUnauthorized: false,
		}, dataTunnel{s})
		if err != nil && ctx.Err() == nil {
			s.log.Error("data tunnel gave up; sessions keep running but nobody can attach",
				"error", err)
		}
	}()

	err := agent.Run(ctx, agent.Config{
		URL:   strings.TrimRight(s.opt.ControlPlane, "/") + "/v1/tunnel/workspace",
		Token: s.opt.Token,
		Params: map[string]string{
			"tenant":    s.opt.TenantID,
			"workspace": s.opt.WorkspaceID,
		},
		FatalOnUnauthorized: true,
	}, s)
	// Say why the task is going away. A workspace that disappears mid-run is
	// alarming, and "the control plane no longer recognises this task" is a
	// different problem from a crash -- it means a replacement placement is what
	// fixes it, not an investigation of this one.
	if errors.Is(err, agent.ErrUnauthorized) {
		s.log.Error("this task is orphaned; exiting so it can be reclaimed",
			"sessions_ending", s.mgr.Count(), "error", err)
	}
	s.mgr.StopAll(true)
	return err
}

// dataTunnel is the supervisor as the RELAY sees it: attach streams and
// nothing else.
//
// A separate handler rather than a flag on the main one, so the set of verbs
// reachable over the data tunnel is a thing you can read rather than a thing
// you have to audit. The relay only ever opens attach streams, but "only ever"
// is a claim about the peer, and this makes it a property of our own code.
type dataTunnel struct{ s *Supervisor }

// OnConnect must RETURN, promptly. agent.connectAndServe calls it inline and
// only then enters its accept loop, so blocking here leaves a tunnel that is
// registered and answers nothing -- which presents as an attach that hangs
// until its deadline rather than as anything pointing at this function.
//
// There is nothing to do on the data tunnel besides let the stream exist:
// headroom, preflight and session-exit are all control-plane facts and travel
// on the other one. The agent owns closing it.
func (d dataTunnel) OnConnect(net.Conn) error { return nil }

func (d dataTunnel) OnStream(stream net.Conn) { d.s.serveAttachOnly(stream) }

func (d dataTunnel) OnDisconnect(err error) {
	d.s.log.Warn("data tunnel down; sessions keep running", "error", err)
}

// OnConnect keeps the event stream and starts the headroom reporter. Reporting
// is unsolicited by design: the control plane admits or refuses new sessions
// against it, and polling for it would race the thing it is meant to prevent.
func (s *Supervisor) OnConnect(events net.Conn) error {
	s.mu.Lock()
	s.events = events
	s.mu.Unlock()

	go s.reportHeadroom(events)

	// Preflight runs after the tunnel is registered, deliberately. A workspace
	// whose Bedrock is misconfigured must still be visible and diagnosable
	// rather than silently absent -- the control plane cannot show an admin a
	// failure it never heard about.
	go func() {
		rep := s.preflightReport(context.Background())
		s.sendEvent(tunnel.MsgPreflight, rep)
	}()
	return nil
}

func (s *Supervisor) OnDisconnect(err error) {
	s.mu.Lock()
	s.events = nil
	s.mu.Unlock()
	// Deliberately does NOT touch the sessions. A dropped tunnel is a detach,
	// not an end: the PTYs keep running so an unattended agent run survives a
	// relay redeploy or a network partition (sections 2.4, 2.8).
	s.log.Warn("tunnel down; sessions keep running",
		"sessions", s.mgr.Count(), "error", err)
}

func (s *Supervisor) sendEvent(typ string, v any) {
	s.mu.Lock()
	ev := s.events
	s.mu.Unlock()
	if ev == nil {
		return
	}
	if err := tunnel.WriteMsg(ev, typ, v); err != nil {
		s.log.Error("sending event", "type", typ, "error", err)
	}
}

func (s *Supervisor) onSessionExit(id string, code int) {
	s.log.Info("session exited", "session", id, "exit_code", code)
	if s.broker != nil {
		s.broker.Close(id)
	}
	s.activity.forget(id)
	s.sendEvent(tunnel.MsgSessionExited, tunnel.SessionExited{
		WorkspaceID: s.opt.WorkspaceID,
		SessionID:   id,
		ExitCode:    code,
	})
}

func (s *Supervisor) reportHeadroom(events net.Conn) {
	t := time.NewTicker(s.opt.HeadroomInterval)
	defer t.Stop()
	for range t.C {
		s.mu.Lock()
		cur := s.events
		s.mu.Unlock()
		if cur != events {
			return // tunnel replaced
		}
		free, limit := memoryHeadroomMB()
		s.sendEvent(tunnel.MsgHeadroom, tunnel.Headroom{
			WorkspaceID: s.opt.WorkspaceID,
			MemFreeMB:   free,
			MemLimitMB:  limit,
			Sessions:    s.mgr.Count(),
			Activity:    s.sessionActivity(),
		})
	}
}

// OnStream dispatches one command stream from the control plane.
func (s *Supervisor) OnStream(stream net.Conn) {
	env, err := tunnel.ReadMsg(stream)
	if err != nil {
		s.log.Error("reading command stream", "error", err)
		_ = stream.Close()
		return
	}
	switch env.Type {
	case tunnel.MsgAttach:
		s.handleAttach(stream, env)
	case tunnel.MsgSessionIdentity:
		s.handleSessionIdentity(stream, env)
	case tunnel.MsgStartSession:
		s.handleStartSession(stream, env)
	case tunnel.MsgStopSession:
		s.handleStopSession(stream, env)
	case tunnel.MsgDeleteSession:
		s.handleDeleteSession(stream, env)
	case tunnel.MsgListSessions:
		s.handleListSessions(stream)
	case tunnel.MsgListDir:
		s.handleListDir(stream, env)
	case tunnel.MsgListProcesses:
		s.handleListProcesses(stream)
	case tunnel.MsgResources:
		s.handleResources(stream)
	default:
		replyError(stream, "unknown message type "+env.Type)
		_ = stream.Close()
	}
}

// serveAttachOnly is OnStream for the DATA tunnel: exactly one verb.
//
// The point of the split is that the relay carries session bytes and nothing
// else -- no directory listings, no process lists, no command lines. Enforcing
// that at THIS end rather than trusting the relay to only ask for attach is
// what makes it a property of the system instead of a property of the peer.
// Section 2.5's viewer mode is the precedent: input-dropping was enforced twice
// and still had a third door.
func (s *Supervisor) serveAttachOnly(stream net.Conn) {
	env, err := tunnel.ReadMsg(stream)
	if err != nil {
		s.log.Error("reading data stream", "error", err)
		_ = stream.Close()
		return
	}
	if env.Type != tunnel.MsgAttach {
		s.log.Warn("refused a non-attach verb on the data tunnel", "type", env.Type)
		replyError(stream, "the data tunnel carries attach and nothing else")
		_ = stream.Close()
		return
	}
	s.handleAttach(stream, env)
}

// createSession forks one session's process. Shared by attach-with-Create and by
// the start_session verb, so a session resumed from a console and one resumed by
// connecting a terminal get identical treatment -- including the argv.
//
// The argv is NOT a parameter, and that is the whole of what makes the previous
// sentence true. It used to arrive on the message, from two services that
// configured it independently and could disagree without a symptom (see
// tunnel.Attach). Taking it from this process's own configuration means the two
// paths cannot fork different programs, because there is no second value.
func (s *Supervisor) createSession(id, term string, rows, cols uint16) (*ptysession.Session, error) {
	cmd := s.opt.DefaultCmd
	if term == "" {
		term = s.opt.Term
	}
	// Who this session runs as, and therefore where its conversations live.
	who, err := s.identityFor(id)
	if err != nil {
		return nil, err
	}
	// Resume decided here, from the disk this process is standing on, rather than
	// from anything the control plane remembers (see claudeargs.go). Per member,
	// because the transcript is in THEIR config directory -- reading a shared one
	// would resume somebody else's conversation into their terminal.
	cmd = sessionArgs(cmd, id, who.configDir)

	// The proxy has to exist before the fork: its URL goes into the child's
	// environment, and a child that started without it would talk to nothing.
	env, err := s.sessionEnv(id, term, who)
	if err != nil {
		return nil, err
	}
	cred, err := who.credential()
	if err != nil {
		return nil, err
	}
	sess, err := s.mgr.Create(ptysession.Spec{
		ID:  id,
		Cmd: cmd,
		Env: env,
		// Their own home. Nothing presumes what a workspace is FOR, so there is
		// no project directory to start in -- /shared is where anything common
		// goes, and what lands there is the customer's business.
		Dir:        who.home,
		Rows:       rows,
		Cols:       cols,
		Credential: cred,
	})
	if err != nil {
		return nil, err
	}
	s.log.Info("session created",
		"session", sess.ID, "argv", cmd, "cols", cols, "rows", rows,
		"uid", who.uid, "home", who.home)
	return sess, nil
}

// identityFor resolves who a session belongs to, and builds their home the first
// time this task sees them.
//
// Two answers, decided by whether this process is root, because that is exactly
// what decides whether any of this is possible: only root can change a child's
// uid, create a passwd entry, or chown a home. Where it cannot, there is no
// boundary to fall short of -- the supervisor and the session are already the
// same person -- so the announced identity is informational and the process's
// own home and config directory stand. That is the local driver and the e2e
// suite, and it is the behaviour they had before any of this existed.
//
// Where the boundary IS real, an unknown session is refused rather than run at
// the shared uid. Running it there would put it in somebody else's home with
// read access to their conversations: the exact failure this exists to prevent,
// and silent.
func (s *Supervisor) identityFor(sessionID string) (identity, error) {
	if os.Geteuid() != 0 {
		// Home empty on purpose: it leaves Spec.Dir unset, so the child inherits
		// this process's working directory rather than being sent to a path
		// nothing has created. uid 0 likewise leaves childEnv's identity
		// variables alone.
		return identity{configDir: s.opt.ConfigDir}, nil
	}
	who, ok := s.ids.get(sessionID)
	if !ok {
		if s.opt.SessionUID == 0 {
			// Root, but the uid boundary is switched off entirely. Already warned
			// about at startup; nothing here can improve on that.
			return identity{configDir: s.opt.ConfigDir}, nil
		}
		return identity{}, fmt.Errorf(
			"no identity for session %s: the Management API sends one before any "+
				"attach, so this session cannot be started with a user", sessionID)
	}
	if s.ids.markProvisioned(who.uid) {
		if err := provisionHome(who); err != nil {
			return identity{}, fmt.Errorf("provisioning a home for uid %d: %w", who.uid, err)
		}
		s.log.Info("member home provisioned",
			"uid", who.uid, "user", who.username, "home", who.home)
	}
	return who, nil
}

// handleSessionIdentity records who a session runs as. Control tunnel only --
// serveAttachOnly refuses every verb but attach on the data tunnel, so a relay
// cannot reach this.
func (s *Supervisor) handleSessionIdentity(stream net.Conn, env tunnel.Envelope) {
	defer func() { _ = stream.Close() }()
	var req tunnel.SessionIdentity
	if err := env.Decode(&req); err != nil {
		replyError(stream, err.Error())
		return
	}
	if req.SessionID == "" || req.UID == 0 || req.Home == "" {
		replyError(stream, "a session identity needs a session, a uid and a home")
		return
	}
	s.ids.put(identity{
		uid:       req.UID,
		username:  req.Username,
		home:      req.Home,
		configDir: filepath.Join(req.Home, ".claude"),
	}, req.SessionID)
	_ = tunnel.WriteMsg(stream, tunnel.MsgOK, tunnel.SessionInfo{ID: req.SessionID})
}

func (s *Supervisor) handleStartSession(stream net.Conn, env tunnel.Envelope) {
	defer func() { _ = stream.Close() }()
	var req tunnel.StartSession
	if err := env.Decode(&req); err != nil {
		replyError(stream, err.Error())
		return
	}
	if req.SessionID == "" {
		replyError(stream, "missing session_id")
		return
	}
	// Idempotent: resuming a session that is already running is what a user gets
	// for double-clicking, and it must not be an error or a second process.
	if sess, ok := s.mgr.Get(req.SessionID); ok {
		rows, cols := sess.Size()
		_ = tunnel.WriteMsg(stream, tunnel.MsgOK, tunnel.SessionInfo{
			ID:        sess.ID,
			Rows:      rows,
			Cols:      cols,
			Attachers: sess.Attachers(),
			StartedAt: sess.StartedAt.UTC().Format(time.RFC3339),
		})
		return
	}
	sess, err := s.createSession(req.SessionID, req.Term, 0, 0)
	if err != nil {
		replyError(stream, err.Error())
		return
	}
	rows, cols := sess.Size()
	_ = tunnel.WriteMsg(stream, tunnel.MsgOK, tunnel.SessionInfo{
		ID:        sess.ID,
		Rows:      rows,
		Cols:      cols,
		StartedAt: sess.StartedAt.UTC().Format(time.RFC3339),
	})
}

// handleDeleteSession ends a session and drops its conversation.
//
// The stop is forced and waited on before the transcript goes: Claude Code
// flushes to the transcript as it runs, so unlinking underneath a live process
// races it into recreating the file, leaving a session that was reported deleted
// but resumes anyway.
func (s *Supervisor) handleDeleteSession(stream net.Conn, env tunnel.Envelope) {
	defer func() { _ = stream.Close() }()
	var req tunnel.DeleteSession
	if err := env.Decode(&req); err != nil {
		replyError(stream, err.Error())
		return
	}
	if sess, ok := s.mgr.Get(req.SessionID); ok {
		_ = sess.Stop(true)
		sess.Wait()
	}
	if err := removeTranscript(s.opt.ConfigDir, req.SessionID); err != nil {
		replyError(stream, err.Error())
		return
	}
	s.log.Info("session deleted; conversation dropped", "session", req.SessionID)
	_ = tunnel.WriteMsg(stream, tunnel.MsgOK, nil)
}

func (s *Supervisor) handleStopSession(stream net.Conn, env tunnel.Envelope) {
	defer func() { _ = stream.Close() }()
	var req tunnel.StopSession
	if err := env.Decode(&req); err != nil {
		replyError(stream, err.Error())
		return
	}
	if err := s.mgr.Stop(req.SessionID, req.Force); err != nil {
		replyError(stream, err.Error())
		return
	}
	_ = tunnel.WriteMsg(stream, tunnel.MsgOK, nil)
}

func (s *Supervisor) handleListSessions(stream net.Conn) {
	defer func() { _ = stream.Close() }()
	var list tunnel.SessionList
	for _, sess := range s.mgr.List() {
		rows, cols := sess.Size()
		list.Sessions = append(list.Sessions, tunnel.SessionInfo{
			ID:        sess.ID,
			Rows:      rows,
			Cols:      cols,
			Attachers: sess.Attachers(),
			StartedAt: sess.StartedAt.UTC().Format(time.RFC3339),
		})
	}
	_ = tunnel.WriteMsg(stream, tunnel.MsgOK, list)
}

// handleAttach binds a client to a session, creating the PTY if asked.
//
// The stream outlives neither more nor less than the attachment: when it
// closes, the client is detached and the child is deliberately left running.
// That decoupling is the point of the whole package (section 2.4).
func (s *Supervisor) handleAttach(stream net.Conn, env tunnel.Envelope) {
	defer func() { _ = stream.Close() }()

	var req tunnel.Attach
	if err := env.Decode(&req); err != nil {
		replyError(stream, err.Error())
		return
	}
	if req.SessionID == "" {
		replyError(stream, "missing session_id")
		return
	}

	sess, ok := s.mgr.Get(req.SessionID)
	created := false
	if !ok {
		if !req.Create {
			replyError(stream, "no such session: "+req.SessionID)
			return
		}
		var err error
		sess, err = s.createSession(req.SessionID, req.Term, req.Rows, req.Cols)
		if err != nil {
			replyError(stream, err.Error())
			return
		}
		created = true
	}

	// No repaint for the attach that created the PTY: the child has
	// nothing drawn yet, and nudging would race its first frame.
	att, err := sess.Attach(req.Mode == tunnel.ModeViewer, req.Rows, req.Cols, !created)
	if err != nil {
		replyError(stream, err.Error())
		return
	}
	// Kept for the early-return paths below; the main path detaches explicitly
	// before waiting on the output goroutine. Detach is idempotent.
	defer sess.Detach(att)

	if err := tunnel.WriteMsg(stream, tunnel.MsgOK, tunnel.AttachOK{
		SessionID: sess.ID,
		Created:   created,
	}); err != nil {
		return
	}
	s.log.Info("client attached",
		"session", sess.ID, "created", created,
		"viewer", att.ReadOnly(), "attachers", sess.Attachers())

	// PTY -> control plane.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			p, reason, ok := att.Next()
			if !ok {
				// The close reason is load-bearing: the relay turns
				// ReasonSessionEnded into a normal WebSocket close so the client
				// knows not to reconnect, and anything else into an abnormal one.
				if reason == ptysession.ReasonSessionEnded {
					_ = tunnel.WriteMsg(stream, tunnel.MsgOK, map[string]string{"event": "session_ended"})
				}
				_ = stream.Close()
				return
			}
			if err := tunnel.WriteFrame(stream, tunnel.FrameData, p); err != nil {
				return
			}
		}
	}()

	// control plane -> PTY, plus resize control.
	for {
		typ, payload, err := tunnel.ReadFrame(stream)
		if err != nil {
			break
		}
		switch typ {
		case tunnel.FrameData:
			if _, err := att.Write(payload); err != nil {
				s.log.Error("writing to the pty", "session", sess.ID, "error", err)
			}
		case tunnel.FrameControl:
			var env tunnel.Envelope
			if err := json.Unmarshal(payload, &env); err != nil {
				continue
			}
			if env.Type != tunnel.MsgResize {
				continue
			}
			var m tunnel.Resize
			if err := env.Decode(&m); err != nil {
				continue
			}
			if err := att.Resize(m.Rows, m.Cols); err != nil {
				s.log.Error("resizing the pty", "session", sess.ID, "error", err)
			}
		}
	}

	// Order matters: detach BEFORE waiting for the output goroutine.
	//
	// That goroutine is parked in att.Next(), which only returns once the
	// attachment closes -- and the attachment closes in Detach. Waiting first
	// deadlocks the pair until the session happens to produce output, so a client
	// detaching from an idle session stayed registered indefinitely: the session
	// looked occupied, and the goroutine leaked.
	sess.Detach(att)
	<-done
	s.log.Info("client detached", "session", sess.ID, "attachers", sess.Attachers())
}

func replyError(stream net.Conn, msg string) {
	_ = tunnel.WriteMsg(stream, tunnel.MsgError, tunnel.Error{Message: msg})
}

// maxDirEntries caps one listing. The tunnel refuses a frame over 1 MiB
// (tunnel/frame.go) and shares the connection with live PTY traffic, so an
// unbounded listing would not merely be slow -- it would fail, and take the
// terminal's responsiveness with it on the way.
const maxDirEntries = 1000

func (s *Supervisor) handleListDir(stream net.Conn, env tunnel.Envelope) {
	defer func() { _ = stream.Close() }()
	var req tunnel.ListDir
	if err := env.Decode(&req); err != nil {
		replyError(stream, "bad list_dir request: "+err.Error())
		return
	}

	abs, err := resolveInRoot(s.opt.WorkspaceRoot, req.Path)
	if err != nil {
		replyError(stream, err.Error())
		return
	}
	info, err := os.Stat(abs)
	if err != nil {
		replyError(stream, err.Error())
		return
	}
	if !info.IsDir() {
		replyError(stream, errNotADir.Error())
		return
	}

	entries, err := os.ReadDir(abs)
	if err != nil {
		replyError(stream, err.Error())
		return
	}

	limit := req.Limit
	if limit <= 0 || limit > maxDirEntries {
		limit = maxDirEntries
	}
	out := tunnel.DirListing{Path: displayPath(s.opt.WorkspaceRoot, abs), Total: len(entries)}
	for i := req.Offset; i < len(entries) && len(out.Entries) < limit; i++ {
		e := entries[i]
		// Lstat, not Stat: a symlink should report itself rather than its
		// target, and a dangling one must not drop the whole listing.
		fi, err := os.Lstat(filepath.Join(abs, e.Name()))
		if err != nil {
			continue
		}
		out.Entries = append(out.Entries, tunnel.DirEntry{
			Name:      e.Name(),
			IsDir:     fi.IsDir(),
			Size:      fi.Size(),
			Mode:      fi.Mode().Perm().String(),
			ModTime:   fi.ModTime().UTC().Format(time.RFC3339),
			IsSymlink: fi.Mode()&os.ModeSymlink != 0,
		})
	}
	out.Truncated = req.Offset+len(out.Entries) < out.Total
	_ = tunnel.WriteMsg(stream, tunnel.MsgOK, out)
}

func (s *Supervisor) handleListProcesses(stream net.Conn) {
	defer func() { _ = stream.Close() }()

	pids := make(map[string]int)
	for _, sess := range s.mgr.List() {
		if pid := sess.Pid(); pid > 0 {
			pids[sess.ID] = pid
		}
	}
	procs, ok := readProcesses(pids)

	out := tunnel.ProcessList{Available: ok}
	for _, p := range procs {
		out.Processes = append(out.Processes, tunnel.ProcessInfo(p))
	}
	_ = tunnel.WriteMsg(stream, tunnel.MsgOK, out)
}

func (s *Supervisor) handleResources(stream net.Conn) {
	defer func() { _ = stream.Close() }()
	_ = tunnel.WriteMsg(stream, tunnel.MsgOK, s.samples.usage())
}

// workspaceMount is where the workspace's durable storage is attached, and
// therefore what "in the workspace" means.
//
// EVERYTHING that has to outlive a task sits under it: the member homes, their
// config directories, and the shared directory. That is not tidiness -- on
// Fargate this is an EBS volume, which ECS creates per task and deletes when the
// task exits, so carrying a workspace forward means snapshotting this mount and
// launching the replacement from the snapshot. A directory outside it has a
// different durability, and the difference is invisible until the first
// replacement.
const workspaceMount = "/workspace"

// defaultWorkspaceRoot matches image/entrypoint.sh, which honours
// LEMUL_PROJECT_DIR and otherwise uses the workspace mount.
func defaultWorkspaceRoot() string {
	if v := os.Getenv("LEMUL_PROJECT_DIR"); v != "" {
		return v
	}
	return workspaceMount
}

// sessionEnv builds the environment for one session's Claude Code process.
//
// In gateway mode it opens that session's loopback proxy and points the child at
// it, so the child holds only a placeholder token.
func (s *Supervisor) sessionEnv(sessionID, term string, who identity) ([]string, error) {
	env := childEnv(term, who)
	if s.broker == nil {
		return env, nil
	}
	sp, err := s.broker.Open(sessionID)
	if err != nil {
		return nil, fmt.Errorf("open gateway proxy for session %s: %w", sessionID, err)
	}
	s.log.Info("session model traffic brokered", "session", sessionID, "proxy", sp.BaseURL)
	return append(env,
		"ANTHROPIC_BASE_URL="+sp.BaseURL,
		"ANTHROPIC_AUTH_TOKEN="+gateway.PlaceholderToken,
	), nil
}

// strippedFromChild are variables that must never reach a session.
//
// This is not defensive tidiness. Claude Code's Bash tool is a child process and
// inherits the environment wholesale -- verified against 2.1.220, where
// AWS_ACCESS_KEY_ID, AWS_SESSION_TOKEN, AWS_CONTAINER_CREDENTIALS_RELATIVE_URI
// and ANTHROPIC_AUTH_TOKEN all reached a Bash command intact. Anything left here
// is a credential handed to every command the agent runs, including commands the
// model wrote.
var strippedFromChild = []string{
	// The gateway credential the supervisor brokers on the session's behalf.
	"LEMUL_GATEWAY_KEY=",
	// The tunnel credential: it would let a session register as this workspace.
	"LEMUL_TOKEN=",
	// Set per session from the broker; a stale inherited value would point the
	// child at another session's proxy and misattribute its spend.
	"ANTHROPIC_BASE_URL=",
	"ANTHROPIC_AUTH_TOKEN=",
	"ANTHROPIC_API_KEY=",
}

// childEnv builds the base environment for a Claude Code process.
//
// TERM must name a terminfo entry that resolves inside the image (hence
// ncurses-term), and COLORTERM is what unlocks 24-bit colour. Without either,
// the transport is clean but the rendering is not (section 4.1).
//
// The identity variables are rewritten from `who` rather than inherited.
// Inheriting them is wrong the moment a session stops sharing the supervisor's
// uid: the container hands root HOME=/root, mode 0700, so the session would run
// with a home it cannot write. Claude Code is a Node process and npm reaches for
// HOME whatever CLAUDE_CONFIG_DIR says, so this fails in ways that do not name
// the cause.
//
// CLAUDE_CONFIG_DIR is per member too, and it is the single variable that moves
// ALL of a member's Claude Code state at once -- skills, plugins, MCP servers,
// output style and the conversation transcripts -- into a directory only they
// can read.
func childEnv(term string, who identity) []string {
	drops := strippedFromChild
	if who.uid != 0 {
		drops = append(append([]string{}, drops...),
			"HOME=", "USER=", "LOGNAME=", "CLAUDE_CONFIG_DIR=")
	}
	env := make([]string, 0, len(os.Environ())+5)
outer:
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "TERM=") || strings.HasPrefix(e, "COLORTERM=") {
			continue
		}
		for _, drop := range drops {
			if strings.HasPrefix(e, drop) {
				continue outer
			}
		}
		env = append(env, e)
	}
	env = append(env, "TERM="+term, "COLORTERM=truecolor")
	if who.uid != 0 {
		home, name := who.home, who.username
		if home == "" || name == "" {
			// No identity was pushed for this session, which is the
			// single-uid fallback. Resolve what the passwd file knows rather
			// than handing over the supervisor's own.
			h, n := sessionHome(who.uid)
			if home == "" {
				home = h
			}
			if name == "" {
				name = n
			}
		}
		env = append(env, "HOME="+home, "USER="+name, "LOGNAME="+name)
		if who.configDir != "" {
			env = append(env, "CLAUDE_CONFIG_DIR="+who.configDir)
		}
	}
	return env
}

// sessionHome resolves the session uid's home and login name, falling back to
// values that at least exist rather than to the supervisor's.
func sessionHome(uid uint32) (home, name string) {
	home, name = "/tmp", strconv.FormatUint(uint64(uid), 10)
	u, err := user.LookupId(name)
	if err != nil {
		return home, name
	}
	if u.HomeDir != "" {
		home = u.HomeDir
	}
	if u.Username != "" {
		name = u.Username
	}
	return home, name
}

// redactURL keeps credentials out of logs if an upstream is ever given as a
// URL with userinfo.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparseable)"
	}
	u.User = nil
	return u.String()
}
