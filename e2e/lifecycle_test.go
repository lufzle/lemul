package e2e_test

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The session-process axis of section 2.4, end to end through the real
// supervisor binary. What these prove is that the process axis moves
// independently of the client-presence axis: a session can be stopped while
// nobody is attached, and resumed without anyone attaching.

// TestStopEndsTheProcessAndKeepsTheRecord is the distinction section 2.4 calls
// out: stopping a session is Ctrl-C/Ctrl-D semantics, not deletion. The record
// outlives the process, because the conversation does.
func TestStopEndsTheProcessAndKeepsTheRecord(t *testing.T) {
	s := newStack(t, "cat")
	sid := s.newSession("w1")
	c := s.attach(sid, 24, 80, "")
	c.writeBytes([]byte("hello\n"))
	c.await("hello", 3*time.Second)

	if got := s.sessionStatus("w1", sid); got != "running" {
		t.Fatalf("before stop: status %q, want running", got)
	}

	if code, body := s.post("/sessions/" + sid + "/stop"); code != http.StatusOK {
		t.Fatalf("stop: %d: %s", code, body)
	}

	if got := s.awaitStatus("w1", sid, "stopped", 5*time.Second); got != "stopped" {
		t.Fatalf("after stop: status %q, want stopped", got)
	}
	// The record survives: this is the difference between stop and delete.
	found := false
	for _, d := range s.sessionList("w1") {
		if d.ID == sid {
			found = true
		}
	}
	if !found {
		t.Error("stop dropped the session record; it should outlive the process")
	}
}

// Stop must be idempotent. A UI button, a retried request and a session whose
// process already exited all have to land on the same answer, or the console
// shows an error for an outcome the user already has.
func TestStopIsIdempotent(t *testing.T) {
	s := newStack(t, "cat")
	sid := s.newSession("w1")

	// Never attached, so the record exists but no PTY ever did.
	if code, body := s.post("/sessions/" + sid + "/stop"); code != http.StatusOK {
		t.Fatalf("stop of a never-started session: %d: %s", code, body)
	}
	if code, body := s.post("/sessions/" + sid + "/stop"); code != http.StatusOK {
		t.Fatalf("second stop: %d: %s", code, body)
	}
}

func TestStopOfAnUnknownSessionIs404(t *testing.T) {
	s := newStack(t, "cat")
	s.newSession("w1") // place the workspace
	code, _ := s.post("/sessions/00000000-0000-4000-8000-000000000000/stop")
	if code != http.StatusNotFound {
		t.Errorf("stop of unknown session: %d, want 404", code)
	}
}

// TestResumeStartsTheProcessWithoutAnyClient is the property that makes resume a
// verb of its own rather than a side effect of attaching: an admin console has to
// be able to put an agent back to work without becoming its terminal.
func TestResumeStartsTheProcessWithoutAnyClient(t *testing.T) {
	s := newStack(t, "cat")
	sid := s.newSession("w1")

	if code, body := s.post("/sessions/" + sid + "/resume"); code != http.StatusOK {
		t.Fatalf("resume: %d: %s", code, body)
	}

	if got := s.awaitStatus("w1", sid, "running", 5*time.Second); got != "running" {
		t.Fatalf("after resume: status %q, want running", got)
	}
	for _, d := range s.sessionList("w1") {
		if d.ID == sid && d.Attachers != 0 {
			t.Errorf("resume attached %d client(s); it must start the process only", d.Attachers)
		}
	}
}

// Resuming a running session is what a user gets for pressing the button twice.
// It must not fork a second process onto the same conversation.
func TestResumeIsIdempotent(t *testing.T) {
	s := newStack(t, "cat")
	sid := s.newSession("w1")

	for i := range 2 {
		if code, body := s.post("/sessions/" + sid + "/resume"); code != http.StatusOK {
			t.Fatalf("resume %d: %d: %s", i, code, body)
		}
	}
	if got := s.awaitStatus("w1", sid, "running", 5*time.Second); got != "running" {
		t.Fatalf("status %q, want running", got)
	}
	// One session, one process. A second create would have failed with ErrExists
	// or, worse, left an orphan.
	n := 0
	for _, d := range s.sessionList("w1") {
		if d.ID == sid {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d records for one session id", n)
	}
}

// Delete is unrecoverable in a way stop is not, so a running session needs the
// intent stated explicitly. Without this a mis-click kills a live agent run AND
// burns the history that would have let it be resumed.
func TestDeleteRefusesARunningSessionWithoutForce(t *testing.T) {
	s := newStack(t, "cat")
	sid := s.newSession("w1")
	c := s.attach(sid, 24, 80, "")
	c.writeBytes([]byte("hi\n"))
	c.await("hi", 3*time.Second)

	code, body := s.del("/sessions/" + sid)
	if code != http.StatusConflict {
		t.Fatalf("delete of a running session: %d: %s, want 409", code, body)
	}
	if !strings.Contains(body, "force") {
		t.Errorf("the 409 should name the way forward; got %q", body)
	}
	if got := s.sessionStatus("w1", sid); got != "running" {
		t.Errorf("the refused delete disturbed the session: status %q", got)
	}

	if code, body := s.del("/sessions/" + sid + "?force=1"); code != http.StatusNoContent {
		t.Fatalf("forced delete: %d: %s", code, body)
	}
	if got := s.sessionStatus("w1", sid); got != "" {
		t.Errorf("session still listed after delete: status %q", got)
	}
}

// A stopped session is deletable with no ceremony, and deleting twice is not an
// error -- the caller has the outcome they asked for either way.
func TestDeleteAStoppedSessionAndThenAgain(t *testing.T) {
	s := newStack(t, "cat")
	sid := s.newSession("w1")

	if code, body := s.del("/sessions/" + sid); code != http.StatusNoContent {
		t.Fatalf("delete: %d: %s", code, body)
	}
	if code, _ := s.del("/sessions/" + sid); code != http.StatusNoContent {
		t.Errorf("second delete: %d, want 204", code)
	}
}

// Session ids must be UUIDs. Claude Code's --session-id rejects anything else, so
// a change to the id format here silently breaks resume for every session --
// which would show up as "Session ID is already in use" rather than as anything
// pointing at the id format.
func TestSessionIDsAreUUIDs(t *testing.T) {
	s := newStack(t, "cat")
	uuid := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	for range 5 {
		if sid := s.newSession("w1"); !uuid.MatchString(sid) {
			t.Fatalf("session id %q is not a v4 UUID", sid)
		}
	}
}

// TestResumeArgvFollowsTheTranscript is the one that guards the sharp edge.
//
// Claude Code 2.1.220's --session-id and --resume are complementary and each
// fails in the other's case, so the supervisor has to pick from what is actually
// on the workspace volume. This drives the real path -- control plane, tunnel,
// real supervisor binary -- and asserts the argv the child was handed.
func TestResumeArgvFollowsTheTranscript(t *testing.T) {
	configDir := t.TempDir()
	// Inherited by the supervisor the local driver spawns, exactly as the image's
	// CLAUDE_CONFIG_DIR=/workspace/.claude reaches it on a real task.
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)

	s := newStack(t, fakeClaude(t))
	sid := s.newSession("w1")

	// First start: no conversation on disk, so the id has to be claimed.
	c := s.attach(sid, 24, 80, "")
	out := c.await("ARGV:", 5*time.Second)
	if !strings.Contains(out, "--session-id "+sid) {
		t.Fatalf("first start should claim the id; argv was %q", firstLine(out))
	}
	if strings.Contains(out, "--resume") {
		t.Fatalf("first start must not resume a conversation that does not exist; argv was %q", firstLine(out))
	}
	c.detach()

	// Claude Code has now filed a conversation under this id.
	writeTranscript(t, configDir, sid)
	if code, body := s.post("/sessions/" + sid + "/stop"); code != http.StatusOK {
		t.Fatalf("stop: %d: %s", code, body)
	}
	if got := s.awaitStatus("w1", sid, "stopped", 5*time.Second); got != "stopped" {
		t.Fatalf("status %q, want stopped", got)
	}

	// Second start: the conversation exists, so it must be resumed rather than
	// claimed -- claiming would fail with "Session ID … is already in use".
	c2 := s.attach(sid, 24, 80, "")
	out2 := c2.await("ARGV:", 5*time.Second)
	if !strings.Contains(out2, "--resume "+sid) {
		t.Fatalf("second start should resume; argv was %q", firstLine(out2))
	}
	if strings.Contains(out2, "--session-id") {
		t.Fatalf("second start must not re-claim the id; argv was %q", firstLine(out2))
	}
}

// Deleting a session drops its conversation, so a later session must not be able
// to resume it. This is what makes delete different from stop.
func TestDeleteDropsTheConversation(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)

	s := newStack(t, fakeClaude(t))
	sid := s.newSession("w1")
	c := s.attach(sid, 24, 80, "")
	c.await("ARGV:", 5*time.Second)
	writeTranscript(t, configDir, sid)

	if code, body := s.del("/sessions/" + sid + "?force=1"); code != http.StatusNoContent {
		t.Fatalf("delete: %d: %s", code, body)
	}
	if transcriptPresent(configDir, sid) {
		t.Error("the conversation survived a delete")
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// THE property `lem --session <id>` rests on, and the reason the CLI has no
// `resume` verb any more.
//
// Attaching to a session whose process has ended must bring it back rather than
// failing, so that one flag covers three cases a user cannot tell apart anyway:
// still running and detached, stopped, or in a workspace whose task is gone.
// Nothing in the client decides which -- the supervisor forks on Create and
// picks --resume or --session-id by looking at the transcript on the volume
// (§12.7), which is the only honest source since a replacement task comes up
// with an empty disk.
//
// Verified here rather than reasoned about: the whole restructure assumed it.
func TestAttachingToAStoppedSessionRevivesIt(t *testing.T) {
	s := newStack(t, "cat")
	sid := s.newSession("w1")

	c := s.attach(sid, 24, 80, "")
	c.writeBytes([]byte("before\n"))
	c.await("before", 3*time.Second)
	c.detach()

	if code, body := s.post("/sessions/" + sid + "/stop"); code != http.StatusOK {
		t.Fatalf("stop: %d: %s", code, body)
	}
	if got := s.awaitStatus("w1", sid, "stopped", 5*time.Second); got != "stopped" {
		t.Fatalf("after stop: status %q, want stopped", got)
	}

	// No call to /resume. Attach alone, which is all `lem --session` does.
	again := s.attach(sid, 24, 80, "")
	again.writeBytes([]byte("after\n"))
	again.await("after", 5*time.Second)

	if got := s.sessionStatus("w1", sid); got != "running" {
		t.Errorf("after reattaching: status %q, want running", got)
	}
	again.detach()
}
