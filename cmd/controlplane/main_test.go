package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The entry point's own logic, which is small and entirely about turning what an
// operator typed into what the server needs. The signing key it resolves is
// tested in internal/statekey, where both services' policies can be compared
// against each other.

// -print-runner-env has to work for an operator holding only the signing key,
// because it runs BEFORE a deployment exists: the credential is a function of
// the key alone, so a uuid needs no database. Requiring one would mean an
// operator cannot produce a runner's configuration until after they have stood
// up Postgres, which is backwards.
func TestAUUIDResolvesWithNoDatabase(t *testing.T) {
	const id = "5f2b6c4e-9a1d-4e7b-8c3a-0d1e2f3a4b5c"

	got, err := runnerTokenSubject(id, "", "")
	if err != nil {
		t.Fatalf("resolving a uuid with no DSN: %v", err)
	}
	if got != id {
		t.Errorf("got %q, want the uuid unchanged", got)
	}
}

// A slug is what a person actually has -- it is in the URL they were looking at
// and in what `lem orgs` printed -- so it is accepted, but resolving one needs
// the directory. With no DSN the refusal has to name both flags that would
// supply it, or the operator is told only that something is missing.
func TestASlugWithoutADatabaseSaysWhichFlagIsMissing(t *testing.T) {
	_, err := runnerTokenSubject("forty-crimson-windmill", "", "")
	if err == nil {
		t.Fatal("a slug resolved with no directory to resolve it against")
	}
	for _, want := range []string{"forty-crimson-windmill", "-directory-url", "-database-url"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// grantAppRole reads the role out of the application DSN so there is nothing
// extra to configure. A DSN naming no user therefore has nobody to grant to, and
// it must say so rather than open a pool and issue `GRANT ... TO ""` -- which
// fails later, inside a transaction, with a syntax error that names none of
// this.
func TestGrantingWithNoRoleInTheDSNRefusesBeforeConnecting(t *testing.T) {
	// Deliberately a DSN that would BLOCK if it were dialled: the refusal has to
	// come from reading the URL, not from a connection attempt.
	err := grantAppRole(t.Context(),
		"postgres://postgres:postgres@192.0.2.1:5432/postgres",
		"postgres://192.0.2.1:5432/postgres")
	if err == nil {
		t.Fatal("granted to nobody")
	}
	if !strings.Contains(err.Error(), "nobody to grant to") {
		t.Errorf("unexpected failure, which suggests it tried to connect: %v", err)
	}
}

// A malformed application DSN is refused with the parse error rather than
// panicking or reaching the database.
func TestGrantingRefusesAnUnparseableDSN(t *testing.T) {
	err := grantAppRole(t.Context(), "postgres://192.0.2.1/db", "://not a url")
	if err == nil {
		t.Fatal("accepted an unparseable application DSN")
	}
	if !strings.Contains(err.Error(), "application DSN") {
		t.Errorf("the failure does not say which DSN: %v", err)
	}
}

// Every flag an operator sets through the environment rather than argv, and the
// list is the assertion: it exists because two of them were missing.
//
// -gateway-url and -gateway-key defaulted to "" while their neighbours read
// LEMUL_*, so a deployment that exported LEMUL_GATEWAY_URL was configured in
// every visible sense and had no gateway. Nothing failed: the control plane
// started, the console reported inference as "Bedrock" -- which is its label
// for NO GATEWAY CONFIGURED and reads like a claim about a backend -- and a
// session would have come up with no model at all.
//
// A table rather than two assertions, because the defect was an INCONSISTENCY.
// The question is not whether one flag reads the environment; it is whether the
// set that should agree does.
//
// Run against the BUILT BINARY rather than an extracted flag set, because
// main() defines these inline and restructuring it to be callable is a larger
// change than the property is worth. `-h` prints a default only when it is
// non-empty, so the sentinel appearing there IS the flag having read the
// variable.
func TestTheFlagsAnOperatorSetsByEnvironmentAllReadIt(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "controlplane")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building: %v\n%s", err, out)
	}

	for _, c := range []struct{ flagName, env string }{
		{"database-url", "LEMUL_DATABASE_URL"},
		{"auth-issuer", "LEMUL_AUTH_ISSUER"},
		{"auth-audience", "LEMUL_AUTH_AUDIENCE"},
		{"auth-cli-client-id", "LEMUL_CLI_CLIENT_ID"},
		{"auth-console-client-id", "LEMUL_CONSOLE_CLIENT_ID"},
		{"gateway-url", "LEMUL_GATEWAY_URL"},
		{"gateway-key", "LEMUL_GATEWAY_KEY"},
	} {
		t.Run(c.flagName, func(t *testing.T) {
			want := "sentinel-" + c.flagName
			cmd := exec.Command(bin, "-h")
			cmd.Env = append(os.Environ(), c.env+"="+want)
			out, _ := cmd.CombinedOutput() // -h exits non-zero by design
			if !strings.Contains(string(out), want) {
				t.Errorf("-%s does not default from %s: an operator who sets the "+
					"variable gets a server that ignores it and says nothing",
					c.flagName, c.env)
			}
		})
	}
}
