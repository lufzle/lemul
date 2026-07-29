// Command runner is the agent that lives in the customer's VPC.
//
// It dials OUT to the relay and holds a single WebSocket open; yamux multiplexes
// every user session onto that one connection as a stream. Nothing listens for
// inbound connections, so the customer opens no firewall rule and grants us no
// IAM. See CC_REMOTE_ANALYSIS.md section 2.2.
//
// In production this also calls ecs:RunTask to place each sandbox; here it
// forks the PTY directly, which is enough to measure what the mux costs.
//
// Usage:
//
//	runner -relay ws://localhost:9000/tunnel -cmd claude
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net"
	"os"
	"os/exec"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
	"github.com/lufzle/tui-proxy-proto/proto"
	"github.com/lufzle/tui-proxy-proto/tunnel"
)

var (
	relayURL = flag.String("relay", "ws://localhost:9000/tunnel", "relay tunnel URL")
	cmdName  = flag.String("cmd", "claude", "command to run in each session's PTY")
	termName = flag.String("term", "xterm-256color", "TERM for the child")
	retry    = flag.Duration("retry", 2*time.Second, "reconnect delay")
)

func main() {
	flag.Parse()
	for {
		if err := connectAndServe(); err != nil {
			log.Printf("tunnel: %v; retrying in %s", err, *retry)
		}
		time.Sleep(*retry)
	}
}

func connectAndServe() error {
	ws, _, err := websocket.DefaultDialer.Dial(*relayURL, nil)
	if err != nil {
		return err
	}
	conn := tunnel.NewWSConn(ws)

	// yamux client side. The runner dials, so it is the "client" even though it
	// accepts the session streams the relay opens.
	sess, err := yamux.Client(conn, yamuxConfig())
	if err != nil {
		return err
	}
	defer func() { _ = sess.Close() }()
	log.Printf("tunnel established to %s", *relayURL)

	for {
		stream, err := sess.Accept()
		if err != nil {
			return err
		}
		go handleSession(stream)
	}
}

func yamuxConfig() *yamux.Config {
	c := yamux.DefaultConfig()
	// yamux's own keepalive doubles as tunnel liveness detection, which is what
	// tells the runner to reconnect after a network partition.
	c.EnableKeepAlive = true
	c.KeepAliveInterval = 15 * time.Second
	c.LogOutput = os.Stderr
	return c
}

// handleSession runs one PTY for the lifetime of one yamux stream.
func handleSession(stream net.Conn) {
	defer func() { _ = stream.Close() }()

	// First frame must be the hello carrying the terminal size.
	typ, payload, err := tunnel.ReadFrame(stream)
	if err != nil {
		log.Printf("session: read hello: %v", err)
		return
	}
	if typ != tunnel.FrameControl {
		log.Printf("session: expected control hello, got type %#x", typ)
		return
	}
	var hello tunnel.Hello
	if err := json.Unmarshal(payload, &hello); err != nil {
		log.Printf("session: bad hello: %v", err)
		return
	}
	if hello.Rows == 0 {
		hello.Rows = 24
	}
	if hello.Cols == 0 {
		hello.Cols = 80
	}

	name := *cmdName
	if hello.Cmd != "" {
		name = hello.Cmd
	}
	term := *termName
	if hello.Term != "" {
		term = hello.Term
	}

	c := exec.Command(name)
	c.Env = append(os.Environ(), "TERM="+term, "COLORTERM=truecolor")

	// Size applied atomically with the fork -- see CC_REMOTE_ANALYSIS.md 4.1.
	ptmx, err := pty.StartWithSize(c, &pty.Winsize{Rows: hello.Rows, Cols: hello.Cols})
	if err != nil {
		log.Printf("session: pty.StartWithSize: %v", err)
		return
	}
	defer func() { _ = ptmx.Close() }()
	log.Printf("session started: pid=%d size=%dx%d", c.Process.Pid, hello.Cols, hello.Rows)

	// PTY -> relay, as data frames.
	childGone := make(chan struct{})
	go func() {
		defer close(childGone)
		buf := make([]byte, 32*1024)
		for {
			n, rerr := ptmx.Read(buf)
			if n > 0 {
				if werr := tunnel.WriteFrame(stream, tunnel.FrameData, buf[:n]); werr != nil {
					return
				}
			}
			if rerr != nil {
				// Child exited. Closing the stream propagates through the relay
				// as a websocket close, so the client stops instead of hanging.
				_ = stream.Close()
				return
			}
		}
	}()

	// relay -> PTY, plus control.
	for {
		typ, payload, err := tunnel.ReadFrame(stream)
		if err != nil {
			break
		}
		switch typ {
		case tunnel.FrameData:
			if _, err := ptmx.Write(payload); err != nil {
				break
			}
		case tunnel.FrameControl:
			var m proto.Control
			if err := json.Unmarshal(payload, &m); err != nil {
				continue
			}
			if m.Type == proto.TypeResize {
				if err := pty.Setsize(ptmx, &pty.Winsize{Rows: m.Rows, Cols: m.Cols}); err != nil {
					log.Printf("session: setsize: %v", err)
				}
			}
		}
	}

	if c.Process != nil {
		_ = c.Process.Kill()
	}
	<-childGone
	_ = c.Wait()
	log.Printf("session ended")
}
