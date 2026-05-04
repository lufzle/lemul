// Command lem attaches the local terminal to a remote Claude Code session.
//
// The user's own terminal emulator does the VT emulation; this process only
// moves bytes and reports size changes. That is what keeps fidelity: every
// escape sequence Claude Code emits reaches a real emulator untouched.
//
// Usage:
//
//	lem                                # new session in your only workspace
//	lem --workspace w1                 # new session in w1
//	lem --session <id>                 # attach to an existing session
//	lem --session <id> --viewer        # watch it, read-only
//
//	lem workspace ls | create [name] | rm <name>
//	lem session ls [--workspace w1] | rm --session <id>
//	lem orgs | invite [--role owner] | join <code>
//	lem login | logout | whoami
//
// `lem` with no verb always starts a NEW session. Reattaching is `--session`,
// explicitly, and that same flag also RESUMES one whose process has ended --
// the supervisor decides between `--resume` and `--session-id` by looking at
// the workspace volume, so one path covers reattach, resume and a cold task
// (section 12.7).
//
// Every workspace lives in an organization, and the API never assumes which --
// see orgs.go. With one organization --org is unnecessary; with more it is
// required.
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
	"github.com/lufzle/lemul/internal/proto"
	"golang.org/x/term"
)

var (
	server    *string
	org       *string
	workspace *string
	sessionID *string
	viewer    *bool
)

func usage(fs *flag.FlagSet) func() {
	return func() {
		fmt.Fprintf(os.Stderr, "usage: lem [--workspace <name>]        start a new session and attach\n")
		fmt.Fprintf(os.Stderr, "       lem --session <id> [--viewer]   attach to an existing one\n\n")
		fmt.Fprintf(os.Stderr, "       lem workspace ls\n")
		fmt.Fprintf(os.Stderr, "       lem workspace create [name]     no name generates one\n")
		fmt.Fprintf(os.Stderr, "       lem workspace rm <name>\n")
		fmt.Fprintf(os.Stderr, "       lem session ls [--workspace <name>]\n")
		fmt.Fprintf(os.Stderr, "       lem session rm --session <id>\n")
		fmt.Fprintf(os.Stderr, "       lem orgs | invite [--role owner] | join <code>\n")
		fmt.Fprintf(os.Stderr, "       lem login | logout | whoami\n\n")
		fmt.Fprintf(os.Stderr, "--session accepts any unique prefix of a session id, and resumes a\n")
		fmt.Fprintf(os.Stderr, "session whose process has ended as readily as it reattaches to one\n")
		fmt.Fprintf(os.Stderr, "still running.\n")
		fmt.Fprintf(os.Stderr, "--org names the organization; needed only if you are in more than one.\n\n")
		fs.PrintDefaults()
	}
}

// main dispatches on the first non-flag argument, and treats its absence as
// "start a session", which is what the tool is for.
func main() {
	fs := flag.NewFlagSet("lem", flag.ExitOnError)
	// LEMUL_SERVER, because every other binary in this tree defaults its flags
	// from the environment and a client that did not would make working against
	// a deployed control plane mean repeating -server on every single command.
	//
	// The default stays loopback: `lem` with nothing set should talk to the
	// stack in the README's local-run section, not to whatever was exported in
	// a shell three days ago.
	server = fs.String("server", envOr("LEMUL_SERVER", "http://localhost:9000"),
		"control plane base URL (LEMUL_SERVER)")
	org = fs.String("org", "", "organization slug; required when you belong to more than one")
	workspace = fs.String("workspace", "", "workspace to work in; empty uses your only one, and refuses if you have several")
	sessionID = fs.String("session", "", "attach to this session (any unique prefix)")
	viewer = fs.Bool("viewer", false, "attach read-only; input is dropped")
	role := fs.String("role", "user", "invite: role the redeemer gets (owner|user)")

	fs.Usage = usage(fs)

	// Flags are accepted on BOTH sides of the verb.
	//
	// flag.Parse stops at the first non-flag argument, which is what gives the
	// subcommand for free -- and the remainder is parsed with the same FlagSet,
	// so `lem --org acme session ls` and `lem session ls --org acme` mean the
	// same thing. Both get typed: --org reads as a global modifier and lands in
	// front, while --session names an argument of the verb and lands behind it.
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	args := fs.Args()

	// No verb: start a session. This is the whole point of the command, so it
	// is what you get for typing its name.
	if len(args) == 0 {
		fail(run())
		return
	}

	rest := func() []string { return parseInterspersed(fs, args[1:]) }

	switch args[0] {
	case "login":
		rest()
		fail(runLogin())
	case "logout":
		rest()
		fail(runLogout())
	case "whoami":
		rest()
		fmt.Println(authStatus())
	case "orgs":
		rest()
		fail(runOrgList())
	case "invite":
		rest()
		fail(runInvite(*role))
	case "join":
		// Takes a positional that is a CODE rather than a name.
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			fmt.Fprintln(os.Stderr, "usage: lem join <invite-code>")
			os.Exit(2)
		}
		code := args[1]
		if err := fs.Parse(args[2:]); err != nil {
			os.Exit(2)
		}
		fail(runJoin(code))
	case "workspace":
		fail(runWorkspace(rest()))
	case "session":
		fail(runSessionVerb(rest()))
	default:
		fs.Usage()
		os.Exit(2)
	}
}

// parseInterspersed parses flags wherever they appear among positionals and
// returns the positionals in order.
//
// flag.Parse STOPS at the first non-flag argument. That is exactly what gives
// main its subcommand for free, but applied a second time to a verb's own
// arguments it stops caring about flags the moment a positional appears -- so
// the comment above promising flags on both sides of the verb was only half
// true, and the failing half was the natural one. Measured 2026-08-03 against
// the built binary:
//
//	lem session rm --session abc   ->  "session rm needs --session <id>"
//	lem --session abc session rm   ->  works
//
// The first form holds the flag it says it is missing, because "rm" ended the
// parse and --session was never read. Same mechanism, worse outcome, on
// `lem workspace create --help`: it created a workspace CALLED --help rather
// than printing usage, and no `lem workspace rm` could then name it.
//
// A lone -- still terminates flags, so a workspace whose name begins with a
// dash stays addressable even though validWorkspaceName now refuses to mint
// another one. That falls out of flag.Parse rather than being handled here: it
// consumes -- and stops, and the loop then takes what follows as a positional.
// An explicit check for it was written first and DELETED, because mutating it
// away changed no test -- it could only matter for a second positional after
// --, and no verb takes one.
func parseInterspersed(fs *flag.FlagSet, args []string) []string {
	var positional []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			os.Exit(2)
		}
		remaining := fs.Args()
		if len(remaining) == 0 {
			break
		}
		positional = append(positional, remaining[0])
		args = remaining[1:]
	}
	return positional
}

func fail(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "\r\nlem: %v\r\n", err)
		os.Exit(1)
	}
}

// runWorkspace implements the `workspace` noun.
func runWorkspace(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: lem workspace ls | create [name] | rm <name>")
	}
	switch args[0] {
	case "ls":
		return runWorkspaceList()
	case "create":
		// No name asks the SERVER to generate one, which is what the explicit
		// null in the request body means. Generating here would put the word
		// lists in two places and let them drift.
		var name *string
		if len(args) > 1 {
			name = &args[1]
		}
		return runWorkspaceCreate(name)
	case "rm":
		if len(args) < 2 {
			return fmt.Errorf("usage: lem workspace rm <name>")
		}
		return runWorkspaceRemove(args[1])
	default:
		return fmt.Errorf("unknown workspace verb %q; try ls, create or rm", args[0])
	}
}

// runSessionVerb implements the `session` noun.
func runSessionVerb(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: lem session ls [--workspace <name>] | rm --session <id>")
	}
	switch args[0] {
	case "ls":
		return runSessionList()
	case "rm":
		if *sessionID == "" {
			return fmt.Errorf("session rm needs --session <id>; `lem session ls` lists them")
		}
		return deleteSession(*sessionID)
	default:
		return fmt.Errorf("unknown session verb %q; try ls or rm", args[0])
	}
}

type endpoint struct {
	Transport  string `json:"transport"`
	Address    string `json:"address"`
	Credential string `json:"credential"`
	Mode       string `json:"mode"`
	PeerPubkey string `json:"peer_pubkey"`
}

// run starts or joins a session and attaches to it.
//
//	--session given  -> that session; reattached if running, resumed if not
//	otherwise        -> a NEW session in --workspace, or in your only workspace
//
// Always a new session when none is named, which is the change from Phase 1's
// reattach-by-default. Opening a second terminal should give a second session,
// which is what the shared filesystem is for -- and picking "some idle session
// in the workspace" for the user could hand them a colleague's conversation,
// which is the client half of section 2.5's owner-scoped attach.
func run() error {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		// Section 2.6: never block on a prompt when stdin is not a TTY, and
		// never pretend a pipe is a terminal.
		return fmt.Errorf("stdin is not a terminal")
	}

	mode := "control"
	if *viewer {
		mode = "viewer"
	}

	sid, wsName := *sessionID, *workspace
	if sid == "" {
		var err error
		if wsName == "" {
			if wsName, err = currentWorkspace(); err != nil {
				return err
			}
		}
		if sid, err = createSession(wsName); err != nil {
			return err
		}
	} else {
		var err error
		if sid, wsName, err = resolveSession(sid); err != nil {
			return err
		}
	}

	// Ask the control plane WHERE to connect rather than assuming the relay.
	// v0.1 always answers "relay", but the client having asked is what makes
	// E2E, `direct` and `tailnet` per-tenant config instead of a rewrite
	// (section 2.7).
	ep, err := negotiate(sid, mode)
	if err != nil {
		return err
	}
	// The credential carries the mode, so what came back is what the relay will
	// enforce -- not what we asked for.
	if ep.Mode != "" && ep.Mode != mode {
		fmt.Fprintf(os.Stderr, "lem: asked for %s, granted %s\r\n", mode, ep.Mode)
	}
	if ep.Transport != "relay" {
		return fmt.Errorf("unsupported transport %q (this client only speaks relay)", ep.Transport)
	}
	return attach(fd, wsName, sid, ep, *sessionID != "")
}

// createSession asks for a session, which starts the workspace task if it is
// not up (section 2.6).
//
// It BLOCKS for as long as that takes, and since Phase 6 it is no longer silent
// about it: the workspace event stream is tailed for the duration, so a cold
// start reads as "starting workspace w1" rather than as 25 s of nothing. The
// request itself is unchanged -- the caller still needs a session id back, and
// there is nothing useful to hand them before the machine exists.
func createSession(ws string) (string, error) {
	u, err := orgURL("/workspaces/" + neturl.PathEscape(ws) + "/sessions")
	if err != nil {
		return "", err
	}
	defer watchWorkspace(ws)()
	resp, err := request(http.MethodPost, u, nil)
	if err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		// The workspace has to exist first: implicit creation is gone, so a
		// typo cannot manufacture one. Name the verb that would.
		return "", fmt.Errorf("workspace %q does not exist here\n"+
			"       create it with: lem %sworkspace create %s", ws, orgArg(), ws)
	}
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

// negotiate asks where to connect, and for which mode.
//
// Mode is declared HERE rather than on the attach URL because it travels inside
// the signed credential: the relay enforces what the control plane authorised
// instead of trusting the connecting client to describe itself.
func negotiate(sid, mode string) (endpoint, error) {
	u, err := orgURL("/sessions/" + neturl.PathEscape(sid) + "/endpoint")
	if err != nil {
		return endpoint{}, err
	}
	if mode != "" {
		u += "?mode=" + neturl.QueryEscape(mode)
	}
	resp, err := request(http.MethodGet, u, nil)
	if err != nil {
		return endpoint{}, fmt.Errorf("endpoint: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusForbidden {
		// Section 2.5: a session belongs to whoever started it. Pass the
		// server's sentence through -- it already names --viewer.
		return endpoint{}, fmt.Errorf("%s", strings.TrimSpace(string(body)))
	}
	if resp.StatusCode != http.StatusOK {
		return endpoint{}, fmt.Errorf("endpoint: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var ep endpoint
	if err := json.Unmarshal(body, &ep); err != nil {
		return endpoint{}, fmt.Errorf("endpoint: bad response: %w", err)
	}
	return ep, nil
}

func attach(fd int, ws, sid string, ep endpoint, rejoined bool) error {
	u, err := neturl.Parse(ep.Address)
	if err != nil {
		return fmt.Errorf("parse address: %w", err)
	}
	q := u.Query()
	q.Set("credential", ep.Credential)
	// The size must reach the supervisor before it forks the child: a PTY
	// defaults to 0x0, and a resize sent after connect arrives too late --
	// Claude Code has already drawn its first frame. So it goes in the dial URL.
	if cols, rows, err := term.GetSize(fd); err == nil {
		q.Set("rows", strconv.Itoa(rows))
		q.Set("cols", strconv.Itoa(cols))
	}
	u.RawQuery = q.Encode()

	wsConn, resp, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if resp != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if err != nil {
		if resp != nil {
			b, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("attach: %s: %s", resp.Status, strings.TrimSpace(string(b)))
		}
		return fmt.Errorf("attach: %w", err)
	}
	c := proto.NewConn(wsConn)
	defer func() { _ = c.Close() }()

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
	// sync.Once makes the duplicate calls harmless.
	var restoreOnce sync.Once
	restore := func() { restoreOnce.Do(func() { _ = term.Restore(fd, oldState) }) }
	defer restore()

	verb := "session"
	if rejoined {
		// Deliberately vague between reattached and resumed: only the
		// supervisor knows which happened, because only it can see whether a
		// transcript was on the volume (section 12.7).
		verb = "joined"
	}
	if ep.Mode == "viewer" {
		verb = "watching"
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
		fmt.Fprintf(os.Stdout, "\r\n[detached — rejoin with: lem %s--session %s]\r\n",
			orgArg(), shortID(sid))
		return nil

	case err := <-remoteDone:
		restore()
		switch {
		case err == nil || proto.IsCleanClose(err):
			// The child exited -- Ctrl-D, /exit, or a crash. The conversation
			// is still on the volume, so --session brings it back.
			fmt.Fprintf(os.Stdout, "\r\n[session ended — resume with: lem %s--session %s]\r\n",
				orgArg(), shortID(sid))
			return nil
		default:
			// No close frame: the connection dropped. Reconnect-and-replay is a
			// later phase; for now say so plainly and name the session.
			return fmt.Errorf("connection lost (session %s is still running): %w", sid, err)
		}
	}
}

// shortID trims a session id to a prefix long enough to stay unique in
// practice, because that is what --session accepts and what a person retypes.
// Twelve hex characters of a UUID is far past collision within one workspace.
func shortID(sid string) string {
	if len(sid) > 12 {
		return sid[:12]
	}
	return sid
}

// envOr is the same one-liner cmd/runner and cmd/supervisor use, rather than a
// shared helper: it is three lines, and a package to hold it would be imported
// by more things than it saves.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
