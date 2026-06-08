package e2e_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// Phase 6 increment 4: placement on create, as a background operation.
//
// Driven end to end because the property is about two things happening at
// different times -- a response, and a machine -- and a unit test of either one
// can be perfectly correct while the pair is broken.

// Creating a workspace starts its machine, and the caller is not made to wait
// for it.
//
// Both halves matter, and neither is visible from the other end. If create
// blocked, the response would be honest and the user would sit through a cold
// start for a verb that is supposed to be bookkeeping. If it returned without
// placing, the response would be instant and the first person to ask for a
// session would pay for the machine to boot -- which on a SHARED workspace
// means one member paying for everybody else's (section 12.10).
func TestCreatingAWorkspacePlacesItsTask(t *testing.T) {
	s := newStack(t, "sh", "-c", `exec cat`)

	start := time.Now()
	code, body := s.postJSON("/workspaces", `{"name":"w1"}`)
	elapsed := time.Since(start)
	if code != http.StatusAccepted {
		t.Fatalf("create workspace: %d: %s", code, body)
	}
	// The local driver execs a real supervisor binary, so a create that waited
	// for the tunnel would take seconds rather than milliseconds. Generous
	// enough not to be a timing test, tight enough to fail a blocking create.
	if elapsed > 2*time.Second {
		t.Errorf("create took %v; it is waiting for the placement it started", elapsed)
	}

	var doc workspaceDocT
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("workspace: %v (%s)", err, body)
	}
	if doc.Status != "starting" {
		t.Errorf("create reported status %q, want %q -- a client reading this "+
			"as ready posts a session and blocks for the cold start", doc.Status, "starting")
	}

	// And the machine really does come up, with nobody having asked again.
	up := s.awaitWorkspaceStatus("w1", "active", 30*time.Second)
	if !up.Connected {
		t.Error("the workspace is recorded active with no task holding a tunnel")
	}
	if up.LastError != "" {
		t.Errorf("a successful placement left an error on the record: %q", up.LastError)
	}
	if n := s.supervisorCount(); n != 1 {
		t.Errorf("driver holds %d tasks after one create; want 1", n)
	}
}

// A session in a workspace that is already up must NOT place a second task.
//
// The risk increment 4 introduces: two things now place, and they run
// concurrently -- the create-time goroutine and a session request arriving
// behind it. Two tasks for one workspace is two filesystems (section 2.8), and
// the local driver hides it by name, so this counts processes rather than
// trusting the record.
func TestASessionAfterCreateDoesNotPlaceASecondTask(t *testing.T) {
	s := newStack(t, "sh", "-c", `exec cat`)
	s.newWorkspace("w1")

	// Deliberately NOT waiting for the placement first: a session arriving
	// while one is in flight is the interleaving worth testing, and it is the
	// one a user produces by typing `lem` straight after `lem workspace create`.
	sid := s.newSession("w1")
	if sid == "" {
		t.Fatal("no session id")
	}
	if n := s.supervisorCount(); n != 1 {
		t.Fatalf("driver holds %d tasks; a workspace with two tasks has two filesystems", n)
	}
	// One placement, so one generation. A second would have rotated the
	// credential out from under the first task.
	if gen := s.workspace("w1").Generation; gen != 1 {
		t.Errorf("generation is %d after one workspace create plus one session; want 1", gen)
	}
}

// A failed placement lands on the record instead of being lost.
//
// This is the whole reason last_error exists. While placement happened inside a
// request, a failure WAS the response body. A background placement has nobody
// to answer, so without this the customer's workspace simply never starts and
// the only explanation is a log line inside a service they cannot read.
func TestAFailedPlacementIsVisibleOnTheWorkspace(t *testing.T) {
	s := newStack(t, "sh", "-c", `exec cat`)
	s.stopRunner()

	code, body := s.postJSON("/workspaces", `{"name":"w1"}`)
	if code != http.StatusAccepted {
		t.Fatalf("create workspace: %d: %s", code, body)
	}

	ws := s.awaitWorkspaceStatus("w1", "stopped", 30*time.Second)
	if ws.LastError == "" {
		t.Fatal("a workspace that could not be placed reports no reason; " +
			"the failure went to a log and nowhere a customer can see")
	}
	if ws.TaskRef != "" {
		t.Errorf("a task reference %q was recorded for a task that was never dispatched", ws.TaskRef)
	}
	if n := s.supervisorCount(); n != 0 {
		t.Errorf("driver holds %d tasks after a placement that could not reach a runner", n)
	}
}
