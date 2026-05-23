package mgmtapi

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/lufzle/lemul/internal/tunnel"
)

// Section 2.4's verdict is ALL FOUR conditions, never any three. Each case below
// leaves exactly one of them unsatisfied, so removing any single condition from
// isIdle fails exactly one of them.
func TestIdleNeedsAllFourConditions(t *testing.T) {
	const timeout = 60 * time.Second
	idle := tunnel.SessionActivity{
		SessionID: "s1", Attachers: 0,
		OutputIdleSecs: 120, ModelIdleSecs: 120, ToolIdleSecs: 120,
	}
	if !isIdle(idle, timeout) {
		t.Fatal("a session quiet on every axis was not idle")
	}

	for _, tc := range []struct {
		name string
		mut  func(a *tunnel.SessionActivity)
	}{
		{"a client is attached", func(a *tunnel.SessionActivity) { a.Attachers = 1 }},
		{"the PTY produced output", func(a *tunnel.SessionActivity) { a.OutputIdleSecs = 5 }},
		{"a model request went out", func(a *tunnel.SessionActivity) { a.ModelIdleSecs = 5 }},
		// The load-bearing one. A one-hour build produces neither output nor
		// model calls, so without this the other three go quiet precisely when
		// the workspace is busiest and the build is reaped an hour in.
		{"a tool is executing", func(a *tunnel.SessionActivity) { a.ToolIdleSecs = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := idle
			tc.mut(&a)
			if isIdle(a, timeout) {
				t.Errorf("stopped a session while %s", tc.name)
			}
		})
	}
}

// The specific regression worth naming: an unattended overnight agent run has no
// client, no keystrokes and long gaps between model calls, but its tool is
// running the whole time. This is the workload remote sandboxes exist for.
func TestALongRunningBuildIsNotIdle(t *testing.T) {
	build := tunnel.SessionActivity{
		SessionID:      "s1",
		Attachers:      0,    // nobody watching
		OutputIdleSecs: 3600, // a build that prints nothing
		ModelIdleSecs:  3600, // Claude Code is blocked waiting on it
		ToolIdleSecs:   0,    // ...and this is why it must survive
	}
	if isIdle(build, 30*time.Minute) {
		t.Fatal("a one-hour build with no client and no output was reaped")
	}
}

// SILENCE IS NOT IDLE, the mirror of "unknown headroom admits".
//
// A wedged tunnel must never look like an idle workspace. Inverting this makes
// the sweeper reap live work -- including a colleague's unattended run, since a
// workspace is shared -- on the strength of a report that never arrived.
func TestAStaleReportIsNotActedOn(t *testing.T) {
	const interval = 30 * time.Second
	now := time.Now()

	fresh := activityReport{ReceivedAt: now.Add(-interval)}
	if !fresh.fresh(now, interval) {
		t.Error("a report one interval old was treated as stale; one missed tick " +
			"is a hiccup, not a dead supervisor")
	}

	// Three intervals is the line. Past it, the supervisor has stopped talking
	// and tells us nothing about its sessions.
	old := activityReport{ReceivedAt: now.Add(-4 * interval)}
	if old.fresh(now, interval) {
		t.Fatal("a report four intervals old was acted on; a wedged tunnel would " +
			"read as an idle workspace and reap live sessions")
	}

	// A report that never arrived at all is the same case, and is what the
	// store's zero value produces.
	var never activityReport
	if never.fresh(now, interval) {
		t.Fatal("a workspace that has never reported was treated as fresh")
	}
}

func TestActivityStoreIsKeyedPerOrganization(t *testing.T) {
	const otherOrg = "00000000-0000-4000-8000-0000000000bb"
	a := newActivityStore()
	now := time.Now()

	a.put(testOrg, tunnel.Headroom{WorkspaceID: "api", Sessions: 3}, now)
	a.put(otherOrg, tunnel.Headroom{WorkspaceID: "api", Sessions: 0}, now)

	// A workspace name is unique only WITHIN an organization, so keying on the
	// name alone would let one customer's report decide another customer's
	// sessions -- the same id-vs-name class as section 12.8's tunnel registry.
	mine, ok := a.get(wsRef{testOrg, "api"})
	if !ok || mine.Headroom.Sessions != 3 {
		t.Fatalf("wrong report for the first organization: %+v", mine)
	}
	theirs, ok := a.get(wsRef{otherOrg, "api"})
	if !ok || theirs.Headroom.Sessions != 0 {
		t.Fatalf("wrong report for the second organization: %+v", theirs)
	}

	// Dropped with the task that produced it: a report left behind describes
	// sessions that died with it, and would read as four conditions satisfied.
	a.forget(wsRef{testOrg, "api"})
	if _, ok := a.get(wsRef{testOrg, "api"}); ok {
		t.Error("forget did not drop the report")
	}
	if _, ok := a.get(wsRef{otherOrg, "api"}); !ok {
		t.Error("forget dropped another organization's report")
	}
}

// RULE 1: an idle or warm-hold stop must NOT advance the workspace generation.
//
// A structural assertion over the source, in the spirit of the relay's
// import-graph test, and for the same reason: nothing else would notice this
// decaying. A generation bump added to the stop path later would compile, pass
// every behavioural test, and break only in production.
//
// What it costs when it is wrong is recorded in creds.go and section 2.8.
// Advancing the generation invalidates the credential held by a task that is
// alive and about to reconnect; the ECS driver then ADOPTS that same
// still-running task and hands back its ARN, so the placement reports success
// while the task can never authenticate again -- and it exits, taking every
// member's sessions with it.
func TestTheReaperNeverAdvancesTheGeneration(t *testing.T) {
	src, err := os.ReadFile("reaper.go")
	if err != nil {
		t.Fatalf("reading reaper.go: %v", err)
	}
	for _, forbidden := range []string{"nextGeneration", "NextGeneration"} {
		if bytes.Contains(src, []byte(forbidden)) {
			t.Errorf("reaper.go references %s.\n"+
				"An idle or warm-hold stop is a STOP, not a placement. Advancing the "+
				"generation rotates the credential out from under a task that is alive "+
				"and about to reconnect, and the ECS driver will adopt that same task "+
				"and report success -- leaving a workspace that can never authenticate "+
				"again (internal/creds/creds.go, section 2.8). Only a real placement "+
				"takes a new generation.", forbidden)
		}
	}
}

// The three states, and the boundary that matters: PINNED is not IDLE.
//
// A session quiet on every axis a person drives, but with a tool still running,
// must be kept alive -- it is the overnight build section 2.4 exists to protect,
// and it is also the dev server somebody left up. Those are indistinguishable
// from the control plane, and treating them the same way is the point.
func TestClassifySeparatesPinnedFromIdle(t *testing.T) {
	const timeout = 60 * time.Second
	quiet := tunnel.SessionActivity{
		Attachers: 0, OutputIdleSecs: 120, ModelIdleSecs: 120, ToolIdleSecs: 120,
	}

	for _, tc := range []struct {
		name string
		mut  func(a *tunnel.SessionActivity)
		want sessionState
	}{
		{"nothing at all is happening", func(*tunnel.SessionActivity) {}, sessionIdle},
		{"a tool is running", func(a *tunnel.SessionActivity) { a.ToolIdleSecs = 0 }, sessionPinned},
		{"a client is attached", func(a *tunnel.SessionActivity) { a.Attachers = 1 }, sessionBusy},
		{"the PTY is producing output", func(a *tunnel.SessionActivity) { a.OutputIdleSecs = 5 }, sessionBusy},
		{"model requests are going out", func(a *tunnel.SessionActivity) { a.ModelIdleSecs = 5 }, sessionBusy},
		// Busy WINS over pinned: somebody working while a build runs is a
		// working session, not a held-open one, and warning about it is noise.
		{"attached AND a tool running", func(a *tunnel.SessionActivity) {
			a.Attachers = 1
			a.ToolIdleSecs = 0
		}, sessionBusy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := quiet
			tc.mut(&a)
			if got := classify(a, timeout); got != tc.want {
				t.Errorf("classify = %v, want %v", got, tc.want)
			}
		})
	}
}

// isIdle must agree with classify, since one of them decides whether a process
// is killed and the other decides whether a warning appears.
func TestIsIdleAgreesWithClassify(t *testing.T) {
	const timeout = 60 * time.Second
	for _, a := range []tunnel.SessionActivity{
		{OutputIdleSecs: 120, ModelIdleSecs: 120, ToolIdleSecs: 120},
		{OutputIdleSecs: 120, ModelIdleSecs: 120, ToolIdleSecs: 0},
		{Attachers: 1, OutputIdleSecs: 120, ModelIdleSecs: 120, ToolIdleSecs: 120},
	} {
		if isIdle(a, timeout) != (classify(a, timeout) == sessionIdle) {
			t.Errorf("isIdle and classify disagree for %+v", a)
		}
	}
}
