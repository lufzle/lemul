package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"strings"
	"text/tabwriter"
)

// The `workspace` noun.
//
// Nothing here takes --force, on either verb. The server refuses a delete that
// would drop conversations, and the way past that is to delete the sessions
// first -- a flag that skips the question is what turns a mis-typed name into
// lost work, and the console has a confirmation dialog to justify one where
// this does not.

type workspaceDoc struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	// AccessScope is who may use it: owner | members | org.
	AccessScope string `json:"access_scope"`
	// Connected is whether a task is holding a tunnel right now, which can
	// disagree with Status: Status is what the control plane last wrote down.
	Connected bool `json:"connected"`
	Sessions  int  `json:"sessions"`
	Running   int  `json:"running"`
	// LastError is why the last placement failed. Since placement happens on
	// create and in the background, this is the only thing that reports a
	// workspace that never came up -- the request that would have carried it is
	// long since answered.
	LastError string `json:"last_error"`
	OwnerName string `json:"owner_name"`
	CreatedAt string `json:"created_at"`
}

func listWorkspaces() ([]workspaceDoc, error) {
	u, err := orgURL("/workspaces")
	if err != nil {
		return nil, err
	}
	resp, err := request(http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list workspaces: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var out struct {
		Workspaces []workspaceDoc `json:"workspaces"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("list workspaces: bad response: %w", err)
	}
	return out.Workspaces, nil
}

// runWorkspaceList implements `lem workspace ls`.
func runWorkspaceList() error {
	wss, err := listWorkspaces()
	if err != nil {
		return err
	}
	if len(wss) == 0 {
		fmt.Println("no workspaces here yet — create one with: lem workspace create")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "WORKSPACE\tSTATUS\tSESSIONS\tACCESS\tOWNER\tCREATED")
	for _, ws := range wss {
		// The record's status and the live tunnel can disagree, and that
		// disagreement is the thing worth seeing -- a workspace recorded active
		// whose task died is the state an operator most needs to notice.
		status := ws.Status
		if ws.Status == "active" && !ws.Connected {
			status = "active (no task)"
		}
		sessions := fmt.Sprintf("%d", ws.Sessions)
		if ws.Running > 0 {
			sessions = fmt.Sprintf("%d (%d running)", ws.Sessions, ws.Running)
		}
		// Who may use it, which is now the more useful column than who owns it:
		// on a shared machine the question a user is asking is whether their
		// colleagues are in here too.
		if ws.LastError != "" {
			// Marked in the column and spelled out under the table. A message
			// that can name an ARN would wreck the alignment of every other
			// row, and the thing a reader needs first is WHICH workspace.
			status += " (failed)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			ws.ID, status, sessions, ws.AccessScope, ws.OwnerName, ws.CreatedAt)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	// Below the table rather than beside it: placement is a background
	// operation now, so this is the ONLY place a failed one surfaces. Left out,
	// a workspace that never started reads as one that is merely stopped.
	for _, ws := range wss {
		if ws.LastError != "" {
			fmt.Printf("\n%s could not start: %s\n", ws.ID, ws.LastError)
		}
	}
	return nil
}

// runWorkspaceCreate implements `lem workspace create [name]`.
//
// A nil name sends an explicit JSON null, which is what asks the server to
// generate one. Absent and null are different on purpose -- absent is "you
// forgot", null is "you pick" -- so this cannot simply omit the field.
func runWorkspaceCreate(name *string) error {
	u, err := orgURL("/workspaces")
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"name": name})
	if err != nil {
		return err
	}
	resp, err := request(http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create workspace: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("create workspace: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var out workspaceDoc
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("create workspace: bad response: %w", err)
	}
	// The server answers 202 and places the task in the background, so the
	// record exists and the machine does not yet. Saying so is the point: the
	// next command is a session, and a user who is told the workspace is
	// "created" and then waits 25 s has been told the wrong thing.
	fmt.Printf("workspace %s created, and its machine is starting\n\n"+
		"start a session in it with: lem %s--workspace %s\n",
		out.ID, orgArg(), out.ID)
	return nil
}

// runWorkspaceRemove implements `lem workspace rm <name>`.
func runWorkspaceRemove(name string) error {
	u, err := orgURL("/workspaces/" + neturl.PathEscape(name))
	if err != nil {
		return err
	}
	resp, err := request(http.MethodDelete, u, nil)
	if err != nil {
		return fmt.Errorf("delete workspace: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		// The server's sentence is the useful part: it says how many sessions
		// are in the way, which is what the reader has to act on.
		return fmt.Errorf("%s", strings.TrimSpace(string(raw)))
	}
	fmt.Printf("workspace %s deleted\n", name)
	return nil
}

// currentWorkspace resolves the workspace a bare `lem` acts in.
//
// The same rule as currentOrg, and deliberately the same shape:
//
//	--workspace given      use it
//	exactly one to choose  use it
//	more than one          refuse, and say which
//	none                   refuse, and name the verb that makes one
//
// It replaced "your personal workspace, created on demand". That default was
// the client conjuring a Fargate task out of a command with no arguments, which
// stopped being defensible once a workspace is a shared machine somebody pays
// for and an organization can say who may create one. Creating is a verb now,
// and it is the user's, not a side effect of connecting.
//
// Refusing on ambiguity rather than picking is the same reasoning as the
// organization rule: a wrong guess is silent, and the person who finds out is
// whoever was sharing the workspace it guessed.
func currentWorkspace() (string, error) {
	all, err := listWorkspaces()
	if err != nil {
		return "", err
	}
	switch len(all) {
	case 1:
		return all[0].ID, nil
	case 0:
		return "", fmt.Errorf("you have no workspaces in this organization\n"+
			"       create one with: lem %sworkspace create <name>", orgArg())
	default:
		return "", fmt.Errorf("you can reach %d workspaces, so --workspace is required:\n%s",
			len(all), workspaceLines(all))
	}
}

func workspaceLines(all []workspaceDoc) string {
	var b strings.Builder
	for _, ws := range all {
		fmt.Fprintf(&b, "  --workspace %-24s %s\n", ws.ID, ws.Status)
	}
	return strings.TrimRight(b.String(), "\n")
}
