package e2e_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/lufzle/lemul/internal/driver/docker"
)

// Section 2.4's auto-stop cascade, end to end against the real image.
//
// The DOCKER driver rather than the local one, and that is not a preference:
// the fourth idle condition reads /proc, which does not exist on darwin, so an
// unprivileged local supervisor reports every session as having a tool running
// (unknown reads as busy, activity.go) and NOTHING is ever idle. A test of idle
// detection on the local driver would pass by never running.
//
// Everything else is the real thing: the real Management API with its sweeper
// running, the real relay, a real runner, and the real supervisor inside the
// real sandbox image. Only the clock is compressed -- the cascade is driven at
// seconds instead of the 30 s cadences it ships with, through the same options
// a deployment sets.

// NOTE: these run the supervisor binary BAKED INTO THE IMAGE, so a change to
// internal/supervisor needs image/build.sh before it is under test here. A
// stale binary presents as a cascade that is simply slow -- the reporting
// interval falls back to its 30 s default and every assertion times out --
// which reads like a flaky test rather than like a stale image.
//
// idleStack brings up the cascade with everything turned down to seconds.
func idleStack(t *testing.T) *stack {
	t.Helper()
	requireDocker(t)
	image := workspaceImage()
	return newStackWith(t, stackOptions{
		// A shell, so the test decides what runs inside a session. Claude Code
		// itself needs a reachable gateway and costs money; what is under test
		// is the supervisor's view of a session's process tree, which a shell
		// exercises exactly as well.
		SessionCmd: []string{"sh"},
		Driver:     docker.New(image, nil),
		Image:      image,
		// The supervisor learns the first of these at placement, so both ends
		// agree about the cadence staleness is measured in.
		HeadroomInterval: time.Second,
		SweepInterval:    time.Second,
		Start:            true,
	})
}

// setPolicy compresses this workspace's lifecycle timings through the real
// PATCH endpoint, rather than by writing columns behind the API's back.
func (s *stack) setPolicy(workspace string, idleSecs, warmSecs int) {
	s.t.Helper()
	body := fmt.Sprintf(`{"policy":{"idle_timeout_secs":%d,"warm_hold_secs":%d}}`,
		idleSecs, warmSecs)
	resp, err := s.hDo(http.MethodPatch, s.orgURL+"/workspaces/"+workspace,
		"application/json", strings.NewReader(body))
	if err != nil {
		s.t.Fatalf("patch policy: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		s.t.Fatalf("patch policy: %s", resp.Status)
	}
}

// startSession creates a session and forks its PTY, leaving no client attached.
//
// The fork matters: a session RECORD has no process until something attaches
// (attach carries Create), so a test that only created one would be asserting
// about a session the supervisor has never heard of.
func (s *stack) startSession(workspace string, run string) string {
	s.t.Helper()
	sid := s.newSession(workspace)
	c := s.attach(sid, 24, 80, "")
	if run != "" {
		c.writeBytes([]byte(run + "\n"))
		// Let the child actually appear in /proc before the client goes away.
		time.Sleep(500 * time.Millisecond)
	}
	c.detach()
	return sid
}

func (s *stack) awaitSession(workspace, sid, want string, d time.Duration) {
	s.t.Helper()
	if got := s.awaitStatus(workspace, sid, want, d); got != want {
		s.t.Fatalf("session %s is %s after %s, want %s", sid, got, d, want)
	}
}

// A session with no client, no output and no tool is stopped. The base case,
// and the one everything else is measured against.
func TestAnIdleSessionIsStopped(t *testing.T) {
	s := idleStack(t)
	const ws = "idle-basic"
	s.newWorkspace(ws)
	// Warm hold long enough that the workspace stays up: this test is about the
	// session axis, and a task disappearing underneath it would prove nothing.
	s.setPolicy(ws, 3, 600)

	sid := s.startSession(ws, "")
	s.awaitSession(ws, sid, "stopped", 45*time.Second)
}

// THE REGRESSION THAT MATTERS. A long-running tool produces no PTY output and
// no model calls, because the shell is blocked waiting on it -- so the other
// three conditions all go quiet exactly when the session is busiest. Without
// the fourth condition this is the session that gets reaped an hour into a
// build.
func TestASessionRunningAToolIsNotStopped(t *testing.T) {
	s := idleStack(t)
	const ws = "idle-tool"
	s.newWorkspace(ws)
	s.setPolicy(ws, 3, 600)

	// A child that starts well after the session, which is what makes it a tool
	// rather than a startup child (supervisor/activity.go).
	sid := s.startSession(ws, "sleep 300 &")

	// Well past the idle timeout and several sweeps.
	time.Sleep(15 * time.Second)

	if got := s.sessionStatus(ws, sid); got != "running" {
		t.Fatalf("a session with a running tool was stopped (status %q); this is "+
			"the one-hour-build regression section 2.4's fourth condition exists "+
			"to prevent", got)
	}

	// And the cost of keeping it alive is REPORTED, since nothing else would
	// tell an operator why a workspace with no attached client is still billing.
	if since := s.workspace(ws).IdlePinnedSince; since == "" {
		t.Error("the workspace was held open by a background process and said " +
			"nothing about it; the bound on this case is visibility, so a silent " +
			"one is the whole failure")
	}
}

// The compute axis: last session goes, warm hold runs, task stops.
func TestWarmHoldStopsTheWorkspace(t *testing.T) {
	s := idleStack(t)
	const ws = "idle-warm"
	s.newWorkspace(ws)
	s.setPolicy(ws, 3, 3)

	sid := s.startSession(ws, "")
	s.awaitSession(ws, sid, "stopped", 45*time.Second)

	// The hold starts when the last session stops, and the task goes when it
	// expires. Asserted on the RECORD rather than on the container, because the
	// record is what the next placement decision reads -- and a task stopped
	// while the row still says active is the state section 2.4 warns about.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if w := s.workspace(ws); w.Status == "stopped" && !w.Connected {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	w := s.workspace(ws)
	t.Fatalf("the workspace never stopped after its warm hold: status=%q connected=%v "+
		"warm_hold_until=%q", w.Status, w.Connected, w.WarmHoldUntil)
}
