package ptysession

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	return NewManager(Options{RingBytes: 64 << 10, NudgeDelay: 20 * time.Millisecond})
}

func startShell(t *testing.T, m *Manager, id, script string, rows, cols uint16) *Session {
	t.Helper()
	s, err := m.Create(Spec{
		ID:   id,
		Cmd:  []string{"sh", "-c", script},
		Env:  append(os.Environ(), "TERM=xterm-256color", "COLORTERM=truecolor"),
		Rows: rows,
		Cols: cols,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop(true) })
	return s
}

// drain reads from an attachment until want appears or the deadline passes.
func drain(t *testing.T, a *Attachment, want string, d time.Duration) string {
	t.Helper()
	var acc bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			p, _, ok := a.Next()
			if !ok {
				return
			}
			acc.Write(p)
			if want != "" && strings.Contains(acc.String(), want) {
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(d):
	}
	return acc.String()
}

// The defining property of the split: a client going away must NOT kill the
// child. Conflating client presence with process lifetime is what would reap an
// overnight agent run when someone closes a laptop lid (section 2.4).
func TestDetachLeavesChildRunning(t *testing.T) {
	m := newTestManager(t)
	s := startShell(t, m, "s1", `stty raw -echo; echo READY; while :; do sleep 0.05; done`, 24, 80)

	a, err := s.Attach(false, 24, 80, true)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if got := drain(t, a, "READY", 5*time.Second); !strings.Contains(got, "READY") {
		t.Fatalf("child never started; got %q", got)
	}

	s.Detach(a)
	time.Sleep(300 * time.Millisecond)

	if s.Exited() {
		t.Fatal("child was killed by detach")
	}
	if _, ok := m.Get("s1"); !ok {
		t.Fatal("session removed from the manager on detach")
	}
	if n := s.Attachers(); n != 0 {
		t.Errorf("attachers = %d after detach, want 0", n)
	}
}

// Reattaching must give the new client the mode prelude, or it gets a correct
// screen with broken Shift+Enter and broken paste (winch-probe/ finding).
func TestReattachReplaysModePrelude(t *testing.T) {
	m := newTestManager(t)
	// Emit exactly the negotiation Claude Code performs at startup.
	s := startShell(t, m,
		"s1",
		`stty raw -echo; printf '\033[?2004h\033[>4;2m\033[>1u'; echo READY; while :; do sleep 0.05; done`,
		24, 80)

	first, err := s.Attach(false, 24, 80, true)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	drain(t, first, "READY", 5*time.Second)
	s.Detach(first)

	second, err := s.Attach(false, 24, 80, true)
	if err != nil {
		t.Fatalf("reattach: %v", err)
	}
	defer s.Detach(second)

	got := drain(t, second, "\x1b[>1u", 5*time.Second)
	for _, want := range []string{"\x1b[?2004h", "\x1b[>4;2m", "\x1b[>1u"} {
		if !strings.Contains(got, want) {
			t.Errorf("reattach did not replay %q\n got: %q", want, got)
		}
	}
}

// The nudge is what makes the reattached screen correct without a VT model: it
// must deliver SIGWINCH and settle back at the client's real size.
//
// The delay between the two ioctls is deliberately generous here. In production
// it only has to be long enough that the child does not coalesce the two
// signals, read the final size and skip the repaint; in a test it also has to
// be long enough for a shell trap to run `stty` and for that output to be
// observed, which is far slower.
func TestAttachNudgeDeliversSIGWINCH(t *testing.T) {
	m := NewManager(Options{RingBytes: 64 << 10, NudgeDelay: 400 * time.Millisecond})
	s, err := m.Create(Spec{
		ID:   "s1",
		Cmd:  []string{"sh", "-c", `stty raw -echo; trap 'stty size' WINCH; echo READY; while :; do sleep 0.05; done`},
		Env:  os.Environ(),
		Rows: 40, Cols: 100,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop(true) })

	// Take the first attach purely to synchronise: it proves the trap is
	// installed, so the nudge we actually assert on cannot race the child's
	// startup.
	warmup, err := s.Attach(false, 40, 100, true)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	drain(t, warmup, "READY", 5*time.Second)
	s.Detach(warmup)

	a, err := s.Attach(false, 40, 100, true)
	if err != nil {
		t.Fatalf("reattach: %v", err)
	}
	defer s.Detach(a)

	got := drain(t, a, "40 100", 8*time.Second)
	if !strings.Contains(got, "40 99") {
		t.Errorf("nudge did not shrink the terminal; got %q", got)
	}
	if !strings.Contains(got, "40 100") {
		t.Errorf("nudge did not restore the size; got %q", got)
	}
	rows, cols := s.Size()
	if rows != 40 || cols != 100 {
		t.Errorf("session size settled at %dx%d, want 40x100", cols, rows)
	}
}

// A viewer must not be able to drive the session. Both attachers write the same
// PTY stdin, so a viewer that can write is silently a co-driver (section 2.5).
func TestViewerInputIsDropped(t *testing.T) {
	m := newTestManager(t)
	s := startShell(t, m, "s1", `stty raw -echo; echo READY; exec cat`, 24, 80)

	ctrl, err := s.Attach(false, 24, 80, true)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer s.Detach(ctrl)
	drain(t, ctrl, "READY", 5*time.Second)

	viewer, err := s.Attach(true, 24, 80, true)
	if err != nil {
		t.Fatalf("attach viewer: %v", err)
	}
	defer s.Detach(viewer)

	if _, err := viewer.Write([]byte("FROM_VIEWER\n")); err != nil {
		t.Fatalf("viewer write: %v", err)
	}
	if _, err := ctrl.Write([]byte("FROM_CONTROL\n")); err != nil {
		t.Fatalf("control write: %v", err)
	}

	got := drain(t, ctrl, "FROM_CONTROL", 5*time.Second)
	if strings.Contains(got, "FROM_VIEWER") {
		t.Errorf("viewer input reached the PTY: %q", got)
	}
	if !strings.Contains(got, "FROM_CONTROL") {
		t.Errorf("control input did not reach the PTY: %q", got)
	}
}

// A viewer resizing the shared PTY would reflow everyone else's screen.
func TestViewerCannotResize(t *testing.T) {
	m := newTestManager(t)
	s := startShell(t, m, "s1", `stty raw -echo; echo READY; exec cat`, 24, 80)

	ctrl, _ := s.Attach(false, 24, 80, true)
	defer s.Detach(ctrl)
	drain(t, ctrl, "READY", 5*time.Second)

	viewer, _ := s.Attach(true, 24, 80, true)
	defer s.Detach(viewer)
	if err := viewer.Resize(50, 200); err != nil {
		t.Fatalf("viewer resize returned an error: %v", err)
	}
	rows, cols := s.Size()
	if rows == 50 || cols == 200 {
		t.Errorf("viewer resized the shared PTY to %dx%d", cols, rows)
	}
}

// The PTY reader must never block on a slow client, or a stalled attacher
// stalls Claude Code's stdout and with it the agent. Dropping the attachment is
// recoverable; dropping bytes corrupts a terminal silently.
func TestSlowAttacherIsDroppedNotThrottled(t *testing.T) {
	m := NewManager(Options{RingBytes: 4 << 10, NudgeDelay: 10 * time.Millisecond})
	s, err := m.Create(Spec{
		ID:   "s1",
		Cmd:  []string{"sh", "-c", `stty raw -echo; i=0; while [ $i -lt 20000 ]; do echo "line-$i"; i=$((i+1)); done; echo DONE; sleep 5`},
		Env:  os.Environ(),
		Rows: 24, Cols: 80,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop(true) })

	// A tiny queue that is never drained stands in for a client that cannot
	// keep up.
	slow := newAttachment(s, false, 8<<10)
	s.mu.Lock()
	s.attachments[slow] = struct{}{}
	s.mu.Unlock()

	// A healthy attacher alongside it: the point is not merely that the slow one
	// is dropped, but that the session keeps running at full speed regardless.
	fast, err := s.Attach(false, 24, 80, true)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer s.Detach(fast)

	if got := drain(t, fast, "DONE", 30*time.Second); !strings.Contains(got, "DONE") {
		t.Fatal("the healthy attacher stalled: a slow client is blocking the PTY reader")
	}

	_, reason, ok := slow.Next()
	if ok {
		t.Fatal("slow attacher still queueing output; it should have been dropped")
	}
	if reason != ReasonOverflow {
		t.Fatalf("closed with reason %v, want ReasonOverflow", reason)
	}
}

// The close reason is what the relay turns into a WebSocket close code, which
// is how the client decides whether to reconnect.
func TestChildExitClosesAttachersCleanly(t *testing.T) {
	m := newTestManager(t)
	s := startShell(t, m, "s1", `stty raw -echo; echo READY; exit 0`, 24, 80)

	a, err := s.Attach(false, 24, 80, true)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}

	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("attachment was never closed after the child exited")
		default:
		}
		_, reason, ok := a.Next()
		if !ok {
			if reason != ReasonSessionEnded {
				t.Fatalf("closed with reason %v, want ReasonSessionEnded", reason)
			}
			// Reaping happens after the attachers are closed, so this has to be
			// waited for rather than asserted: the attachment closing is not a
			// promise that the manager has already dropped the session.
			until := time.After(5 * time.Second)
			for {
				if _, still := m.Get("s1"); !still {
					return
				}
				select {
				case <-until:
					t.Fatal("exited session was never removed from the manager")
				case <-time.After(10 * time.Millisecond):
				}
			}
		}
	}
}

// A duplicate create would orphan a running Claude Code process.
func TestCreateRejectsDuplicateID(t *testing.T) {
	m := newTestManager(t)
	startShell(t, m, "s1", `sleep 30`, 24, 80)
	if _, err := m.Create(Spec{ID: "s1", Cmd: []string{"sleep", "30"}, Env: os.Environ()}); err != ErrExists {
		t.Fatalf("second create returned %v, want ErrExists", err)
	}
}

// Two sessions in one workspace share a filesystem -- that is why the task
// belongs to the workspace rather than the session (section 2.3).
func TestMultipleSessionsCoexist(t *testing.T) {
	m := newTestManager(t)
	a := startShell(t, m, "s1", `stty raw -echo; echo ONE; exec cat`, 24, 80)
	b := startShell(t, m, "s2", `stty raw -echo; echo TWO; exec cat`, 30, 120)

	aa, _ := a.Attach(false, 24, 80, true)
	defer a.Detach(aa)
	ba, _ := b.Attach(false, 30, 120, true)
	defer b.Detach(ba)

	if got := drain(t, aa, "ONE", 5*time.Second); !strings.Contains(got, "ONE") {
		t.Errorf("session 1 output missing: %q", got)
	}
	if got := drain(t, ba, "TWO", 5*time.Second); !strings.Contains(got, "TWO") {
		t.Errorf("session 2 output missing: %q", got)
	}
	if n := m.Count(); n != 2 {
		t.Errorf("manager holds %d sessions, want 2", n)
	}
}

// TestViewerAttachDoesNotResizeTheSession guards the quieter half of section
// 2.5's viewer rule.
//
// The relay and this package both drop a viewer's INPUT, because both attachers
// write the same PTY stdin and a viewer that can write is silently a co-driver.
// Size is the same violation wearing a different hat: the PTY has ONE size, so a
// viewer attaching from a different-sized terminal would reflow the controller's
// Claude Code -- and a browser viewer, whose size is whatever the window happens
// to be, would do it every time someone opened the page.
func TestViewerAttachDoesNotResizeTheSession(t *testing.T) {
	m := newTestManager(t)
	s := startShell(t, m, "s1", "sleep 30", 24, 80)

	// A viewer arrives from a very different-sized terminal.
	a, err := s.Attach(true, 60, 200, true)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer s.Detach(a)
	time.Sleep(150 * time.Millisecond) // let the nudge run

	if rows, cols := s.Size(); rows != 24 || cols != 80 {
		t.Fatalf("viewer resized the session to %dx%d; it must stay 80x24", cols, rows)
	}
}

// The controller is still allowed to resize on attach -- that is how a reattach
// from a differently-sized terminal gets its repaint.
func TestControllerAttachStillResizes(t *testing.T) {
	m := newTestManager(t)
	s := startShell(t, m, "s1", "sleep 30", 24, 80)

	a, err := s.Attach(false, 60, 200, true)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer s.Detach(a)
	time.Sleep(150 * time.Millisecond)

	if rows, cols := s.Size(); rows != 60 || cols != 200 {
		t.Fatalf("controller attach left the session at %dx%d, want 200x60", cols, rows)
	}
}
