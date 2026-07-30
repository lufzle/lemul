// Command ourcli attaches the local terminal to a remote Claude Code session.
//
// The user's own terminal emulator does the VT emulation; this process only
// moves bytes and reports size changes. That is what keeps fidelity: every
// escape sequence Claude Code emits reaches a real emulator untouched.
//
// Usage:
//
//	ourcli connect <workspace>               # reattach if idle, else new
//	ourcli connect <workspace> -new          # always a new session
//	ourcli connect <workspace> -session <id> # a specific session
//	ourcli ls <workspace>                    # what is running in there
//	ourcli stop <workspace> -session <id>    # Ctrl-C/Ctrl-D; conversation kept
//	ourcli resume <workspace> -session <id>  # start it again, history intact
//	ourcli rm <workspace> -session <id>      # end it and drop the conversation
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/gorilla/websocket"
	"github.com/lufzle/lemul-cc/internal/proto"
	"golang.org/x/term"
)

var (
	server    *string
	sessionID *string
	mode      *string
	forceNew  *bool
)

// main parses the subcommand and its positional argument itself, then hands the
// rest to the flag package.
//
// flag.Parse alone would not do: it stops at the first non-flag argument, so
// `ourcli connect w1 -session s-123` would silently ignore -session -- and that
// is precisely the line the detach message tells the user to type.
func main() {
	fs := flag.NewFlagSet("ourcli", flag.ExitOnError)
	server = fs.String("server", "http://localhost:9000", "control plane base URL")
	sessionID = fs.String("session", "", "attach to this specific session")
	mode = fs.String("mode", "control", "control|viewer")
	forceNew = fs.Bool("new", false, "always start a new session, never reattach")

	force := fs.Bool("force", false, "stop: SIGKILL rather than SIGHUP. rm: delete even if running")

	usage := func() {
		fmt.Fprintf(os.Stderr, "usage: ourcli connect <workspace> [flags]\n")
		fmt.Fprintf(os.Stderr, "       ourcli ls      <workspace>\n")
		fmt.Fprintf(os.Stderr, "       ourcli stop    <workspace> -session <id>\n")
		fmt.Fprintf(os.Stderr, "       ourcli resume  <workspace> -session <id>\n")
		fmt.Fprintf(os.Stderr, "       ourcli rm      <workspace> -session <id> [-force]\n\n")
		fmt.Fprintf(os.Stderr, "-session accepts any unique prefix of a session id.\n\n")
		fs.PrintDefaults()
	}
	fs.Usage = usage

	args := os.Args[1:]
	if len(args) < 2 || strings.HasPrefix(args[1], "-") {
		usage()
		os.Exit(2)
	}
	cmd, workspace := args[0], args[1]
	if err := fs.Parse(args[2:]); err != nil {
		os.Exit(2)
	}

	var err error
	switch cmd {
	case "connect":
		err = run(workspace)
	case "ls":
		err = runList(workspace)
	case "stop", "resume":
		if *sessionID == "" {
			err = fmt.Errorf("%s needs -session <id>; `ourcli ls %s` lists them", cmd, workspace)
			break
		}
		err = postSession(workspace, *sessionID, cmd, *force)
	case "rm":
		if *sessionID == "" {
			err = fmt.Errorf("rm needs -session <id>; `ourcli ls %s` lists them", workspace)
			break
		}
		err = deleteSession(workspace, *sessionID, *force)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "\r\nourcli: %v\r\n", err)
		os.Exit(1)
	}
}

type endpoint struct {
	Transport  string `json:"transport"`
	Address    string `json:"address"`
	Credential string `json:"credential"`
	PeerPubkey string `json:"peer_pubkey"`
}

func run(workspace string) error {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		// Section 2.6: never block on a prompt when stdin is not a TTY, or
		// `ourcli connect` in a pipeline hangs forever. There is nothing to
		// prompt for yet, but the same rule applies to needing a real terminal.
		return fmt.Errorf("stdin is not a terminal")
	}

	sid := *sessionID
	reattached := false
	switch {
	case sid != "":
		// Explicit intent: join this session even if someone is already on it.
	case *forceNew:
		var err error
		if sid, err = createSession(workspace); err != nil {
			return err
		}
	default:
		var err error
		if sid, reattached, err = pickSession(workspace); err != nil {
			return err
		}
	}

	// Ask the control plane WHERE to connect rather than assuming the relay.
	// v0.1 always answers "relay", but the client having asked is what makes
	// E2E, `direct` and `tailnet` per-tenant config instead of a rewrite
	// (section 2.7).
	ep, err := negotiate(sid)
	if err != nil {
		return err
	}
	switch ep.Transport {
	case "relay":
	default:
		return fmt.Errorf("unsupported transport %q (this client only speaks relay)", ep.Transport)
	}

	return attach(fd, sid, ep, reattached)
}

func createSession(workspace string) (string, error) {
	u := strings.TrimRight(*server, "/") + "/v1/workspaces/" + neturl.PathEscape(workspace) + "/sessions"
	resp, err := http.Post(u, "application/json", nil)
	if err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusAccepted {
		return "", fmt.Errorf("create session: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("create session: bad response: %w", err)
	}
	return out.ID, nil
}

func negotiate(sid string) (endpoint, error) {
	u := strings.TrimRight(*server, "/") + "/v1/sessions/" + neturl.PathEscape(sid) + "/endpoint"
	resp, err := http.Get(u)
	if err != nil {
		return endpoint{}, fmt.Errorf("endpoint: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return endpoint{}, fmt.Errorf("endpoint: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var ep endpoint
	if err := json.Unmarshal(body, &ep); err != nil {
		return endpoint{}, fmt.Errorf("endpoint: bad response: %w", err)
	}
	return ep, nil
}

func attach(fd int, sid string, ep endpoint, reattached bool) error {
	u, err := neturl.Parse(ep.Address)
	if err != nil {
		return fmt.Errorf("parse address: %w", err)
	}
	q := u.Query()
	q.Set("credential", ep.Credential)
	q.Set("mode", *mode)
	// The size must reach the supervisor before it forks the child: a PTY
	// defaults to 0x0, and a resize sent after connect arrives too late --
	// Claude Code has already drawn its first frame. So it goes in the dial URL.
	if cols, rows, err := term.GetSize(fd); err == nil {
		q.Set("rows", strconv.Itoa(rows))
		q.Set("cols", strconv.Itoa(cols))
	}
	u.RawQuery = q.Encode()

	ws, resp, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		if resp != nil {
			b, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("attach: %s: %s", resp.Status, strings.TrimSpace(string(b)))
		}
		return fmt.Errorf("attach: %w", err)
	}
	c := proto.NewConn(ws)
	defer c.Close()

	// Raw mode disables ICANON, ECHO and ISIG so Ctrl-C, arrow keys and
	// bracketed-paste sequences are delivered as bytes to the remote process
	// instead of being interpreted locally. Without it Ctrl-C kills this client
	// rather than interrupting Claude Code's current tool call.
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("make raw: %w", err)
	}
	// Restore on every exit path: the deferred call covers normal return and
	// panics, the signal handler covers SIGTERM, and the exit paths call it
	// explicitly so their closing message prints with the terminal already sane.
	// sync.Once makes the duplicate calls harmless. Leaving raw mode set leaves
	// the user with a broken shell.
	var restoreOnce sync.Once
	restore := func() { restoreOnce.Do(func() { _ = term.Restore(fd, oldState) }) }
	defer restore()

	verb := "session"
	if reattached {
		verb = "reattached to"
	}
	fmt.Fprintf(os.Stdout, "[%s %s — Ctrl-] to detach]\r\n", verb, sid)

	fatal := make(chan os.Signal, 1)
	signal.Notify(fatal, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-fatal
		restore()
		os.Exit(1)
	}()

	// The initial size already went out in the dial URL. This tracks later
	// changes: SIGWINCH fires on a window resize, and the supervisor turns each
	// report into TIOCSWINSZ.
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	go func() {
		for range winch {
			cols, rows, err := term.GetSize(fd)
			if err != nil {
				continue
			}
			_ = c.WriteControl(proto.Control{
				Type: proto.TypeResize,
				Rows: uint16(rows),
				Cols: uint16(cols),
			})
		}
	}()

	// stdin -> remote. In raw mode every byte is forwarded, so Ctrl-C reaches
	// Claude Code and interrupts its tool call rather than killing this process
	// -- that is the intended behaviour, not a bug. Detaching therefore needs
	// its own escape sequence, the way SSH uses "~.".
	detach := make(chan struct{})
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				if idx := detachIndex(buf[:n]); idx >= 0 {
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
				return
			}
		}
	}()

	// remote -> stdout. Writing straight to the fd avoids buffering that would
	// delay a redraw; the local emulator handles the escape sequences.
	remoteDone := make(chan error, 1)
	go func() {
		for {
			mt, data, err := c.Read()
			if err != nil {
				remoteDone <- err
				return
			}
			if mt == websocket.BinaryMessage {
				if _, werr := os.Stdout.Write(data); werr != nil {
					remoteDone <- werr
					return
				}
			}
		}
	}()

	select {
	case <-detach:
		// Deliberate detach. The remote session keeps running: the supervisor
		// does not kill the child when an attachment goes away (section 2.4).
		_ = c.WriteCloseFrame(websocket.CloseGoingAway, "client detached")
		restore()
		fmt.Fprintf(os.Stdout, "\r\n[detached — reattach with: ourcli connect <workspace> -session %s]\r\n", sid)
		return nil

	case err := <-remoteDone:
		restore()
		switch {
		case err == nil || proto.IsCleanClose(err):
			// The child exited -- Ctrl-D, /exit, or a crash.
			fmt.Fprint(os.Stdout, "\r\n[session ended]\r\n")
			return nil
		default:
			// No close frame: the connection dropped. Reconnect-and-replay is
			// the hardened path in Phase 2; for now say so plainly and name the
			// session so the user can reattach by hand.
			return fmt.Errorf("connection lost (session %s is still running): %w", sid, err)
		}
	}
}
