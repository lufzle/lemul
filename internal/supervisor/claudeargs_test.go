package supervisor

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// writeTranscript fakes what Claude Code leaves on the workspace volume: a
// transcript named for the session id, under a project directory named from the
// mangled working directory. The mangling is deliberately nonsense here -- the
// probe must not depend on reproducing it.
func writeTranscript(t *testing.T, configDir, projectDir, sessionID string) {
	t.Helper()
	dir := filepath.Join(configDir, "projects", projectDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionID+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestSessionArgsPicksResumeOnlyWhenTheTranscriptExists is the test that matters.
//
// Measured against Claude Code 2.1.220, the two flags are complementary and each
// fails in the other's case: --session-id on an existing conversation is
// "Session ID … is already in use", and --resume on an absent one is "No
// conversation found with session ID: …". Either way the child exits immediately,
// so getting this backwards is not a degraded session, it is a dead one.
func TestSessionArgsPicksResumeOnlyWhenTheTranscriptExists(t *testing.T) {
	dir := t.TempDir()
	const fresh = "11111111-2222-4333-8444-555555555555"
	const existing = "99999999-8888-4777-8666-555555555555"
	writeTranscript(t, dir, "-workspace-some-project", existing)

	got := sessionArgs([]string{"claude"}, fresh, dir)
	if want := []string{"claude", "--session-id", fresh}; !slices.Equal(got, want) {
		t.Errorf("no transcript: got %v, want %v", got, want)
	}

	got = sessionArgs([]string{"claude"}, existing, dir)
	if want := []string{"claude", "--resume", existing}; !slices.Equal(got, want) {
		t.Errorf("transcript present: got %v, want %v", got, want)
	}
}

// The probe globs across project directories instead of deriving the directory
// name from the working directory, so that it couples only to "<id>.jsonl lives
// somewhere under projects/" and not to Claude Code's internal mangling scheme.
func TestTranscriptFoundUnderAnyProjectDirectory(t *testing.T) {
	dir := t.TempDir()
	const sid = "abcdef01-2345-4678-89ab-cdef01234567"
	writeTranscript(t, dir, "-some-entirely-unguessable-path", sid)

	if !transcriptExists(dir, sid) {
		t.Fatal("transcript not found under an unguessable project directory")
	}
	if transcriptExists(dir, "00000000-0000-4000-8000-000000000000") {
		t.Error("reported a transcript for a session that has none")
	}
}

// The create path is shared with whatever -session-cmd names, and the e2e
// fidelity suite drives it with cat and sh. Appending Claude Code's flags to an
// arbitrary command would break those and, worse, would pass an unknown flag to
// whatever a customer configures.
func TestSessionArgsLeavesNonClaudeCommandsAlone(t *testing.T) {
	dir := t.TempDir()
	const sid = "11111111-2222-4333-8444-555555555555"
	for _, cmd := range [][]string{
		{"cat"},
		{"sh", "-c", "echo hi"},
		{"/bin/bash"},
		{},
	} {
		if got := sessionArgs(cmd, sid, dir); !slices.Equal(got, cmd) {
			t.Errorf("cmd %v was rewritten to %v", cmd, got)
		}
	}
}

// A full path to claude is still claude.
func TestSessionArgsMatchesClaudeByBasename(t *testing.T) {
	dir := t.TempDir()
	const sid = "11111111-2222-4333-8444-555555555555"
	got := sessionArgs([]string{"/usr/local/bin/claude"}, sid, dir)
	want := []string{"/usr/local/bin/claude", "--session-id", sid}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// An operator who configured a session flag themselves means it. Adding a second
// one produces an argv Claude Code rejects, which would turn a deliberate
// configuration into a workspace where no session can start.
func TestSessionArgsRespectsAnExplicitSessionFlag(t *testing.T) {
	dir := t.TempDir()
	const sid = "11111111-2222-4333-8444-555555555555"
	for _, cmd := range [][]string{
		{"claude", "--continue"},
		{"claude", "-c"},
		{"claude", "--resume", "other"},
		{"claude", "-r"},
		{"claude", "--session-id", "other"},
		{"claude", "--session-id=other"},
	} {
		if got := sessionArgs(cmd, sid, dir); !slices.Equal(got, cmd) {
			t.Errorf("cmd %v was rewritten to %v", cmd, got)
		}
	}
}

// sessionArgs must not scribble on the caller's slice. The control plane hands
// the same Options.SessionCmd to every session, so an append that reused its
// backing array would leak one session's id into the next one's argv -- and the
// second session would then fail with "already in use", intermittently.
func TestSessionArgsDoesNotAliasTheCallersSlice(t *testing.T) {
	dir := t.TempDir()
	shared := make([]string, 1, 8) // spare capacity is what makes append dangerous
	shared[0] = "claude"

	a := sessionArgs(shared, "11111111-2222-4333-8444-555555555555", dir)
	b := sessionArgs(shared, "99999999-8888-4777-8666-555555555555", dir)

	if a[2] == b[2] {
		t.Fatalf("second call overwrote the first: %v vs %v", a, b)
	}
	if len(shared) != 1 || shared[0] != "claude" {
		t.Fatalf("caller's slice was modified: %v", shared)
	}
}

func TestRemoveTranscript(t *testing.T) {
	dir := t.TempDir()
	const sid = "abcdef01-2345-4678-89ab-cdef01234567"
	writeTranscript(t, dir, "-a-project", sid)

	if err := removeTranscript(dir, sid); err != nil {
		t.Fatal(err)
	}
	if transcriptExists(dir, sid) {
		t.Error("transcript survived removal")
	}
	// Idempotent: delete is retried, and a second call must not fail.
	if err := removeTranscript(dir, sid); err != nil {
		t.Errorf("second removal: %v", err)
	}
}
