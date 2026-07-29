package main

// Two-hop test harness: client -> relay -> yamux tunnel -> runner -> PTY.
//
// Mirrors relay/main.go and runner/main.go closely enough that a fidelity
// regression here indicts the mux and the tunnel framing, not the harness.

import (
	"encoding/json"
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
	"github.com/hashicorp/yamux"
	"github.com/lufzle/tui-proxy-proto/proto"
	"github.com/lufzle/tui-proxy-proto/tunnel"
)

// startTwoHopSized brings up a relay and an outbound-dialing runner, then
// returns a session attached through both.
func startTwoHopSized(t *testing.T, rows, cols uint16, cmd string, args ...string) *session {
	t.Helper()

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	var regMu sync.Mutex
	var muxSess *yamux.Session
	registered := make(chan struct{})
	var once sync.Once

	mux := http.NewServeMux()

	// --- relay: /tunnel (runner dials in) ---
	mux.HandleFunc("/tunnel", func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		cfg := yamux.DefaultConfig()
		cfg.EnableKeepAlive = true
		cfg.LogOutput = os.Stderr
		s, err := yamux.Server(tunnel.NewWSConn(ws), cfg)
		if err != nil {
			t.Errorf("yamux.Server: %v", err)
			return
		}
		regMu.Lock()
		muxSess = s
		regMu.Unlock()
		once.Do(func() { close(registered) })
		<-s.CloseChan()
	})

	// --- relay: /pty (user connects) ---
	mux.HandleFunc("/pty", func(w http.ResponseWriter, r *http.Request) {
		regMu.Lock()
		s := muxSess
		regMu.Unlock()
		if s == nil {
			http.Error(w, "no runner", http.StatusServiceUnavailable)
			return
		}
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		c := proto.NewConn(ws)
		defer c.Close()

		stream, err := s.Open()
		if err != nil {
			return
		}
		defer func() { _ = stream.Close() }()

		if err := tunnel.WriteControlFrame(stream, tunnel.Hello{Rows: rows, Cols: cols}); err != nil {
			return
		}

		go func() {
			defer c.Close()
			for {
				typ, payload, err := tunnel.ReadFrame(stream)
				if err != nil {
					_ = c.WriteCloseFrame(websocket.CloseNormalClosure, "session ended")
					return
				}
				if typ == tunnel.FrameData {
					if err := c.WriteBytes(payload); err != nil {
						return
					}
				}
			}
		}()

		for {
			mt, data, err := c.Read()
			if err != nil {
				return
			}
			var typ byte
			switch mt {
			case websocket.BinaryMessage:
				typ = tunnel.FrameData
			case websocket.TextMessage:
				typ = tunnel.FrameControl
			default:
				continue
			}
			if err := tunnel.WriteFrame(stream, typ, data); err != nil {
				return
			}
		}
	})

	srv := httptest.NewServer(mux)

	// --- runner: dials out, accepts streams, forks PTYs ---
	tunnelURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/tunnel"
	rws, _, err := websocket.DefaultDialer.Dial(tunnelURL, nil)
	if err != nil {
		srv.Close()
		t.Fatalf("runner dial: %v", err)
	}
	rcfg := yamux.DefaultConfig()
	rcfg.EnableKeepAlive = true
	rcfg.LogOutput = os.Stderr
	runnerSess, err := yamux.Client(tunnel.NewWSConn(rws), rcfg)
	if err != nil {
		srv.Close()
		t.Fatalf("yamux.Client: %v", err)
	}

	go func() {
		for {
			stream, err := runnerSess.Accept()
			if err != nil {
				return
			}
			go runnerHandleSession(t, stream, cmd, args)
		}
	}()

	select {
	case <-registered:
	case <-time.After(5 * time.Second):
		srv.Close()
		t.Fatal("runner tunnel never registered")
	}

	// --- client ---
	ptyURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/pty"
	cws, _, err := websocket.DefaultDialer.Dial(ptyURL, nil)
	if err != nil {
		srv.Close()
		t.Fatalf("client dial: %v", err)
	}
	if tc, ok := cws.UnderlyingConn().(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}

	s := &session{ws: cws, out: make(chan []byte, 1024)}
	go func() {
		defer close(s.out)
		for {
			mt, data, err := cws.ReadMessage()
			if err != nil {
				return
			}
			if mt == websocket.BinaryMessage {
				s.out <- data
			}
		}
	}()

	t.Cleanup(func() {
		_ = cws.Close()
		_ = runnerSess.Close()
		_ = rws.Close()
		srv.Close()
	})
	return s
}

// runnerHandleSession mirrors runner/main.go's handleSession.
func runnerHandleSession(t *testing.T, stream net.Conn, cmd string, args []string) {
	defer func() { _ = stream.Close() }()

	typ, payload, err := tunnel.ReadFrame(stream)
	if err != nil || typ != tunnel.FrameControl {
		return
	}
	var hello tunnel.Hello
	if err := json.Unmarshal(payload, &hello); err != nil {
		return
	}
	if hello.Rows == 0 {
		hello.Rows = 24
	}
	if hello.Cols == 0 {
		hello.Cols = 80
	}

	c := exec.Command(cmd, args...)
	c.Env = append(os.Environ(), "TERM=xterm-256color", "COLORTERM=truecolor")
	ptmx, err := pty.StartWithSize(c, &pty.Winsize{Rows: hello.Rows, Cols: hello.Cols})
	if err != nil {
		t.Errorf("pty.StartWithSize: %v", err)
		return
	}
	defer func() { _ = ptmx.Close() }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 32*1024)
		for {
			n, rerr := ptmx.Read(buf)
			if n > 0 {
				if werr := tunnel.WriteFrame(stream, tunnel.FrameData, buf[:n]); werr != nil {
					return
				}
			}
			if rerr != nil {
				_ = stream.Close()
				return
			}
		}
	}()

	for {
		typ, payload, err := tunnel.ReadFrame(stream)
		if err != nil {
			break
		}
		switch typ {
		case tunnel.FrameData:
			if _, werr := ptmx.Write(payload); werr != nil {
				break
			}
		case tunnel.FrameControl:
			var m proto.Control
			if json.Unmarshal(payload, &m) == nil && m.Type == proto.TypeResize {
				_ = pty.Setsize(ptmx, &pty.Winsize{Rows: m.Rows, Cols: m.Cols})
			}
		}
	}

	if c.Process != nil {
		_ = c.Process.Kill()
	}
	<-done
	_ = c.Wait()
}

// --- framing unit tests -------------------------------------------------

// The tunnel framing is new surface that the one-hop path does not exercise.
// A length-prefix bug would corrupt terminal output in ways that look like a
// TUI problem, so it gets direct coverage.
func TestTunnelFrameRoundTrip(t *testing.T) {
	cases := [][]byte{
		{},
		{0x00},
		[]byte("\x1b[200~pasted\x1b[201~"),
		func() []byte {
			b := make([]byte, 0, 256)
			for i := 0; i < 256; i++ {
				b = append(b, byte(i))
			}
			return b
		}(),
		make([]byte, 64*1024), // larger than one PTY read
	}

	var buf strings.Builder
	pr, pw := net.Pipe()
	go func() {
		for _, c := range cases {
			_ = tunnel.WriteFrame(pw, tunnel.FrameData, c)
		}
		_ = tunnel.WriteControlFrame(pw, proto.Control{Type: proto.TypeResize, Rows: 50, Cols: 200})
		_ = pw.Close()
	}()

	for i, want := range cases {
		typ, got, err := tunnel.ReadFrame(pr)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if typ != tunnel.FrameData {
			t.Fatalf("case %d: type = %#x, want data", i, typ)
		}
		if len(got) != len(want) {
			t.Fatalf("case %d: len = %d, want %d", i, len(got), len(want))
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("case %d: byte %d = %#x, want %#x", i, j, got[j], want[j])
			}
		}
	}

	typ, payload, err := tunnel.ReadFrame(pr)
	if err != nil {
		t.Fatalf("control frame: %v", err)
	}
	if typ != tunnel.FrameControl {
		t.Fatalf("control frame type = %#x", typ)
	}
	var m proto.Control
	if err := json.Unmarshal(payload, &m); err != nil {
		t.Fatalf("control unmarshal: %v", err)
	}
	if m.Rows != 50 || m.Cols != 200 {
		t.Fatalf("control = %+v, want 50x200", m)
	}
	_ = buf
}

// A control frame must never be delivered as terminal data. If it were, JSON
// would be injected into the user's screen; the inverse would silently drop
// output. This is the specific hazard the tunnel's explicit type tag exists to
// prevent, since WebSocket frame types do not survive the mux.
func TestTunnelFrameTypesDoNotCross(t *testing.T) {
	pr, pw := net.Pipe()
	go func() {
		_ = tunnel.WriteControlFrame(pw, proto.Control{Type: proto.TypeResize, Rows: 1, Cols: 1})
		_ = tunnel.WriteFrame(pw, tunnel.FrameData, []byte("PTYDATA"))
		_ = pw.Close()
	}()

	typ, _, err := tunnel.ReadFrame(pr)
	if err != nil || typ != tunnel.FrameControl {
		t.Fatalf("first frame: typ=%#x err=%v, want control", typ, err)
	}
	typ, payload, err := tunnel.ReadFrame(pr)
	if err != nil || typ != tunnel.FrameData {
		t.Fatalf("second frame: typ=%#x err=%v, want data", typ, err)
	}
	if string(payload) != "PTYDATA" {
		t.Fatalf("payload = %q", payload)
	}
}
