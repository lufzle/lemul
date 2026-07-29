// Command client attaches the local terminal to a remote PTY over WebSocket.
//
// This stands in for the `ourcli connect` command. The user's own terminal
// emulator (iTerm2, Ghostty, Windows Terminal) does the VT emulation -- this
// process only moves bytes and reports size changes.
//
// Usage:
//
//	client -url ws://localhost:8080/pty
package main

import (
	"bytes"
	"flag"
	"fmt"
	neturl "net/url"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"

	"github.com/gorilla/websocket"
	"github.com/lufzle/tui-proxy-proto/proto"
	"golang.org/x/term"
)

var url = flag.String("url", "ws://localhost:8080/pty", "supervisor websocket URL")

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\r\nclient: %v\r\n", err)
		os.Exit(1)
	}
}

func run() error {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return fmt.Errorf("stdin is not a terminal")
	}

	// The size must reach the supervisor before it forks the child: a PTY
	// defaults to 0x0, and a resize message sent after connect arrives too
	// late -- Claude Code has already drawn its first frame. So it goes in the
	// dial URL, not in a control message.
	dialURL := *url
	if cols, rows, err := term.GetSize(fd); err == nil {
		u, uerr := neturl.Parse(dialURL)
		if uerr != nil {
			return fmt.Errorf("parse url: %w", uerr)
		}
		q := u.Query()
		q.Set("rows", strconv.Itoa(rows))
		q.Set("cols", strconv.Itoa(cols))
		u.RawQuery = q.Encode()
		dialURL = u.String()
	}

	ws, _, err := websocket.DefaultDialer.Dial(dialURL, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	c := proto.NewConn(ws)
	defer c.Close()

	// Raw mode disables ICANON, ECHO, and ISIG so that Ctrl-C, arrow keys, and
	// bracketed-paste sequences are delivered as bytes to the remote process
	// instead of being interpreted locally. Without this, Ctrl-C kills this
	// client rather than interrupting Claude Code's current tool call.
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("make raw: %w", err)
	}

	// Restore on every exit path. The deferred call covers normal return and
	// panics; the signal handler below covers SIGTERM; the exit paths call it
	// explicitly so their closing message prints with the terminal already
	// sane. sync.Once makes the duplicate calls harmless. Leaving the terminal
	// in raw mode leaves the user with a broken shell.
	var restoreOnce sync.Once
	restore := func() { restoreOnce.Do(func() { _ = term.Restore(fd, oldState) }) }
	defer restore()

	fmt.Fprintf(os.Stdout, "[connected to %s — Ctrl-] to detach]\r\n", *url)

	fatal := make(chan os.Signal, 1)
	signal.Notify(fatal, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-fatal
		restore()
		os.Exit(1)
	}()

	// The initial size already went out in the dial URL (above). This tracks
	// subsequent changes: SIGWINCH fires when the user resizes the window, and
	// the supervisor turns each report into TIOCSWINSZ.
	sendSize := func() {
		cols, rows, err := term.GetSize(fd)
		if err != nil {
			return
		}
		_ = c.WriteControl(proto.Control{
			Type: proto.TypeResize,
			Rows: uint16(rows),
			Cols: uint16(cols),
		})
	}

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	go func() {
		for range winch {
			sendSize()
		}
	}()

	// stdin -> remote, as binary frames.
	//
	// In raw mode every byte is forwarded, so Ctrl-C reaches Claude Code and
	// interrupts its tool call rather than killing this process -- that is the
	// intended behavior, not a bug. Detaching therefore needs its own escape
	// sequence, the way SSH uses "~.".
	detach := make(chan struct{})
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				if idx := findEscape(buf[:n]); idx >= 0 {
					// Forward anything typed before the escape, then detach.
					if idx > 0 {
						_ = c.WriteBytes(buf[:idx])
					}
					close(detach)
					return
				}
				if werr := c.WriteBytes(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				// Local stdin closed (rare in raw mode). Leave the remote
				// session alone and let the read loop decide.
				return
			}
		}
	}()

	// remote -> stdout. Writing straight to the fd avoids buffering that would
	// delay a redraw; the local emulator handles the escape sequences.
	type readResult struct{ err error }
	remoteDone := make(chan readResult, 1)
	go func() {
		for {
			mt, data, err := c.Read()
			if err != nil {
				remoteDone <- readResult{err}
				return
			}
			if mt == websocket.BinaryMessage {
				if _, werr := os.Stdout.Write(data); werr != nil {
					remoteDone <- readResult{werr}
					return
				}
			}
		}
	}()

	select {
	case <-detach:
		// Deliberate detach: tell the supervisor we are going away cleanly.
		_ = c.WriteCloseFrame(websocket.CloseGoingAway, "client detached")
		restore()
		fmt.Fprint(os.Stdout, "\r\n[detached]\r\n")
		return nil

	case r := <-remoteDone:
		restore()
		switch {
		case r.err == nil || proto.IsCleanClose(r.err):
			// Child exited normally -- e.g. Ctrl-D, or `exit`.
			fmt.Fprint(os.Stdout, "\r\n[session ended]\r\n")
			return nil
		default:
			// Connection dropped without a close frame. In production this is
			// the case that triggers reconnect + replay.
			return fmt.Errorf("connection lost: %w", r.err)
		}
	}
}

// escapeSeq detaches the client without sending anything to the remote.
// Ctrl-] is the classic telnet escape and is not bound by Claude Code.
var escapeSeq = []byte{0x1d}

func findEscape(p []byte) int { return bytes.Index(p, escapeSeq) }
