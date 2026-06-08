package e2e_test

import (
	"net/http"
	"testing"
	"time"
)

// What a session runs has ONE owner, and these execute the wiring that decides
// it rather than asserting the configuration is present somewhere.
//
// The bug this replaces was found by hand, not by the suite, and the suite could
// not have found it: `SessionCmd` was configured independently in the relay (for
// attach-created PTYs) and in the Management API (for resume), each defaulting
// to `claude` on its own. A control plane started with `-session-cmd sh` kept
// starting Claude Code, because the relay had never been told -- and every test
// passed, because the harness dutifully configured BOTH services with the same
// value. A suite that sets up both halves of an agreement proves the two agree
// when told to; it says nothing about a deployment that tells only one.
//
// So the harness now configures the control plane and NOTHING ELSE, and these
// two tests are the reason that matters. They are deliberately about the same
// property from the two directions a session can be forked from, because that
// property is precisely "the two paths cannot disagree".

// The attach path, which is the one that was broken. The relay is constructed
// with a signing key and no session command -- the field no longer exists -- so
// a session created by attaching can only be running what the task was told at
// placement.
func TestAnAttachCreatedSessionRunsWhatOnlyTheControlPlaneWasTold(t *testing.T) {
	// A command nothing defaults to: `claude` would be indistinguishable from
	// the old bug, and `cat` is what half this suite uses. This one identifies
	// itself, so a failure says which program actually ran.
	s := newStack(t, "sh", "-c", `echo TOLD_AT_PLACEMENT; exec cat`)

	sid := s.newSession("w1")
	c := s.attach(sid, 24, 80, "")
	c.await("TOLD_AT_PLACEMENT", 5*time.Second)
}

// The resume path, forking with no client attached. Same workspace, same
// configuration, and it must reach the same program -- which it now does by
// construction rather than by two services being kept in step, since neither
// message carries a command any more (tunnel.Attach, tunnel.StartSession).
func TestAResumedSessionRunsTheSameProgramAsAnAttachedOne(t *testing.T) {
	s := newStack(t, "sh", "-c", `echo TOLD_AT_PLACEMENT; exec cat`)

	sid := s.newSession("w1")
	c := s.attach(sid, 24, 80, "")
	c.await("TOLD_AT_PLACEMENT", 5*time.Second)
	c.detach()

	if code, body := s.post("/sessions/" + sid + "/stop"); code != http.StatusOK {
		t.Fatalf("stop: %d: %s", code, body)
	}
	if got := s.awaitStatus("w1", sid, "stopped", 5*time.Second); got != "stopped" {
		t.Fatalf("after stop: status %q, want stopped", got)
	}

	// Resume forks with nobody watching, so the proof has to come from
	// reattaching afterwards: the replay ring holds what the new process wrote.
	if code, body := s.post("/sessions/" + sid + "/resume"); code != http.StatusOK {
		t.Fatalf("resume: %d: %s", code, body)
	}
	if got := s.awaitStatus("w1", sid, "running", 5*time.Second); got != "running" {
		t.Fatalf("after resume: status %q, want running", got)
	}

	c2 := s.attach(sid, 24, 80, "")
	c2.await("TOLD_AT_PLACEMENT", 5*time.Second)
}

// An argv element may contain spaces -- `sh -c 'a; b'` is ONE argument, and the
// fidelity suite is built out of them. The command now reaches the task through
// the environment, so it is serialised and parsed on the way, and a
// space-joined encoding would re-split that one argument into three.
//
// The failure would not look like a corrupted argument. It looks like a session
// that exits immediately with a message from a shell about an unexpected token,
// which reads as a broken workspace.
func TestAnArgumentContainingSpacesSurvivesPlacement(t *testing.T) {
	s := newStack(t, "sh", "-c", `echo "one two three"; exec cat`)

	sid := s.newSession("w1")
	c := s.attach(sid, 24, 80, "")
	c.await("one two three", 5*time.Second)
}
