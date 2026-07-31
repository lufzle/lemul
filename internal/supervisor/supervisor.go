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
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lufzle/lemul-cc/internal/agent"
	"github.com/lufzle/lemul-cc/internal/bedrock"
	"github.com/lufzle/lemul-cc/internal/gateway"
	"github.com/lufzle/lemul-cc/internal/ptysession"
	"github.com/lufzle/lemul-cc/internal/tunnel"
)

type Options struct {
	ControlPlane string
	TenantID     string
	WorkspaceID  string
	Token        string
	// DefaultCmd runs when an attach asks to create a session without naming one.
	DefaultCmd []string
	// ConfigDir is Claude Code's CLAUDE_CONFIG_DIR, where conversations are
	// filed. Empty resolves it the way Claude Code does. It has to sit on the
	// workspace volume -- that is what lets a conversation outlive its process
	// and makes resume mean anything (section 2.4).
	ConfigDir string
	Term      string
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

	samples  *sampler
	stopOnce sync.Once
	stopped  chan struct{}
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
	s := &Supervisor{opt: o, stopped: make(chan struct{})}
	// Sampling starts with the process, not with the first request: CPU and
	// network are rates, so the first caller would otherwise get a zero and a
	// wrong one at that.
	s.samples = newSampler(o.WorkspaceRoot)
	go s.samples.run(s.stopped)
	if o.GatewayURL != "" {
		b, err := gateway.New(gateway.Options{
			Upstream:    o.GatewayURL,
			APIKey:      o.GatewayKey,
			WorkspaceID: o.WorkspaceID,
			UserID:      o.UserID,
		})
		if err != nil {
			log.Fatalf("gateway: %v", err)
		}
		s.broker = b
		log.Printf("gateway mode: brokering model traffic to %s", redactURL(o.GatewayURL))
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

// Run dials out and serves until ctx is cancelled.
func (s *Supervisor) Run(ctx context.Context) error {
	cfg := agent.Config{
		URL:   strings.TrimRight(s.opt.ControlPlane, "/") + "/v1/tunnel/workspace",
		Token: s.opt.Token,
		Params: map[string]string{
			"tenant":    s.opt.TenantID,
			"workspace": s.opt.WorkspaceID,
		},
	}
	err := agent.Run(ctx, cfg, s)
	// Say why the task is going away. A workspace that disappears mid-run is
	// alarming, and "the control plane no longer recognises this task" is a
	// different problem from a crash -- it means a replacement placement is what
	// fixes it, not an investigation of this one.
	if errors.Is(err, agent.ErrUnauthorized) {
		log.Printf("this task is orphaned: %v", err)
		log.Printf("exiting so it can be reclaimed; %d session(s) end with it", s.mgr.Count())
	}
	s.mgr.StopAll(true)
	return err
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
	log.Printf("tunnel down (%v); %d session(s) still running", err, s.mgr.Count())
}

func (s *Supervisor) sendEvent(typ string, v any) {
	s.mu.Lock()
	ev := s.events
	s.mu.Unlock()
	if ev == nil {
		return
	}
	if err := tunnel.WriteMsg(ev, typ, v); err != nil {
		log.Printf("event %s: %v", typ, err)
	}
}

func (s *Supervisor) onSessionExit(id string, code int) {
	log.Printf("session %s exited (code %d)", id, code)
	if s.broker != nil {
		s.broker.Close(id)
	}
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
		})
	}
}

// OnStream dispatches one command stream from the control plane.
func (s *Supervisor) OnStream(stream net.Conn) {
	env, err := tunnel.ReadMsg(stream)
	if err != nil {
		log.Printf("stream: %v", err)
		_ = stream.Close()
		return
	}
	switch env.Type {
	case tunnel.MsgAttach:
		s.handleAttach(stream, env)
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

// createSession forks one session's process. Shared by attach-with-Create and by
// the start_session verb, so a session resumed from a console and one resumed by
// connecting a terminal get identical treatment -- including the argv.
func (s *Supervisor) createSession(id string, cmd []string, term string, rows, cols uint16) (*ptysession.Session, error) {
	if len(cmd) == 0 {
		cmd = s.opt.DefaultCmd
	}
	if term == "" {
		term = s.opt.Term
	}
	// Resume decided here, from the disk this process is standing on, rather than
	// from anything the control plane remembers (see claudeargs.go).
	cmd = sessionArgs(cmd, id, s.opt.ConfigDir)

	// The proxy has to exist before the fork: its URL goes into the child's
	// environment, and a child that started without it would talk to nothing.
	env, err := s.sessionEnv(id, term)
	if err != nil {
		return nil, err
	}
	sess, err := s.mgr.Create(ptysession.Spec{
		ID:   id,
		Cmd:  cmd,
		Env:  env,
		Rows: rows,
		Cols: cols,
	})
	if err != nil {
		return nil, err
	}
	log.Printf("session %s created: %v size=%dx%d", sess.ID, cmd, cols, rows)
	return sess, nil
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
	sess, err := s.createSession(req.SessionID, req.Cmd, req.Term, 0, 0)
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
	log.Printf("session %s deleted (process stopped, conversation dropped)", req.SessionID)
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
		sess, err = s.createSession(req.SessionID, req.Cmd, req.Term, req.Rows, req.Cols)
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
	log.Printf("session %s attached (created=%v viewer=%v attachers=%d)",
		sess.ID, created, att.ReadOnly(), sess.Attachers())

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
				log.Printf("session %s: write: %v", sess.ID, err)
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
				log.Printf("session %s: resize: %v", sess.ID, err)
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
	log.Printf("session %s detached (attachers now %d)", sess.ID, sess.Attachers())
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

// defaultWorkspaceRoot matches image/entrypoint.sh, which honours
// LEMUL_PROJECT_DIR and otherwise uses /workspace.
func defaultWorkspaceRoot() string {
	if v := os.Getenv("LEMUL_PROJECT_DIR"); v != "" {
		return v
	}
	return "/workspace"
}

// sessionEnv builds the environment for one session's Claude Code process.
//
// In gateway mode it opens that session's loopback proxy and points the child at
// it, so the child holds only a placeholder token.
func (s *Supervisor) sessionEnv(sessionID, term string) ([]string, error) {
	env := childEnv(term)
	if s.broker == nil {
		return env, nil
	}
	sp, err := s.broker.Open(sessionID)
	if err != nil {
		return nil, fmt.Errorf("open gateway proxy for session %s: %w", sessionID, err)
	}
	log.Printf("session %s: model traffic brokered via %s", sessionID, sp.BaseURL)
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
func childEnv(term string) []string {
	env := make([]string, 0, len(os.Environ())+2)
outer:
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "TERM=") || strings.HasPrefix(e, "COLORTERM=") {
			continue
		}
		for _, drop := range strippedFromChild {
			if strings.HasPrefix(e, drop) {
				continue outer
			}
		}
		env = append(env, e)
	}
	return append(env, "TERM="+term, "COLORTERM=truecolor")
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
