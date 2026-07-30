package e2e_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestClaudeCodeDetachReattach is the Phase 1 exit criterion, minus Fargate:
// a real Claude Code process, detached from and reattached to, must come back
// with its screen intact.
//
// Opt-in, because it needs the claude binary and a working login on the machine
// running the test:
//
//	LEMUL_E2E_CLAUDE=1 go test ./e2e -run TestClaudeCode -v
//
// The screen-correctness half of this was measured separately and more
// rigorously in winch-probe/, by replaying the nudge output into a fresh VT
// emulator. What this adds is that the mechanism survives the full split path.
func TestClaudeCodeDetachReattach(t *testing.T) {
	if os.Getenv("LEMUL_E2E_CLAUDE") == "" {
		t.Skip("set LEMUL_E2E_CLAUDE=1 to run against the real claude binary")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skipf("claude not on PATH: %v", err)
	}

	s := newStack(t, "claude")
	sid := s.newSession("w1")

	first := s.attach(sid, 40, 100, "")
	// The banner is the first thing Claude Code draws once it has a real PTY.
	if got := first.await("Claude Code", 60*time.Second); !strings.Contains(got, "Claude Code") {
		t.Fatalf("Claude Code never rendered; got %d bytes:\n%q", len(got), tail(got, 2000))
	}
	// A TUI at all means isatty() was true -- the binary failure mode did not
	// happen.
	if !strings.Contains(first.await("\x1b[", 5*time.Second), "\x1b[") {
		t.Error("no escape sequences: Claude Code may have degraded to non-interactive mode")
	}
	first.detach()

	time.Sleep(2 * time.Second)

	second := s.attach(sid, 40, 100, "")
	// Wait on the repaint, not on the banner: the banner arrives first, from the
	// replay ring, so awaiting it would sample the stream before the nudge has
	// had a chance to produce anything.
	//
	// ESC[2K per line is the signature of the full-viewport repaint that the
	// whole of decision #4 rests on.
	got := second.await("\x1b[2K", 30*time.Second)

	if !strings.Contains(got, "\x1b[2K") {
		t.Error("no erase-line sequences: the nudge did not trigger a full repaint")
	}
	if !strings.Contains(got, "Claude Code") {
		t.Fatalf("reattach did not restore the session's screen:\n%q", tail(got, 2000))
	}
	// The mode prelude is the part the SIGWINCH repaint does NOT restore, and
	// the part that silently breaks Shift+Enter and paste when missing.
	for _, want := range []string{"\x1b[?2004h", "\x1b[>1u"} {
		if !strings.Contains(got, want) {
			t.Errorf("reattach did not restore terminal mode %q", want)
		}
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
