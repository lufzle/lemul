// Command supervisor owns a PTY running Claude Code (or any command) and
// relays it over a WebSocket.
//
// In production this runs inside the sandbox task in the customer's VPC. Here
// it runs on localhost so step 1 of spike S3 can validate terminal fidelity
// over a single hop, before the relay + mux are inserted in step 2.
//
// Usage:
//
//	supervisor -addr :8080 -cmd claude
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
	"github.com/lufzle/tui-proxy-proto/proto"
)

var (
	addr = flag.String("addr", ":8080", "listen address")
	cmd  = flag.String("cmd", "claude", "command to run in the PTY")
	term = flag.String("term", "xterm-256color", "TERM to set for the child")
)

// No Origin check: this prototype is localhost-only. Production authenticates
// at the relay before the upgrade (see CC_REMOTE_ANALYSIS.md section 2.2).
var upgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true },
}

func main() {
	flag.Parse()
	http.HandleFunc("/pty", handlePTY)
	log.Printf("supervisor listening on %s, cmd=%q", *addr, *cmd)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

// sizeFromQuery reads the client's initial terminal size from ?rows=&cols=.
// Falls back to 24x80 -- a sane default beats 0x0 if the client omits them.
func sizeFromQuery(r *http.Request) *pty.Winsize {
	ws := &pty.Winsize{Rows: 24, Cols: 80}
	if v, err := strconv.ParseUint(r.URL.Query().Get("rows"), 10, 16); err == nil && v > 0 {
		ws.Rows = uint16(v)
	}
	if v, err := strconv.ParseUint(r.URL.Query().Get("cols"), 10, 16); err == nil && v > 0 {
		ws.Cols = uint16(v)
	}
	return ws
}

func handlePTY(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade: %v", err)
		return
	}
	c := proto.NewConn(ws)
	defer c.Close()

	c2 := exec.Command(*cmd, flag.Args()...)

	// Environment matters as much as the transport. TERM must name a terminfo
	// entry the child can find, and COLORTERM is what unlocks 24-bit color --
	// without it we lose truecolor even though the pipe is clean.
	c2.Env = append(os.Environ(),
		"TERM="+*term,
		"COLORTERM=truecolor",
	)

	// The initial size must be known BEFORE the child starts. A PTY defaults to
	// 0x0, and a resize control message that arrives after spawn is too late --
	// Claude Code has already rendered its first frame into a zero-size
	// terminal. The client therefore passes rows/cols as query parameters and
	// StartWithSize applies them atomically with the fork.
	ws0 := sizeFromQuery(r)

	// StartWithSize allocates a real PTY via forkpty. This is the single most
	// important call in the prototype: with pipes instead, isatty() returns
	// false and Claude Code degrades to non-interactive mode -- no TUI at all,
	// no colors, line-buffered output. Binary failure mode.
	ptmx, err := pty.StartWithSize(c2, ws0)
	if err != nil {
		log.Printf("pty.StartWithSize: %v", err)
		return
	}
	defer func() { _ = ptmx.Close() }()

	log.Printf("session started: pid=%d size=%dx%d", c2.Process.Pid, ws0.Cols, ws0.Rows)

	// PTY -> client. Reading []byte and writing binary frames performs zero
	// transformation, so escape sequences and partial runes pass through
	// intact.
	//
	// childGone is closed when the PTY reaches EOF, which happens when the
	// child exits -- including on Ctrl-D, where the shell or REPL sees EOF on
	// stdin and terminates. Closing the websocket from here is what lets the
	// client's read loop return instead of blocking forever.
	childGone := make(chan struct{})
	go func() {
		defer close(childGone)
		buf := make([]byte, 32*1024)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				if werr := c.WriteBytes(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				// Child exited or PTY closed. Send a normal close frame so the
				// client can tell "session over" from "connection dropped" --
				// step 2 reconnects on the latter but not the former.
				_ = c.WriteCloseFrame(websocket.CloseNormalClosure, "child exited")
				_ = c.Close() // unblocks the read loop below
				return
			}
		}
	}()

	// client -> PTY, plus control messages.
	for {
		mt, data, err := c.Read()
		if err != nil {
			break
		}
		switch mt {
		case websocket.BinaryMessage:
			if _, err := ptmx.Write(data); err != nil {
				break
			}
		case websocket.TextMessage:
			var m proto.Control
			if err := json.Unmarshal(data, &m); err != nil {
				log.Printf("bad control message: %v", err)
				continue
			}
			if m.Type == proto.TypeResize {
				// ioctl(TIOCSWINSZ) -> SIGWINCH to the child. Without this
				// every TUI renders at 80x24 and wraps into garbage.
				if err := pty.Setsize(ptmx, &pty.Winsize{
					Rows: m.Rows,
					Cols: m.Cols,
				}); err != nil {
					log.Printf("setsize: %v", err)
				}
			}
		}
	}

	// Either the client disconnected or the child exited. Killing here is a
	// no-op in the child-exited case; in the client-disconnected case step 1
	// kills the child deliberately. Step 2 will instead keep it alive behind a
	// replay buffer so a closed laptop lid does not end the session.
	if c2.Process != nil {
		_ = c2.Process.Kill()
	}
	<-childGone
	_ = c2.Wait()
	log.Printf("session ended")
}
