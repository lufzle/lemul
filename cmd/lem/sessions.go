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
	Workspace string `json:"workspace"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
	OwnerName string `json:"owner_name"`
	// Mine says whether this session is the caller's. Only their own may be
	// driven; anything else is watchable at best (section 2.5), and the server
	// reports it because nothing else tells a client who it is.
	Mine      bool   `json:"mine"`
	Attachers int    `json:"attachers"`
	Rows      uint16 `json:"rows"`
	Cols      uint16 `json:"cols"`
}

// listSessions returns what the caller may see, newest first: everything in the
// organization when no workspace is named, or one workspace's when one is.
//
// It never starts a workspace, so it is safe to call before deciding anything.
func listSessions(ws string) ([]sessionDoc, error) {
	path := "/sessions"
	if ws != "" {
		path = "/workspaces/" + neturl.PathEscape(ws) + "/sessions"
	}
	u, err := orgURL(path)
	if err != nil {
		return nil, err
	}
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

// runSessionList implements `lem session ls [--workspace <name>]`.
func runSessionList() error {
	sessions, err := listSessions(*workspace)
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		if *workspace != "" {
			fmt.Printf("no sessions in workspace %q\n", *workspace)
		} else {
			fmt.Println("no sessions")
		}
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SESSION\tWORKSPACE\tSTATUS\tCLIENTS\tSIZE\tWHOSE\tCREATED")
	for _, s := range sessions {
		size := "-"
		if s.Cols > 0 {
			size = fmt.Sprintf("%dx%d", s.Cols, s.Rows)
		}
		// "yours" rather than an owner column of raw uuids: what a reader needs
		// is whether they can drive it, and an email when somebody else's.
		whose := "yours"
		if !s.Mine {
			whose = s.OwnerName
			if whose == "" {
				whose = "another user"
			}
		}
		ws := s.Workspace
		if ws == "" {
			ws = *workspace
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
			shortID(s.ID), ws, s.Status, s.Attachers, size, whose, s.CreatedAt)
	}
	return w.Flush()
}

// resolveSession turns what the user typed into a session id and the workspace
// it lives in, accepting a unique prefix.
//
// Session ids are UUIDs because Claude Code's --session-id demands one and we
// adopted its format so ours IS the conversation id (section 12.7). That is the
// right trade, but it leaves 36 characters to retype from a detach message, so
// a prefix matching exactly one session is enough.
//
// An ambiguous prefix is an error rather than a guess: the sessions in an
// organization are different conversations, and picking one would at best waste
// the user's time and at worst attach them to the wrong agent run.
//
// Resolved across the whole organization rather than one workspace, because the
// endpoint that acts on a session is org-scoped -- demanding --workspace purely
// to look an id up would ask for something the API does not need.
func resolveSession(want string) (id, ws string, err error) {
	sessions, err := listSessions(*workspace)
	if err != nil {
		return "", "", err
	}
	var matches []sessionDoc
	for _, s := range sessions {
		if s.ID == want {
			return s.ID, s.Workspace, nil // exact match always wins over any prefix
		}
		if strings.HasPrefix(s.ID, want) {
			matches = append(matches, s)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0].ID, matches[0].Workspace, nil
	case 0:
		return "", "", fmt.Errorf("no session you can reach matches %q", want)
	default:
		ids := make([]string, 0, len(matches))
		for _, m := range matches {
			ids = append(ids, shortID(m.ID)+" in "+m.Workspace)
		}
		return "", "", fmt.Errorf("%q matches %d sessions: %s",
			want, len(matches), strings.Join(ids, ", "))
	}
}

// deleteSession ends a session and drops its conversation.
//
// Unlike detaching, this is not recoverable -- the transcript IS the session's
// memory. There is no --force here: the server refuses a running session, and
// the way to end one is to end it from inside, with Ctrl-D or /exit.
func deleteSession(want string) error {
	sid, _, err := resolveSession(want)
	if err != nil {
		return err
	}
	u, err := orgURL("/sessions/" + neturl.PathEscape(sid))
	if err != nil {
		return err
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
	fmt.Printf("session %s deleted; its conversation is gone\n", shortID(sid))
	return nil
}
