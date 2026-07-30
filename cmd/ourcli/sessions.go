package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"strings"
	"text/tabwriter"
)

type sessionDoc struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
	Attachers int    `json:"attachers"`
	Rows      uint16 `json:"rows"`
	Cols      uint16 `json:"cols"`
}

// listSessions returns a workspace's sessions, newest first. It never starts
// the workspace, so it is safe to call before deciding what to do.
func listSessions(workspace string) ([]sessionDoc, error) {
	u := strings.TrimRight(*server, "/") + "/v1/workspaces/" + neturl.PathEscape(workspace) + "/sessions"
	resp, err := request(http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list sessions: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var out struct {
		Sessions []sessionDoc `json:"sessions"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("list sessions: bad response: %w", err)
	}
	return out.Sessions, nil
}

// runList implements `ourcli ls <workspace>`.
func runList(workspace string) error {
	sessions, err := listSessions(workspace)
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		fmt.Printf("no sessions in workspace %q\n", workspace)
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SESSION\tSTATUS\tCLIENTS\tSIZE\tCREATED")
	for _, s := range sessions {
		size := "-"
		if s.Cols > 0 {
			size = fmt.Sprintf("%dx%d", s.Cols, s.Rows)
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n", s.ID, s.Status, s.Attachers, size, s.CreatedAt)
	}
	return w.Flush()
}

// resolveSession turns what the user typed into a session id, accepting a unique
// prefix.
//
// Session ids are UUIDs because Claude Code's --session-id demands one and we
// adopted its format so that ours IS the conversation id. That is the right
// trade, but it leaves the user with 36 characters to retype from a detach
// message, so a prefix that matches exactly one session is enough.
//
// An ambiguous prefix is an error rather than a guess: the sessions in a
// workspace are different conversations, and picking one for the user would at
// best waste their time and at worst stop the wrong agent run.
func resolveSession(workspace, want string) (string, error) {
	sessions, err := listSessions(workspace)
	if err != nil {
		// Fall back to using it verbatim. A control plane that cannot list should
		// still let an operator act on an id they already have in full.
		return want, nil
	}
	var matches []string
	for _, s := range sessions {
		if s.ID == want {
			return want, nil // exact match always wins over any prefix
		}
		if strings.HasPrefix(s.ID, want) {
			matches = append(matches, s.ID)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("no session in workspace %q matches %q", workspace, want)
	default:
		return "", fmt.Errorf("%q matches %d sessions in workspace %q: %s",
			want, len(matches), workspace, strings.Join(matches, ", "))
	}
}

// pickSession decides what `ourcli connect <workspace>` should attach to.
//
//	an unattended running session  -> reattach to the newest one
//	otherwise                      -> create a new session
//
// Reattaching only when nobody else is attached is what makes this safe to do by
// default. Both attachers write the same PTY stdin (section 2.5), so silently
// joining an in-use session would make two people co-drive one Claude Code
// without either having asked for it. Opening a second terminal while working
// therefore still gives a second session, which is the behaviour the shared
// filesystem exists for -- and losing a session id no longer strands it, which
// is the problem this solves.
//
// Explicit intent always wins: -session joins a specific session even if it is
// already attached, and -new always forks.
//
// SECURITY, once there is a user model (Phase 3): this must filter to sessions
// the CALLER OWNS. A session is user-specific (section 2.3) and a workspace can
// be shared at team or org scope (section 2.5), so "any idle session in the
// workspace" would drop a user into a colleague's live conversation. It is safe
// today only because Phase 1 has no auth at all -- and note that the explicit
// path is no safer, since /v1/sessions/{sid}/endpoint will mint an attach
// credential for any session id to anyone who can reach the control plane.
func pickSession(workspace string) (sid string, reattached bool, err error) {
	sessions, err := listSessions(workspace)
	if err != nil {
		// Listing is a convenience, not a precondition. A control plane that
		// cannot list should still be able to start a session.
		sessions = nil
	}
	for _, s := range sessions { // already newest-first
		if s.Status == "running" && s.Attachers == 0 {
			return s.ID, true, nil
		}
	}
	sid, err = createSession(workspace)
	return sid, false, err
}

// postSession drives one lifecycle verb against a session (section 2.4). The
// process axis only: stopping a session leaves the workspace task running, since
// a sibling session may still be working in it.
func postSession(workspace, want, verb string, force bool) error {
	sid, err := resolveSession(workspace, want)
	if err != nil {
		return err
	}
	u := strings.TrimRight(*server, "/") + "/v1/sessions/" + neturl.PathEscape(sid) + "/" + verb
	if force {
		u += "?force=1"
	}
	resp, err := request(http.MethodPost, u, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", verb, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s: %s: %s", verb, resp.Status, strings.TrimSpace(string(body)))
	}
	switch verb {
	case "stop":
		fmt.Printf("session %s stopped (resume with: ourcli resume %s -session %s)\n", sid, workspace, sid)
	case "resume":
		fmt.Printf("session %s resumed (attach with: ourcli connect %s -session %s)\n", sid, workspace, sid)
	}
	return nil
}

// deleteSession ends a session and drops its conversation. Unlike stop, this is
// not recoverable -- the transcript is the session's memory.
func deleteSession(workspace, want string, force bool) error {
	sid, err := resolveSession(workspace, want)
	if err != nil {
		return err
	}
	u := strings.TrimRight(*server, "/") + "/v1/sessions/" + neturl.PathEscape(sid)
	if force {
		u += "?force=1"
	}
	resp, err := request(http.MethodDelete, u, nil)
	if err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("delete: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	fmt.Printf("session %s deleted; its conversation is gone\n", sid)
	return nil
}
