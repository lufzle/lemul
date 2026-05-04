package main

import (
	"strings"
	"testing"
	"time"
)

// The workspace task is configured almost entirely from its environment,
// because that is the one mechanism identical for a local child process and an
// ECS task definition (mgmtapi.workspaceEnv). These are the parsers on the
// receiving end of it: everything a deployment sets arrives here as a string,
// and every one of them has a wrong answer that is silent.

// The command a session runs, which is the whole of section 13's second finding.
// It arrives as JSON because an argv element may itself contain spaces --
// `sh -c 'a; b'` is ONE argument -- and a space-separated encoding would
// re-split it.
func TestTheSessionCommandSurvivesTheEnvironment(t *testing.T) {
	got := argvFromJSON(`["sh","-c","echo one two three; exec cat"]`, []string{"claude"})

	want := []string{"sh", "-c", "echo one two three; exec cat"}
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", []string(got), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %q, want %q", []string(got), want)
		}
	}
}

// Falling back rather than failing. A malformed value should not stop a
// workspace starting: the fallback is what the deployment would have run anyway,
// and a task that refuses to come up is a much worse outcome than one running
// the default.
func TestABadSessionCommandFallsBack(t *testing.T) {
	for _, v := range []string{
		"",                 // nothing set: a hand-run supervisor
		"claude",           // the flag's form, not the environment's
		`{"cmd":"claude"}`, // right idea, wrong shape
		`[]`,               // valid JSON, no program in it
		`[`,                // truncated
	} {
		got := argvFromJSON(v, []string{"claude"})
		if len(got) != 1 || got[0] != "claude" {
			t.Errorf("argvFromJSON(%q) = %q, want the fallback", v, []string(got))
		}
	}
}

// -cmd REPLACES what the environment seeded rather than being ignored by it,
// which is how every other environment-defaulted flag in this binary behaves.
// It is the reason this is a flag.Value and not a flag default: a default
// computed from a JSON environment variable would be mangled by the flag's own
// space-splitting.
func TestTheFlagOverridesTheEnvironment(t *testing.T) {
	a := argvFromJSON(`["claude"]`, []string{"claude"})

	if err := a.Set("sh -c exit"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := strings.Join(a, "|"); got != "sh|-c|exit" {
		t.Errorf("after -cmd: %q", got)
	}
}

// envUint yields 0 for anything it cannot parse, and 0 DISABLES the uid
// boundary that keeps the gateway credential out of a session's reach. That is
// the one setting here whose safe direction is not the zero value, which is why
// the supervisor warns at startup when it is root, holding a credential, and
// unbounded -- this pins the parsing half of that.
func TestEnvUintIsZeroForAnythingUnparseable(t *testing.T) {
	for _, v := range []string{"", "abc", "-1", "1000x", "99999999999999999999"} {
		t.Setenv("LEMUL_TEST_UID", v)
		if got := envUint("LEMUL_TEST_UID"); got != 0 {
			t.Errorf("envUint(%q) = %d, want 0", v, got)
		}
	}
	t.Setenv("LEMUL_TEST_UID", "1000")
	if got := envUint("LEMUL_TEST_UID"); got != 1000 {
		t.Errorf("envUint(1000) = %d", got)
	}
}

// The headroom cadence is measured for staleness at the OTHER end (section 2.4),
// so the two have to agree about it. A bad value falls back rather than failing,
// but it must fall back to the SAME default the control plane assumes -- and
// non-positive is a bad value, not a way to disable reporting, since a zero
// ticker panics and a negative one never fires.
func TestEnvDurationFallsBackRatherThanFailing(t *testing.T) {
	const def = 30 * time.Second

	for _, v := range []string{"", "soon", "0", "0s", "-5s"} {
		t.Setenv("LEMUL_TEST_INTERVAL", v)
		if got := envDuration("LEMUL_TEST_INTERVAL", def); got != def {
			t.Errorf("envDuration(%q) = %s, want the default", v, got)
		}
	}
	t.Setenv("LEMUL_TEST_INTERVAL", "2s")
	if got := envDuration("LEMUL_TEST_INTERVAL", def); got != 2*time.Second {
		t.Errorf("envDuration(2s) = %s", got)
	}
}
