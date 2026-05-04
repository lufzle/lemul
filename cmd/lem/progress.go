package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	neturl "net/url"
	"os"
	"strings"
	"sync"
)

// Progress during a cold start.
//
// The symptom this removes: `lem` in a workspace whose task is not up posts a
// session, and the control plane blocks inside ensureWorkspace until a Fargate
// task dials in -- 20-60 s during which this process printed absolutely nothing.
// A blank terminal is indistinguishable from a hang, and the user's next move is
// Ctrl-C, which abandons a placement that was going to succeed.
//
// Nothing here changes what the control plane does. It reads the workspace event
// stream (section 2.6) while the session request is in flight, and says out loud
// what the record already said. That is deliberate: a client inferring progress
// from its own elapsed time would be inventing a story about a service it cannot
// see, and would be wrong exactly when the placement failed.

// workspaceEvent mirrors the server's event document. Only the fields a user
// needs to be told about -- the rest of the record is not news.
type workspaceEvent struct {
	Status    string `json:"status"`
	Connected bool   `json:"connected"`
	LastError string `json:"last_error"`
}

// watchWorkspace narrates a workspace's status to stderr until the returned
// stop function is called.
//
// STDERR rather than stdout, and it matters: `lem` hands the terminal to a PTY
// stream a moment later, so progress has to be something a user reads and a
// pipe does not have to carry.
//
// Best effort throughout. A control plane too old to serve the stream, a
// network that drops it, an unparseable line -- every one of them means the user
// waits without commentary, which is exactly where they were before this
// existed. Nothing about starting a session is allowed to depend on it.
func watchWorkspace(ws string) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tailWorkspace(ctx, ws)
	}()
	// Waited on rather than merely cancelled, so a final line cannot land in the
	// middle of the raw-mode terminal the caller is about to set up.
	return func() {
		cancel()
		wg.Wait()
	}
}

func tailWorkspace(ctx context.Context, ws string) {
	u, err := orgURL("/workspaces/" + neturl.PathEscape(ws) + "/events")
	if err != nil {
		return
	}
	resp, err := requestCtx(ctx, http.MethodGet, u, nil)
	if err != nil {
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return
	}

	first := true
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok {
			continue // an event: name, a heartbeat comment, or the blank separator
		}
		var ev workspaceEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		say, done := narrate(ws, ev, first)
		first = false
		if say != "" {
			fmt.Fprintln(os.Stderr, say)
		}
		if done {
			return
		}
	}
}

// narrate turns one event into what to tell the user, and whether there is
// anything left to watch for.
//
// A pure function because the ONE rule in it is easy to get wrong and invisible
// when it is: the first event is a SNAPSHOT of state that predates this
// command, and every one after it is a change this command caused.
//
//   - A snapshot saying active is a workspace already up. Nothing is narrated,
//     which is what keeps the warm path silent.
//   - A snapshot carrying `last_error` is the PREVIOUS attempt's, and the one
//     this command is about to replace. Reporting it would announce a failure
//     that has not happened, on the run most likely to succeed -- and it would
//     do so to a user who just asked for a session and is about to get one.
func narrate(ws string, ev workspaceEvent, first bool) (say string, done bool) {
	up := ev.Status == "active" && ev.Connected
	switch {
	case first && up:
		return "", true
	case first:
		return fmt.Sprintf("lem: starting workspace %s — a cold start takes 20-60 s…", ws), false
	case ev.LastError != "":
		return fmt.Sprintf("lem: starting %s failed: %s", ws, ev.LastError), true
	case up:
		return fmt.Sprintf("lem: workspace %s is up", ws), true
	}
	return "", false
}
