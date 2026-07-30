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
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/lufzle/lemul-cc/internal/agent"
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
	Term       string
	RingBytes  int
	NudgeDelay time.Duration
	// HeadroomInterval is how often resource headroom is reported upward.
	HeadroomInterval time.Duration
}

// Supervisor implements agent.Handler.
type Supervisor struct {
	opt Options
	mgr *ptysession.Manager

	mu     sync.Mutex
	events net.Conn // nil while disconnected
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
	s := &Supervisor{opt: o}
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
	case tunnel.MsgStopSession:
		s.handleStopSession(stream, env)
	case tunnel.MsgListSessions:
		s.handleListSessions(stream)
	default:
		replyError(stream, "unknown message type "+env.Type)
		_ = stream.Close()
	}
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
		cmd := req.Cmd
		if len(cmd) == 0 {
			cmd = s.opt.DefaultCmd
		}
		term := req.Term
		if term == "" {
			term = s.opt.Term
		}
		var err error
		sess, err = s.mgr.Create(ptysession.Spec{
			ID:   req.SessionID,
			Cmd:  cmd,
			Env:  childEnv(term),
			Rows: req.Rows,
			Cols: req.Cols,
		})
		if err != nil {
			replyError(stream, err.Error())
			return
		}
		created = true
		log.Printf("session %s created: %v size=%dx%d", sess.ID, cmd, req.Cols, req.Rows)
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

// childEnv builds the environment for a Claude Code process.
//
// TERM must name a terminfo entry that resolves inside the image (hence
// ncurses-term), and COLORTERM is what unlocks 24-bit colour. Without either,
// the transport is clean but the rendering is not (section 4.1).
func childEnv(term string) []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "TERM=") || strings.HasPrefix(e, "COLORTERM=") {
			continue
		}
		env = append(env, e)
	}
	return append(env, "TERM="+term, "COLORTERM=truecolor")
}
