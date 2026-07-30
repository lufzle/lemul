// Package ptysession owns the PTYs inside one workspace task.
//
// This is where the runner/supervisor split actually bites. In the Phase 0
// prototype a PTY lived and died with the WebSocket that carried it; here the
// two lifetimes are independent, which is what section 2.4 calls the difference
// between the "session process" axis and the "client presence" axis:
//
//	client disconnects  -> the child keeps running. That IS detach.
//	child exits         -> attachers are closed with a clean reason.
//
// A session fans its output out to zero or more attachers and keeps filling its
// replay ring when nobody is watching, so an overnight agent run survives a
// closed laptop lid.
package ptysession

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

const (
	defaultRingBytes  = 256 << 10
	defaultQueueBytes = 4 << 20
	defaultNudgeDelay = 75 * time.Millisecond
	readBufBytes      = 32 << 10
)

var (
	ErrSessionExited = errors.New("session has exited")
	ErrExists        = errors.New("session already exists")
	ErrNotFound      = errors.New("session not found")
)

// Spec describes a session to create.
type Spec struct {
	ID   string
	Cmd  []string
	Env  []string
	Rows uint16
	Cols uint16
}

// Session is one Claude Code process on one PTY.
type Session struct {
	ID        string
	StartedAt time.Time

	cmd  *exec.Cmd
	ptmx *os.File

	nudgeDelay time.Duration
	nudgeMu    sync.Mutex // serialises concurrent attachers' nudges

	mu          sync.Mutex
	ring        *ring
	modes       *ModeTracker
	attachments map[*Attachment]struct{}
	rows, cols  uint16
	exited      bool
	exitCode    int

	done chan struct{}
}

// newSession forks the child. The caller supplies a fully-resolved size: a PTY
// defaults to 0x0, and a resize arriving after the fork is too late -- Claude
// Code has already drawn its first frame into a zero-size terminal. Size
// therefore travels in the attach request and is applied by StartWithSize,
// atomically with the fork (section 4.1).
func newSession(spec Spec, ringBytes int, nudgeDelay time.Duration) (*Session, error) {
	if len(spec.Cmd) == 0 {
		return nil, errors.New("empty command")
	}
	if spec.Rows == 0 {
		spec.Rows = 24
	}
	if spec.Cols == 0 {
		spec.Cols = 80
	}

	c := exec.Command(spec.Cmd[0], spec.Cmd[1:]...)
	c.Env = spec.Env

	// StartWithSize allocates a real PTY via forkpty. With pipes instead,
	// isatty() returns false and Claude Code degrades to non-interactive mode:
	// no TUI, no colour, line-buffered. Binary failure mode.
	ptmx, err := pty.StartWithSize(c, &pty.Winsize{Rows: spec.Rows, Cols: spec.Cols})
	if err != nil {
		return nil, fmt.Errorf("pty.StartWithSize: %w", err)
	}

	s := &Session{
		ID:          spec.ID,
		StartedAt:   time.Now(),
		cmd:         c,
		ptmx:        ptmx,
		nudgeDelay:  nudgeDelay,
		ring:        newRing(ringBytes),
		modes:       NewModeTracker(),
		attachments: make(map[*Attachment]struct{}),
		rows:        spec.Rows,
		cols:        spec.Cols,
		done:        make(chan struct{}),
	}
	return s, nil
}

// readLoop is the single reader of the PTY. It runs for the life of the child,
// with or without an attached client -- that is what keeps an unattended agent
// run alive and its output in the ring.
func (s *Session) readLoop(onExit func(id string, code int)) {
	buf := make([]byte, readBufBytes)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			s.mu.Lock()
			// Record the boundary state BEFORE consuming the chunk: it says
			// whether a replay may begin at this chunk.
			clean := s.modes.AtBoundary()
			s.modes.Feed(buf[:n])
			s.ring.push(buf[:n], clean)
			for a := range s.attachments {
				a.push(buf[:n])
			}
			s.mu.Unlock()
		}
		if err != nil {
			break
		}
	}
	s.finish(onExit)
}

func (s *Session) finish(onExit func(id string, code int)) {
	code := 0
	if err := s.cmd.Wait(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	s.mu.Lock()
	s.exited = true
	s.exitCode = code
	// Closed under the lock, together with the exited flag, because
	// pty.Setsize reaches the fd through File.Fd(), which is not safe against a
	// concurrent Close the way Read and Write are. A reattach nudge racing a
	// child that is exiting hits exactly this.
	_ = s.ptmx.Close()
	for a := range s.attachments {
		a.close(ReasonSessionEnded)
	}
	s.attachments = make(map[*Attachment]struct{})
	s.mu.Unlock()

	close(s.done)
	if onExit != nil {
		onExit(s.ID, code)
	}
}

// Attach registers a new client. readOnly is the viewer mode from section 2.5:
// both attachers write the same PTY stdin, so a viewer that can write is
// silently a co-driver. The relay enforces this too; this is the last line of
// defence.
//
// repaint asks for the SIGWINCH nudge. It must be false for the attach that
// created the session: the child has not drawn anything yet, so there is
// nothing to repaint, and nudging races its first frame -- the child reads the
// shrunken width and renders one column narrow. That is the same class of bug
// as starting a PTY at 0x0, and just as easy to misattribute to the TUI.
//
// The returned Attachment is pre-seeded, under the same lock the reader uses,
// with the mode prelude and then the replay ring -- taking the snapshot inside
// the lock is what prevents a gap or a duplicate against concurrent live output.
func (s *Session) Attach(readOnly bool, rows, cols uint16, repaint bool) (*Attachment, error) {
	s.mu.Lock()
	if s.exited {
		s.mu.Unlock()
		return nil, ErrSessionExited
	}
	a := newAttachment(s, readOnly, defaultQueueBytes)
	if prelude := s.modes.Prelude(); len(prelude) > 0 {
		a.push(prelude)
	}
	if snap := s.ring.snapshot(); len(snap) > 0 {
		a.push(snap)
	}
	s.attachments[a] = struct{}{}
	s.mu.Unlock()

	if repaint {
		if readOnly {
			// A viewer never changes the size. The PTY has exactly one, so
			// honouring a viewer's terminal would reflow the CONTROLLER's Claude
			// Code -- the same violation as letting a viewer type, just quieter
			// (section 2.5). Passing zero makes nudge use the current size, which
			// takes the cols-1 path: a repaint for the new viewer, no resize for
			// anyone. It matters most for the browser viewer, whose size is
			// whatever the window happens to be.
			go s.nudge(0, 0)
		} else {
			go s.nudge(rows, cols)
		}
	}
	return a, nil
}

// Detach removes an attacher. The child is deliberately left running.
func (s *Session) Detach(a *Attachment) {
	s.mu.Lock()
	delete(s.attachments, a)
	s.mu.Unlock()
	a.close(ReasonDetached)
}

// nudge makes a running child repaint by delivering SIGWINCH.
//
// Measured against Claude Code 2.1.220 (winch-probe/): the repaint homes the
// cursor and erases each line (ESC[2K) before rewriting it, reconstructing the
// entire visible viewport even onto a dirty or blank screen. That is what makes
// detach/reattach work in Phase 1 without a VT state model (decision #4).
//
// A reattaching client whose terminal is a different size gets its repaint from
// the resize itself; only when the size is unchanged is the cols-1 trick needed,
// because a no-op resize delivers no SIGWINCH. Preferring the plain resize also
// spares the user a second reflow they would see as a flicker.
//
// The two ioctls of the trick need a gap between them: delivered back-to-back,
// the child can coalesce them, read the final size, see no change and skip the
// repaint.
//
// Caveat for multi-attach: a nudge is a property of the PTY, not of one client,
// so it briefly reflows the screen for everyone already attached. The Phase 2
// VT model removes the need for the trick entirely.
func (s *Session) nudge(rows, cols uint16) {
	s.nudgeMu.Lock()
	defer s.nudgeMu.Unlock()

	curRows, curCols := s.Size()
	if rows == 0 || cols == 0 {
		rows, cols = curRows, curCols
	}
	if rows != curRows || cols != curCols {
		_ = s.Resize(rows, cols)
		return
	}
	if cols < 2 {
		return // nothing to shrink into
	}
	_ = s.Resize(rows, cols-1)
	time.Sleep(s.nudgeDelay)
	_ = s.Resize(rows, cols)
}

// Resize applies the size and delivers SIGWINCH to the child.
//
// The ioctl runs under the session lock so it cannot race finish() closing the
// PTY. That is safe to hold here because Setsize does not block; Write
// deliberately does not hold it, since a child that has stopped reading its
// stdin would block the write and with it the whole fan-out.
func (s *Session) Resize(rows, cols uint16) error {
	if rows == 0 || cols == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exited {
		return ErrSessionExited
	}
	s.rows, s.cols = rows, cols
	return pty.Setsize(s.ptmx, &pty.Winsize{Rows: rows, Cols: cols})
}

func (s *Session) Size() (rows, cols uint16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rows, s.cols
}

func (s *Session) Attachers() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.attachments)
}

func (s *Session) Exited() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exited
}

// write sends bytes to the child's stdin. Only non-viewer attachers reach here.
//
// Deliberately not holding the session lock across the write: a child that has
// stopped reading stdin blocks it, and the PTY reader needs that lock to fan
// output out to every attacher. os.File.Write is refcount-safe against a
// concurrent Close, so a session ending mid-write surfaces as an error rather
// than a race.
func (s *Session) write(p []byte) (int, error) {
	s.mu.Lock()
	exited := s.exited
	s.mu.Unlock()
	if exited {
		return 0, ErrSessionExited
	}
	return s.ptmx.Write(p)
}

// Stop applies Ctrl-C/Ctrl-D semantics (section 2.4): it interrupts the agent
// the way quitting local Claude Code does. The conversation lives on the
// workspace filesystem, so `claude --resume` picks it up afterwards.
func (s *Session) Stop(force bool) error {
	if s.cmd.Process == nil {
		return nil
	}
	sig := syscall.SIGHUP
	if force {
		sig = syscall.SIGKILL
	}
	return s.cmd.Process.Signal(sig)
}

// Wait blocks until the child has exited and attachers have been closed.
func (s *Session) Wait() { <-s.done }
