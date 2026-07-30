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
	resp, err := http.Get(u)
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
