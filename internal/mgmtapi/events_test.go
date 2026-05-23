package mgmtapi

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Section 2.6's workspace event stream, which existed in the API surface from
// Phase 1 and had never been built. Its absence is why a cold start looked like
// a hang: there was no channel on which "the machine is booting" could be said.

// readEvents opens the stream and returns a channel of decoded events, plus a
// stop function. Best effort by design -- a stream is ended by the reader, so a
// test has to be the thing that ends it.
func readEvents(t *testing.T, base, org, ws, tok string) (<-chan workspaceEvent, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		base+"/v1/orgs/"+org+"/workspaces/"+ws+"/events", nil)
	if err != nil {
		cancel()
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("events: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		cancel()
		_ = resp.Body.Close()
		t.Fatalf("events: %s", resp.Status)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		cancel()
		_ = resp.Body.Close()
		t.Fatalf("content type is %q, not an event stream", ct)
	}

	out := make(chan workspaceEvent, 16)
	go func() {
		defer close(out)
		defer func() { _ = resp.Body.Close() }()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			data, ok := strings.CutPrefix(sc.Text(), "data: ")
			if !ok {
				continue
			}
			var ev workspaceEvent
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				return
			}
			out <- ev
		}
	}()
	return out, func() { cancel() }
}

func nextEvent(t *testing.T, ch <-chan workspaceEvent, why string) workspaceEvent {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatalf("the stream ended while waiting for %s", why)
		}
		return ev
	case <-time.After(10 * time.Second):
		t.Fatalf("no event for %s", why)
		return workspaceEvent{}
	}
}

// The FIRST event is a snapshot, and without it the endpoint is useless for the
// case it has to answer fastest.
//
// A client connecting to a workspace that is already active would otherwise
// wait for a transition that has already happened -- so `lem` would print
// "starting…" for a workspace that is up, or wait forever to be told it is.
// Sending current state on connect is what makes "nothing is happening" a
// legible answer rather than silence.
func TestTheEventStreamOpensWithASnapshot(t *testing.T) {
	s, h, iss := authedServerFull(t)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	tok, org := signUp(t, h, iss, "alice")
	ws := createWS(t, h, tok, org, `{"name":"w1"}`)
	// The create-time placement has no runner to reach, so it settles at
	// stopped with a reason -- a determinate state to take a snapshot of.
	s.WaitForPlacements()

	ch, stop := readEvents(t, srv.URL, org, ws, tok)
	defer stop()

	snap := nextEvent(t, ch, "the opening snapshot")
	if snap.Status != WorkspaceStopped {
		t.Errorf("snapshot status is %q, want %q", snap.Status, WorkspaceStopped)
	}
	if snap.LastError == "" {
		t.Error("the snapshot omits the reason the last placement failed, " +
			"which is the one thing a watcher cannot get anywhere else")
	}
	if snap.Connected {
		t.Error("the snapshot reports a task holding a tunnel; none was ever placed")
	}
}

// A change is delivered, and only a change is delivered.
//
// Both halves are load-bearing. Without the first the stream is a heartbeat with
// no content; without the second it is a 2 Hz firehose of identical documents,
// which a console would re-render on every one -- and a client cannot tell a
// repeat from a real transition back to the same state.
func TestTheEventStreamReportsChangesAndNothingElse(t *testing.T) {
	s, h, iss := authedServerFull(t)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	tok, org := signUp(t, h, iss, "alice")
	ws := createWS(t, h, tok, org, `{"name":"w1"}`)
	s.WaitForPlacements()

	ch, stop := readEvents(t, srv.URL, org, ws, tok)
	defer stop()
	_ = nextEvent(t, ch, "the opening snapshot")

	// Several poll intervals with nothing changing. Silence is the assertion.
	select {
	case ev := <-ch:
		t.Fatalf("a second event arrived with nothing changed: %+v", ev)
	case <-time.After(4 * eventsPollInterval):
	}

	// Now the transition a placement would produce: the record goes active and
	// a task holds a tunnel.
	sc := scopeForOrg(t, s, org)
	rec := workspaceRow(t, s, sc, ws)
	if err := s.setWorkspaceStatus(context.Background(), sc, rec.ID,
		WorkspaceActive, "arn:aws:ecs:task/live"); err != nil {
		t.Fatalf("setWorkspaceStatus: %v", err)
	}
	s.reg.AddWorkspace(fakeTunnel(t, sc.TenantID, ws))

	got := nextEvent(t, ch, "the workspace going active")
	if got.Status != WorkspaceActive {
		t.Errorf("status is %q after the record went active, want %q", got.Status, WorkspaceActive)
	}
	if !got.Connected {
		t.Error("connected is false with a task holding a tunnel")
	}
	if got.LastError != "" {
		t.Errorf("the previous failure survived a successful status write: %q", got.LastError)
	}
}

// The stream is behind the same 404 as everything else with a {wid} in it.
//
// Worth its own test because it is a NEW route, and section 2.5's rule is that
// a caller with no standing cannot tell a workspace they may not see from one
// that does not exist -- an endpoint that leaks it would be a way to probe which
// names are taken.
func TestTheEventStreamIsNotReachableByAStranger(t *testing.T) {
	h, iss := authedServer(t)
	alice, aliceOrg := signUp(t, h, iss, "alice")
	bob, _ := signUp(t, h, iss, "bob")
	invite(t, h, alice, aliceOrg, bob, "user")
	ws := createWS(t, h, alice, aliceOrg, `{"name":"secret-project"}`)

	code, _ := do(t, h, http.MethodGet,
		"/v1/orgs/"+aliceOrg+"/workspaces/"+ws+"/events", bob, "")
	if code != http.StatusNotFound {
		t.Errorf("a non-member watching a workspace got %d, want 404", code)
	}
}
