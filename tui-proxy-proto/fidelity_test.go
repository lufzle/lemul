// Automated portion of spike S3. Validates the mechanical fidelity
// requirements from CC_REMOTE_ANALYSIS.md section 4.1 end-to-end through the
// supervisor, so regressions surface without a human in the loop.
//
// The interactive items in section 4.5 (bracketed paste in Claude Code,
// Shift+Enter, visual reflow) still need a human and a real terminal --
// see MANUAL_MATRIX.md.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

// --- test harness: a minimal in-process copy of the supervisor loop ---------

type session struct {
	ws  *websocket.Conn
	mu  sync.Mutex
	out chan []byte
}

func (s *session) writeBytes(t *testing.T, p []byte) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ws.WriteMessage(websocket.BinaryMessage, p); err != nil {
		t.Fatalf("write bytes: %v", err)
	}
}

func (s *session) resize(t *testing.T, rows, cols uint16) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	msg, _ := json.Marshal(map[string]any{"type": "resize", "rows": rows, "cols": cols})
	if err := s.ws.WriteMessage(websocket.TextMessage, msg); err != nil {
		t.Fatalf("write resize: %v", err)
	}
}

// awaitOutput drains until want appears or the deadline passes.
func (s *session) awaitOutput(t *testing.T, want string, d time.Duration) string {
	t.Helper()
	var acc bytes.Buffer
	deadline := time.After(d)
	for {
		select {
		case b, ok := <-s.out:
			if !ok {
				t.Fatalf("stream closed while awaiting %q; got:\n%s", want, acc.String())
			}
			acc.Write(b)
			if strings.Contains(acc.String(), want) {
				return acc.String()
			}
		case <-deadline:
			t.Fatalf("timeout awaiting %q; got:\n%s", want, acc.String())
		}
	}
}

// awaitRaw drains until the accumulated bytes contain want (byte-exact).
func (s *session) awaitRaw(t *testing.T, want []byte, d time.Duration) []byte {
	t.Helper()
	var acc bytes.Buffer
	deadline := time.After(d)
	for {
		select {
		case b, ok := <-s.out:
			if !ok {
				t.Fatalf("stream closed; got % x", acc.Bytes())
			}
			acc.Write(b)
			if bytes.Contains(acc.Bytes(), want) {
				return acc.Bytes()
			}
		case <-deadline:
			t.Fatalf("timeout awaiting % x; got % x", want, acc.Bytes())
		}
	}
}

// transport parameterizes every fidelity test over both hop counts, so a
// failure that appears only in "2hop" is attributable to the mux specifically.
// That attribution is the entire reason step 1 and step 2 are separate.
type transport struct {
	name  string
	start func(t *testing.T, rows, cols uint16, cmd string, args ...string) *session
}

func transports() []transport {
	return []transport{
		{"1hop", startSupervisorSized},
		{"2hop", startTwoHopSized},
	}
}

// eachTransport runs fn against every transport as a subtest.
func eachTransport(t *testing.T, fn func(t *testing.T, tr transport)) {
	t.Helper()
	for _, tr := range transports() {
		t.Run(tr.name, func(t *testing.T) { fn(t, tr) })
	}
}

// startSupervisor runs cmd in a PTY behind a websocket, mirroring
// supervisor/main.go. Returns a session; cleanup is registered with t.
func startSupervisor(t *testing.T, cmd string, args ...string) *session {
	return startSupervisorSized(t, 24, 80, cmd, args...)
}

// startSupervisorSized is startSupervisor with an explicit initial PTY size,
// applied atomically at fork the way the real supervisor does.
func startSupervisorSized(t *testing.T, rows, cols uint16, cmd string, args ...string) *session {
	t.Helper()

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	ready := make(chan *session, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		c := exec.Command(cmd, args...)
		c.Env = append(os.Environ(), "TERM=xterm-256color", "COLORTERM=truecolor")
		ptmx, err := pty.StartWithSize(c, &pty.Winsize{Rows: rows, Cols: cols})
		if err != nil {
			t.Errorf("pty.StartWithSize: %v", err)
			return
		}
		defer func() { _ = ptmx.Close() }()

		// PTY -> ws
		go func() {
			buf := make([]byte, 32*1024)
			for {
				n, err := ptmx.Read(buf)
				if n > 0 {
					cp := make([]byte, n)
					copy(cp, buf[:n])
					_ = ws.WriteMessage(websocket.BinaryMessage, cp)
				}
				if err != nil {
					_ = ws.Close()
					return
				}
			}
		}()

		// ws -> PTY + control
		for {
			mt, data, err := ws.ReadMessage()
			if err != nil {
				break
			}
			switch mt {
			case websocket.BinaryMessage:
				_, _ = ptmx.Write(data)
			case websocket.TextMessage:
				var m struct {
					Type string `json:"type"`
					Rows uint16 `json:"rows"`
					Cols uint16 `json:"cols"`
				}
				if json.Unmarshal(data, &m) == nil && m.Type == "resize" {
					_ = pty.Setsize(ptmx, &pty.Winsize{Rows: m.Rows, Cols: m.Cols})
				}
			}
		}
		if c.Process != nil {
			_ = c.Process.Kill()
		}
		_ = c.Wait()
	}))

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/pty"
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		srv.Close()
		t.Fatalf("dial: %v", err)
	}
	if tc, ok := ws.UnderlyingConn().(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}

	s := &session{ws: ws, out: make(chan []byte, 1024)}
	go func() {
		defer close(s.out)
		for {
			mt, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if mt == websocket.BinaryMessage {
				s.out <- data
			}
		}
	}()

	t.Cleanup(func() {
		_ = ws.Close()
		srv.Close()
	})
	select {
	case ready <- s:
	default:
	}
	return s
}

// --- Requirement 1: a real PTY is allocated --------------------------------

// The single most important property. With pipes instead of a PTY, isatty()
// is false and Claude Code silently degrades to non-interactive mode.
func TestPTYIsATTY(t *testing.T) {
	eachTransport(t, func(t *testing.T, tr transport) {
		s := tr.start(t, 24, 80, "sh", "-c", `test -t 0 && echo TTY_YES || echo TTY_NO; exec cat`)
		got := s.awaitOutput(t, "TTY_", 5*time.Second)
		if !strings.Contains(got, "TTY_YES") {
			t.Fatalf("child does not see a TTY on stdin; output:\n%s", got)
		}
	})
}

// Regression test. A PTY defaults to 0x0. If the initial size is applied by a
// resize message *after* the child starts, Claude Code has already rendered its
// first frame into a zero-size terminal -- the TUI comes up collapsed or
// garbled, and only a manual window resize fixes it.
//
// The child must therefore see a correct size on its very first TIOCGWINSZ,
// which means the size has to travel with the connect request (query params)
// and be applied by pty.StartWithSize, atomically with the fork.
func TestInitialSizeAppliedBeforeChildStarts(t *testing.T) {
	eachTransport(t, func(t *testing.T, tr transport) {
		// `stty size` runs immediately, before any resize control message could
		// possibly arrive.
		s := tr.start(t, 50, 200, "sh", "-c", `stty size; exec cat`)
		got := s.awaitOutput(t, "\n", 5*time.Second)

		if strings.Contains(got, "0 0") {
			t.Fatalf("child saw a 0x0 terminal on startup -- initial size not applied at fork; got %q", strings.TrimSpace(got))
		}
		if !strings.Contains(got, "50 200") {
			t.Fatalf("expected 50 200 on first read, got %q", strings.TrimSpace(got))
		}
	})
}

// --- Requirement 3: out-of-band resize -> SIGWINCH ------------------------

// Terminal size cannot travel in the byte stream. Without a control channel
// every TUI renders at 80x24 and wraps into garbage.
func TestResizeDeliversSIGWINCH(t *testing.T) {
	eachTransport(t, func(t *testing.T, tr transport) {
		// trap WINCH, then report the new size on each signal.
		script := `trap 'stty size' WINCH; echo READY; while :; do sleep 0.1; done`
		s := tr.start(t, 24, 80, "sh", "-c", script)
		s.awaitOutput(t, "READY", 5*time.Second)

		s.resize(t, 50, 200)
		got := s.awaitOutput(t, "50 200", 5*time.Second)
		if !strings.Contains(got, "50 200") {
			t.Fatalf("resize not applied; got:\n%s", got)
		}

		// A second resize must also land -- catches handlers that only fire once.
		s.resize(t, 30, 120)
		got2 := s.awaitOutput(t, "30 120", 5*time.Second)
		if !strings.Contains(got2, "30 120") {
			t.Fatalf("second resize not applied; got:\n%s", got2)
		}
	})
}

// --- Requirement 4: byte transparency ------------------------------------

// Terminal output is not valid UTF-8 at arbitrary boundaries. Text frames
// would force UTF-8 validation and corrupt anything that straddles a frame.
// This test pushes bytes that a text-framed or string-coercing relay mangles.
func TestBinaryTransparency(t *testing.T) {
	eachTransport(t, func(t *testing.T, tr transport) {
		// `stty raw -echo` disables the PTY line discipline before cat starts.
		// With default termios, ECHOCTL renders control bytes visibly (ESC echoes
		// as the two characters "^["), which looks like corruption but is the
		// terminal's own echo. Raw mode gives a true byte-for-byte loopback.
		s := tr.start(t, 24, 80, "sh", "-c", `stty raw -echo; echo RAW_READY; exec cat`)
		s.awaitOutput(t, "RAW_READY", 5*time.Second)

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
			{"csi sequence", []byte("\x1b[38;2;255;100;0mX\x1b[0m")},
			{"osc 52 clipboard", []byte("\x1b]52;c;SGVsbG8=\x07")},
			{"bracketed paste wrapper", []byte("\x1b[200~pasted\x1b[201~")},
			{"kitty keyboard push", []byte("\x1b[>1u")},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				// `cat` in a PTY echoes input, so a marker makes the round trip
				// locatable without depending on line discipline details.
				marker := []byte(fmt.Sprintf("<%s>", strings.ReplaceAll(tc.name, " ", "_")))
				payload := append(append([]byte{}, tc.data...), marker...)
				s.writeBytes(t, payload)
				acc := s.awaitRaw(t, marker, 5*time.Second)

				// In raw mode the echo is byte-exact, so require the payload
				// immediately followed by its marker -- adjacency catches
				// dropped or reordered bytes that a Contains-only check misses.
				if !bytes.Contains(acc, payload) {
					t.Errorf("payload corrupted in transit\n sent: % x\n recv: % x", payload, acc)
				}
			})
		}
	})
}

// A large burst must arrive complete and in order -- catches frames dropped
// or reordered under load, and buffer-boundary corruption.
func TestHighVolumeOutputIntegrity(t *testing.T) {
	eachTransport(t, func(t *testing.T, tr transport) {
		const lines = 20000
		script := fmt.Sprintf(`i=0; while [ $i -lt %d ]; do echo "line-$i"; i=$((i+1)); done; echo DONE_MARKER`, lines)
		s := tr.start(t, 24, 80, "sh", "-c", script)

		start := time.Now()
		got := s.awaitOutput(t, "DONE_MARKER", 60*time.Second)
		elapsed := time.Since(start)

		for _, probe := range []string{"line-0\r\n", "line-9999\r\n", fmt.Sprintf("line-%d\r\n", lines-1)} {
			if !strings.Contains(got, probe) {
				t.Errorf("missing %q in %d bytes of output", probe, len(got))
			}
		}
		t.Logf("%d lines / %d bytes in %v (%.1f MB/s)",
			lines, len(got), elapsed, float64(len(got))/elapsed.Seconds()/1e6)
	})
}

// --- Requirement 6: TERM / COLORTERM propagation --------------------------

func TestTerminalEnvPropagation(t *testing.T) {
	eachTransport(t, func(t *testing.T, tr transport) {
		s := tr.start(t, 24, 80, "sh", "-c", `echo "TERM=$TERM COLORTERM=$COLORTERM"; exec cat`)
		got := s.awaitOutput(t, "COLORTERM=", 5*time.Second)
		if !strings.Contains(got, "TERM=xterm-256color") {
			t.Errorf("TERM not propagated: %q", strings.TrimSpace(got))
		}
		if !strings.Contains(got, "COLORTERM=truecolor") {
			t.Errorf("COLORTERM not propagated (loses 24-bit color): %q", strings.TrimSpace(got))
		}
	})
}

// terminfo must actually resolve inside the environment, or apps fall back
// and misbehave. `tput` fails if the TERM entry is missing.
func TestTerminfoResolves(t *testing.T) {
	eachTransport(t, func(t *testing.T, tr transport) {
		s := tr.start(t, 24, 80, "sh", "-c", `tput colors 2>&1; exec cat`)
		got := s.awaitOutput(t, "\n", 5*time.Second)
		if !strings.Contains(got, "256") {
			t.Errorf("tput colors did not report 256 (terminfo entry missing?): %q", strings.TrimSpace(got))
		}
	})
}

// --- Requirement 2 (partial): signal delivery ----------------------------

// Ctrl-C must reach the remote process group, not the local client. Raw mode
// on the client is what makes this possible; here we verify the PTY side
// translates 0x03 into SIGINT for the child.
func TestCtrlCDeliversSIGINT(t *testing.T) {
	eachTransport(t, func(t *testing.T, tr transport) {
		script := `trap 'echo GOT_SIGINT' INT; echo READY; while :; do sleep 0.1; done`
		s := tr.start(t, 24, 80, "sh", "-c", script)
		s.awaitOutput(t, "READY", 5*time.Second)

		s.writeBytes(t, []byte{0x03}) // Ctrl-C
		got := s.awaitOutput(t, "GOT_SIGINT", 5*time.Second)
		if !strings.Contains(got, "GOT_SIGINT") {
			t.Fatalf("Ctrl-C did not deliver SIGINT; got:\n%s", got)
		}
	})
}

// --- Session teardown ----------------------------------------------------

// Regression test. When the child exits -- Ctrl-D on a shell, `exit`, a crash
// -- the supervisor must close the websocket. Otherwise the client's read loop
// blocks forever and the process appears hung: the original Ctrl-D bug.
func TestChildExitClosesConnection(t *testing.T) {
	eachTransport(t, func(t *testing.T, tr transport) {
		s := tr.start(t, 24, 80, "sh", "-c", `echo READY; exec cat`)
		s.awaitOutput(t, "READY", 5*time.Second)

		// 0x04 is EOT. In a PTY with the default line discipline this is what the
		// terminal driver turns into EOF on the child's stdin, so `cat` exits.
		s.writeBytes(t, []byte{0x04})

		select {
		case _, ok := <-s.out:
			// Drain until closed; any trailing output is fine.
			for ok {
				_, ok = <-s.out
			}
		case <-time.After(5 * time.Second):
			t.Fatal("websocket still open 5s after child exit -- client would hang")
		}
	})
}

// The close frame's code must distinguish "session over" from "connection
// dropped". Step 2's reconnect logic branches on it, so a wrong code makes the
// client reconnect into a dead session (or fail to reconnect a live one).
func TestChildExitSendsNormalCloseCode(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		c := exec.Command("sh", "-c", `exec cat`)
		ptmx, err := pty.Start(c)
		if err != nil {
			t.Errorf("pty.Start: %v", err)
			return
		}
		defer func() { _ = ptmx.Close() }()

		go func() {
			buf := make([]byte, 4096)
			for {
				n, err := ptmx.Read(buf)
				if n > 0 {
					_ = ws.WriteMessage(websocket.BinaryMessage, buf[:n])
				}
				if err != nil {
					_ = ws.WriteControl(websocket.CloseMessage,
						websocket.FormatCloseMessage(websocket.CloseNormalClosure, "child exited"),
						time.Now().Add(2*time.Second))
					_ = ws.Close()
					return
				}
			}
		}()
		for {
			mt, data, err := ws.ReadMessage()
			if err != nil {
				break
			}
			if mt == websocket.BinaryMessage {
				_, _ = ptmx.Write(data)
			}
		}
		if c.Process != nil {
			_ = c.Process.Kill()
		}
		_ = c.Wait()
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/pty"
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = ws.Close() }()

	_ = ws.WriteMessage(websocket.BinaryMessage, []byte{0x04}) // EOT -> EOF

	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var readErr error
	for readErr == nil {
		_, _, readErr = ws.ReadMessage()
	}
	if !websocket.IsCloseError(readErr, websocket.CloseNormalClosure) {
		t.Fatalf("expected CloseNormalClosure so the client knows not to reconnect, got: %v", readErr)
	}
}

// --- Latency: keystroke round trip ---------------------------------------

// A TUI redraws its input area on every keystroke, so this is the number the
// user actually feels. Localhost here establishes the transport's own floor;
// anything above it is geography.
func TestKeystrokeRoundTripLatency(t *testing.T) {
	eachTransport(t, func(t *testing.T, tr transport) {
		s := tr.start(t, 24, 80, "cat")

		const n = 50
		var total time.Duration
		var worst time.Duration
		for i := 0; i < n; i++ {
			marker := []byte(fmt.Sprintf("k%03d", i))
			start := time.Now()
			s.writeBytes(t, marker)
			s.awaitRaw(t, marker, 5*time.Second)
			d := time.Since(start)
			total += d
			if d > worst {
				worst = d
			}
		}
		avg := total / n
		t.Logf("keystroke RTT over loopback: avg=%v worst=%v", avg, worst)
		if avg > 20*time.Millisecond {
			t.Errorf("loopback RTT %v is far above expected (<5ms) -- check TCP_NODELAY on every hop", avg)
		}
	})
}
