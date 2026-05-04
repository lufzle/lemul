package main

import (
	"strings"
	"testing"
)

// THE rule in narrate, and the one that is invisible when it is wrong.
//
// The event stream opens with a snapshot of state that predates this command.
// A workspace whose last placement failed still carries that message, so a
// client that reported every `last_error` it saw would greet a user who just
// typed `lem` with the failure of somebody's attempt an hour ago -- and then
// stop watching, on the run that was about to succeed.
func TestAStaleFailureInTheSnapshotIsNotReported(t *testing.T) {
	stale := workspaceEvent{Status: "stopped", LastError: "no runner connected for organization acme"}

	say, done := narrate("w1", stale, true)
	if strings.Contains(say, "failed") {
		t.Errorf("the snapshot's old failure was reported as this attempt's: %q", say)
	}
	if done {
		t.Error("watching stopped on a failure that had already happened before we connected")
	}
	if !strings.Contains(say, "starting") {
		t.Errorf("a workspace that is not up said %q; the user is owed a reason for the wait", say)
	}

	// The same message arriving as a CHANGE is this attempt's, and is reported.
	say, done = narrate("w1", stale, false)
	if !strings.Contains(say, "failed") {
		t.Errorf("a failure of the attempt being watched was not reported: %q", say)
	}
	if !done {
		t.Error("kept watching a placement that had already failed")
	}
}

// A workspace already up is narrated with nothing at all. The warm path is the
// common one, and a line about starting a workspace that is running would be
// both wrong and the first thing a user sees on every single session.
func TestAWarmWorkspaceIsSilent(t *testing.T) {
	up := workspaceEvent{Status: "active", Connected: true}
	if say, done := narrate("w1", up, true); say != "" || !done {
		t.Errorf("a workspace that was already up said %q (done=%v)", say, done)
	}
}

// Recorded active but with no task holding a tunnel is NOT up. Status is what
// the control plane last wrote down; connected is whether anything is there --
// and telling a user their workspace is up while the request they are waiting
// on places a replacement task is the one case where the two disagreeing
// matters to them.
func TestActiveWithoutATaskIsNotUp(t *testing.T) {
	stale := workspaceEvent{Status: "active", Connected: false}
	say, done := narrate("w1", stale, true)
	if done {
		t.Error("stopped watching a workspace recorded active whose task is gone")
	}
	if !strings.Contains(say, "starting") {
		t.Errorf("said %q for a workspace with no task; a replacement is being placed", say)
	}
}
