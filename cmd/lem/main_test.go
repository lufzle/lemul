package main

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain points the user config directory at a throwaway location.
//
// cliauth resolves the token path through os.UserConfigDir, which is a real
// directory on a real machine -- so without this, every test in this package
// reads whatever `lem login` last wrote and behaves differently depending on
// whether the person running the suite happens to be signed in. That is exactly
// how TestListSessionsReportsHTTPErrors started failing on a laptop with a
// stale token and passing everywhere else.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "lem-test")
	if err != nil {
		panic(err)
	}
	// darwin derives it from HOME, linux from XDG_CONFIG_HOME. Set both rather
	// than branch on the platform.
	_ = os.Setenv("HOME", dir)
	_ = os.Setenv("XDG_CONFIG_HOME", dir)

	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// -server has to be settable by environment, or working against a deployed
// control plane means repeating it on every command -- and getting it wrong on
// ONE command is not a visible error: the token cached for the remote issuer is
// simply rejected by whatever else is listening on localhost:9000, which reads
// as a broken login rather than a misdirected one.
//
// Run against the built binary for the reason cmd/controlplane's equivalent is:
// `-h` prints a default only when it is non-empty.
func TestTheServerFlagIsSettableByEnvironment(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "lem")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("building: %v\n%s", err, out)
	}
	cmd := exec.Command(bin, "-h")
	cmd.Env = append(os.Environ(), "LEMUL_SERVER=https://cp.example.invalid")
	out, _ := cmd.CombinedOutput() // -h exits non-zero by design
	if !strings.Contains(string(out), "https://cp.example.invalid") {
		t.Errorf("-server does not default from LEMUL_SERVER; every command against "+
			"a remote control plane would need the flag repeated\n%s", out)
	}
}

// Flags really do work on both sides of the verb.
//
// They did not. flag.Parse stops at the first non-flag argument, so a flag
// written AFTER a verb was silently dropped -- and the dropped form is the one
// people type. Measured against the built binary on 2026-08-03:
//
//	lem session rm --session abc  ->  "session rm needs --session <id>"
//
// which is the command reporting the absence of the argument it was holding.
// The failure has no bad exit code to notice either: it reads as a usage error
// the user then "fixes" by retyping the same thing.
func TestFlagsAreParsedOnEitherSideOfTheVerb(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		wantPos    []string
		wantOrg    string
		wantSessID string
	}{
		{"flag before the verb", []string{"--session", "abc", "session", "rm"},
			[]string{"session", "rm"}, "", "abc"},
		{"flag after the verb", []string{"session", "rm", "--session", "abc"},
			[]string{"session", "rm"}, "", "abc"},
		{"flags on both sides", []string{"--org", "acme", "session", "ls", "--session", "abc"},
			[]string{"session", "ls"}, "acme", "abc"},
		{"no flags at all", []string{"workspace", "create", "mine"},
			[]string{"workspace", "create", "mine"}, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("lem", flag.ContinueOnError)
			gotOrg := fs.String("org", "", "")
			gotSess := fs.String("session", "", "")

			pos := parseInterspersed(fs, tc.args)

			if strings.Join(pos, " ") != strings.Join(tc.wantPos, " ") {
				t.Errorf("positionals = %q, want %q", pos, tc.wantPos)
			}
			if *gotOrg != tc.wantOrg {
				t.Errorf("--org = %q, want %q", *gotOrg, tc.wantOrg)
			}
			if *gotSess != tc.wantSessID {
				t.Errorf("--session = %q, want %q", *gotSess, tc.wantSessID)
			}
		})
	}
}

// A lone -- ends flag parsing, so a dash-leading name stays addressable.
//
// The server refuses to mint another one (mgmtapi.validWorkspaceName), but the
// workspace called --help that provoked all of this already existed, and a
// client that cannot name an existing record is how it got stuck in the first
// place. Everything after -- is a positional even when it looks like a flag.
func TestADoubleDashMakesADashLeadingNameAddressable(t *testing.T) {
	fs := flag.NewFlagSet("lem", flag.ContinueOnError)
	help := fs.Bool("help-me", false, "")

	pos := parseInterspersed(fs, []string{"workspace", "rm", "--", "--help-me"})

	if want := []string{"workspace", "rm", "--help-me"}; strings.Join(pos, " ") != strings.Join(want, " ") {
		t.Errorf("positionals = %q, want %q -- the name after -- was eaten as a flag", pos, want)
	}
	if *help {
		t.Error("--help-me after -- was parsed as a flag; -- has to terminate parsing")
	}
}
