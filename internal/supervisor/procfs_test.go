package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The classic /proc parsing bug: the comm field is parenthesised and its
// contents are chosen by the process itself, so splitting the line on spaces
// reads attacker-controlled text as the ppid and the memory figures. A process
// can literally name itself "(evil) 1 2 3".
func TestParseStatSurvivesAHostileCommField(t *testing.T) {
	// comm contains spaces, parens and digits; fields after it must still land.
	line := "4242 (evil) 1 2 3) S 99 " + strings.Repeat("0 ", 9) +
		"111 222 0 0 0 0 0 0 0 0 0 0 0 0 5000 12345 0 0 0 0 0 0 0 0 0 0 0"

	got, ok := parseStat(line)
	if !ok {
		t.Fatal("parseStat rejected a valid line")
	}
	if got.info.PID != 4242 {
		t.Errorf("PID = %d, want 4242", got.info.PID)
	}
	if got.info.Name != "evil) 1 2 3" {
		t.Errorf("Name = %q; the comm field was not taken from the LAST paren", got.info.Name)
	}
	if got.info.State != "S" {
		t.Errorf("State = %q, want S", got.info.State)
	}
	if got.info.PPID != 99 {
		t.Errorf("PPID = %d, want 99 -- fields were read from inside comm", got.info.PPID)
	}
	// utime 111 + stime 222 over 100 ticks/sec.
	if want := 3.33; got.info.CPUSecs < want-0.01 || got.info.CPUSecs > want+0.01 {
		t.Errorf("CPUSecs = %v, want ~%v", got.info.CPUSecs, want)
	}
}

func TestParseStatRejectsGarbage(t *testing.T) {
	for _, s := range []string{"", "no parens here", "123 (short) S", "(x) 1 2"} {
		if _, ok := parseStat(s); ok {
			t.Errorf("parseStat(%q) accepted garbage", s)
		}
	}
}

// Attribution is what makes the process panel worth having: a Bash tool three
// levels below Claude Code has to land on the conversation that ran it.
func TestReadProcessesAttributesDescendantsToSessions(t *testing.T) {
	root := t.TempDir()
	procRoot = root
	t.Cleanup(func() { procRoot = "/proc" })

	// 1 init -> 10 supervisor -> 20 claude(session A) -> 30 bash tool
	//                                                 -> 40 mcp server
	//                         -> 21 claude(session B)
	writeProc(t, root, 1, 0, "init", "S")
	writeProc(t, root, 10, 1, "supervisor", "S")
	writeProc(t, root, 20, 10, "claude", "S")
	writeProc(t, root, 30, 20, "bash", "R")
	writeProc(t, root, 40, 20, "mcp-server", "S")
	writeProc(t, root, 21, 10, "claude", "S")

	procs, ok := readProcesses(map[string]int{"sess-a": 20, "sess-b": 21})
	if !ok {
		t.Fatal("readProcesses reported unavailable against a fixture tree")
	}

	byPID := map[int]ProcInfo{}
	for _, p := range procs {
		byPID[p.PID] = p
	}
	for pid, want := range map[int]string{
		1: "", 10: "", // outside any session
		20: "sess-a", 30: "sess-a", 40: "sess-a", // the whole subtree
		21: "sess-b",
	} {
		if got := byPID[pid].SessionID; got != want {
			t.Errorf("pid %d attributed to %q, want %q", pid, got, want)
		}
	}
}

// A reparented process can produce a cycle, since /proc is read without a lock.
// The walk must terminate rather than hang the supervisor.
func TestReadProcessesTerminatesOnACycle(t *testing.T) {
	root := t.TempDir()
	procRoot = root
	t.Cleanup(func() { procRoot = "/proc" })

	writeProc(t, root, 50, 51, "a", "S")
	writeProc(t, root, 51, 50, "b", "S")

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, ok := readProcesses(map[string]int{"sess": 999}); !ok {
			t.Error("unavailable against a fixture tree")
		}
	}()
	<-done // the depth cap is what makes this return at all
}

// Absent /proc is "unknown", not "nothing is running" -- the local driver on
// darwin has none, and a console showing an empty process list there would be
// stating something false.
func TestReadProcessesReportsUnavailable(t *testing.T) {
	procRoot = filepath.Join(t.TempDir(), "does-not-exist")
	t.Cleanup(func() { procRoot = "/proc" })

	if _, ok := readProcesses(nil); ok {
		t.Error("reported available with no procfs")
	}
}

func TestCmdlineJoinsAndCaps(t *testing.T) {
	if got := cmdline([]byte("claude\x00--resume\x00abc\x00")); got != "claude --resume abc" {
		t.Errorf("got %q", got)
	}
	long := cmdline([]byte(strings.Repeat("x", 5000)))
	if len(long) > 600 {
		t.Errorf("cmdline not capped: %d chars", len(long))
	}
}

func writeProc(t *testing.T, root string, pid, ppid int, name, state string) {
	t.Helper()
	dir := filepath.Join(root, fmt.Sprint(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// pid (comm) state ppid, then padding through the fields parseStat reads.
	line := fmt.Sprintf("%d (%s) %s %d %s", pid, name, state, ppid,
		strings.TrimSpace(strings.Repeat("0 ", 30)))
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The supervisor's own argv carries -token, the workspace tunnel credential. It
// was rendered in full in the console the first time this panel met a real
// task -- the same hole /proc/environ was excluded to avoid, arriving through a
// door that had not been shut.
func TestRedactSecretsHidesCredentialsInArgv(t *testing.T) {
	// Shaped like a real credential -- 64 hex characters, as mintWorkspace
	// produces -- but not one. The value that exposed this was copied from a
	// live container, and committing it would have put a credential in the
	// repository in the course of fixing a leak.
	const secret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	got := redactSecrets("/usr/local/bin/supervisor -control-plane ws://host:9000 -workspace demo -token " + secret)

	if strings.Contains(got, secret) {
		t.Fatalf("the tunnel credential survived redaction: %q", got)
	}
	if !strings.Contains(got, "<redacted>") {
		t.Errorf("no redaction marker: %q", got)
	}
	// Everything an operator actually needs must survive.
	for _, want := range []string{"supervisor", "-workspace", "demo", "ws://host:9000"} {
		if !strings.Contains(got, want) {
			t.Errorf("redaction ate %q: %s", want, got)
		}
	}
}

func TestRedactSecretsHandlesTheEqualsForm(t *testing.T) {
	got := redactSecrets("supervisor --gateway-key=sk-not-a-real-key --workspace=demo")
	if strings.Contains(got, "sk-not-a-real-key") {
		t.Errorf("key survived: %q", got)
	}
	if !strings.Contains(got, "--workspace=demo") {
		t.Errorf("a non-secret flag was redacted: %q", got)
	}
}

func TestRedactSecretsLeavesOrdinaryCommandsAlone(t *testing.T) {
	const cmd = "claude --session-id 775cd2a7-73eb-4f4d-b27f-e74f63d26905"
	if got := redactSecrets(cmd); got != cmd {
		t.Errorf("got %q, want it untouched", got)
	}
}
