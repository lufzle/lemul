package e2e_test

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// These are the TUI fidelity requirements, ported onto the runner/supervisor
// split. The job here is to prove the split does not degrade the PTY path.

// --- Requirement 1: a real PTY -------------------------------------------

// With pipes instead of a PTY, isatty() is false and Claude Code silently
// degrades to non-interactive mode: no TUI, no colour, line-buffered. It is the
// single most important property and a binary failure mode.
func TestPTYIsATTY(t *testing.T) {
	s := newStack(t, "sh", "-c", `test -t 0 && echo IS_A_TTY || echo NOT_A_TTY; exec cat`)
	c := s.attach(s.newSession("w1"), 24, 80, "")

	got := c.await("TTY", 10*time.Second)
	if !strings.Contains(got, "IS_A_TTY") {
		t.Fatalf("child does not see a TTY: %q", got)
	}
}

// --- Requirement 3: size known before the fork ---------------------------

// A PTY defaults to 0x0. If the size arrives as a post-connect control message
// the child has already drawn its first frame into a zero-size terminal --
// visible as a collapsed TUI that only fixes itself on a manual resize. The
// size therefore travels in the attach request and is applied by
// StartWithSize, atomically with the fork.
func TestInitialSizeAppliedBeforeChildStarts(t *testing.T) {
	s := newStack(t, "sh", "-c", `stty size; exec cat`)
	c := s.attach(s.newSession("w1"), 50, 200, "")

	got := c.await("\n", 10*time.Second)
	if strings.Contains(got, "0 0") {
		t.Fatalf("child started at 0x0: %q", strings.TrimSpace(got))
	}
	if !strings.Contains(got, "50 200") {
		t.Fatalf("initial size not applied at fork; got %q, want 50 200", strings.TrimSpace(got))
	}
}

// Resize must reach the child as SIGWINCH, and must keep working after the
// first one -- a handler that fires once looks correct in a single-resize test.
func TestResizeDeliversSIGWINCH(t *testing.T) {
	s := newStack(t, "sh", "-c", `trap 'stty size' WINCH; echo READY; while :; do sleep 0.1; done`)
	c := s.attach(s.newSession("w1"), 24, 80, "")
	c.await("READY", 10*time.Second)

	c.resize(40, 150)
	if got := c.await("40 150", 10*time.Second); !strings.Contains(got, "40 150") {
		t.Fatalf("first resize not applied; got:\n%s", got)
	}
	c.resize(30, 120)
	if got := c.await("30 120", 10*time.Second); !strings.Contains(got, "30 120") {
		t.Fatalf("second resize not applied; got:\n%s", got)
	}
}

// --- Requirement 4: byte transparency ------------------------------------

// Terminal output is not valid UTF-8 at arbitrary boundaries. Text frames would
// force UTF-8 validation and corrupt anything that straddles a frame. These
// payloads are what a text-framed or string-coercing relay mangles.
//
// The split added a second framing scheme (the tunnel's explicit type tag), so
// this is also the regression guard for the frame-type mapping in the relay.
func TestBinaryTransparency(t *testing.T) {
	// `stty raw -echo` disables the PTY line discipline before cat starts. With
	// default termios, ECHOCTL renders control bytes visibly (ESC echoes as the
	// two characters "^["), which looks like corruption but is the terminal's
	// own echo.
	s := newStack(t, "sh", "-c", `stty raw -echo; echo RAW_READY; exec cat`)
	c := s.attach(s.newSession("w1"), 24, 80, "")
	c.await("RAW_READY", 10*time.Second)

	cases := []struct {
		name string
		data []byte
	}{
		{"invalid utf8 lone continuation", []byte{0x41, 0x80, 0x42}},
		{"invalid utf8 truncated 3-byte", []byte{0x43, 0xE2, 0x82, 0x44}},
		{"all high bytes", func() []byte {
			b := make([]byte, 0, 128)
			for i := 0x80; i <= 0xFF; i++ {
				b = append(b, byte(i))
			}
			return b
		}()},
		{"csi truecolor", []byte("\x1b[38;2;255;100;0mX\x1b[0m")},
		{"osc 52 clipboard", []byte("\x1b]52;c;SGVsbG8=\x07")},
		{"bracketed paste wrapper", []byte("\x1b[200~pasted\x1b[201~")},
		{"kitty keyboard push", []byte("\x1b[>1u")},
		{"modify other keys", []byte("\x1b[>4;2m")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			marker := []byte(fmt.Sprintf("<%s>", strings.ReplaceAll(tc.name, " ", "_")))
			payload := append(append([]byte{}, tc.data...), marker...)
			c.writeBytes(payload)
			acc := c.awaitRaw(marker, 10*time.Second)

			// Adjacency, not mere presence: it catches dropped or reordered
			// bytes that a Contains-only check on the payload would miss.
			if !bytes.Contains(acc, payload) {
				t.Errorf("payload corrupted in transit\n sent: % x\n recv: % x", payload, acc)
			}
		})
	}
}

// A large burst must arrive complete and in order: catches frames dropped or
// reordered under load, and buffer-boundary corruption.
func TestHighVolumeOutputIntegrity(t *testing.T) {
	const lines = 20000
	script := fmt.Sprintf(`i=0; while [ $i -lt %d ]; do echo "line-$i"; i=$((i+1)); done; echo DONE_MARKER; exec cat`, lines)
	s := newStack(t, "sh", "-c", script)
	c := s.attach(s.newSession("w1"), 24, 80, "")

	start := time.Now()
	got := c.await("DONE_MARKER", 60*time.Second)
	elapsed := time.Since(start)

	for _, probe := range []string{"line-0\r\n", "line-9999\r\n", fmt.Sprintf("line-%d\r\n", lines-1)} {
		if !strings.Contains(got, probe) {
			t.Errorf("missing %q in %d bytes of output", probe, len(got))
		}
	}
	t.Logf("%d lines / %d bytes in %v (%.1f MB/s)",
		lines, len(got), elapsed, float64(len(got))/elapsed.Seconds()/1e6)
}

// --- Requirement 6: TERM / COLORTERM --------------------------------------

func TestTerminalEnvPropagation(t *testing.T) {
	s := newStack(t, "sh", "-c", `echo "TERM=$TERM COLORTERM=$COLORTERM"; exec cat`)
	c := s.attach(s.newSession("w1"), 24, 80, "")

	got := c.await("COLORTERM=", 10*time.Second)
	if !strings.Contains(got, "TERM=xterm-256color") {
		t.Errorf("TERM not propagated: %q", strings.TrimSpace(got))
	}
	if !strings.Contains(got, "COLORTERM=truecolor") {
		t.Errorf("COLORTERM not propagated (loses 24-bit colour): %q", strings.TrimSpace(got))
	}
}

// terminfo must actually resolve, or apps fall back and misbehave. This is what
// ncurses-term in the sandbox image is for.
func TestTerminfoResolves(t *testing.T) {
	s := newStack(t, "sh", "-c", `tput colors 2>&1; exec cat`)
	c := s.attach(s.newSession("w1"), 24, 80, "")

	got := c.await("\n", 10*time.Second)
	if !strings.Contains(got, "256") {
		t.Errorf("tput colors did not report 256 (terminfo entry missing?): %q", strings.TrimSpace(got))
	}
}

// --- Requirement 2: signal delivery ---------------------------------------

// Ctrl-C must reach the remote process, not the local client. Raw mode on the
// client is what makes that possible; here we verify the PTY side turns 0x03
// into SIGINT for the child rather than anything on the path swallowing it.
func TestCtrlCDeliversSIGINT(t *testing.T) {
	s := newStack(t, "sh", "-c", `trap 'echo GOT_SIGINT' INT; echo READY; while :; do sleep 0.1; done`)
	c := s.attach(s.newSession("w1"), 24, 80, "")
	c.await("READY", 10*time.Second)

	c.writeBytes([]byte{0x03})
	if got := c.await("GOT_SIGINT", 10*time.Second); !strings.Contains(got, "GOT_SIGINT") {
		t.Fatalf("Ctrl-C did not deliver SIGINT; got:\n%s", got)
	}
}

// --- Session end ----------------------------------------------------------

// When the child exits the connection must close, or Ctrl-D hangs the client
// forever. The close CODE is load-bearing: it is how the client distinguishes
// "session over, do not reconnect" from "connection dropped, reconnect".
func TestChildExitSendsNormalCloseCode(t *testing.T) {
	s := newStack(t, "sh", "-c", `echo BYE; exit 0`)
	c := s.attach(s.newSession("w1"), 24, 80, "")
	c.await("BYE", 10*time.Second)

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		closed, err := c.isClosed()
		if closed {
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				t.Fatalf("close was not CloseNormalClosure: %v", err)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("connection did not close after the child exited")
}

// --- Latency --------------------------------------------------------------

// The floor the transport itself adds. Everything above it in production is
// geography, which is the argument for placing the relay in the customer's
// region rather than near the user.
func TestKeystrokeRoundTripLatency(t *testing.T) {
	s := newStack(t, "sh", "-c", `stty raw -echo; echo READY; exec cat`)
	c := s.attach(s.newSession("w1"), 24, 80, "")
	c.await("READY", 10*time.Second)

	const n = 20
	var total time.Duration
	for i := range n {
		probe := []byte(fmt.Sprintf("<k%d>", i))
		start := time.Now()
		c.writeBytes(probe)
		if got := c.awaitRaw(probe, 5*time.Second); !bytes.Contains(got, probe) {
			t.Fatalf("keystroke %d never echoed back", i)
		}
		total += time.Since(start)
	}
	t.Logf("average keystroke round trip over the full split path: %v", total/n)
}
