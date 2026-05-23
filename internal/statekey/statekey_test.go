package statekey

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lufzle/lemul/internal/creds"
)

// THE property every derived credential rests on, and the reason this package
// exists at all.
//
// Credentials are derived from the key rather than stored (internal/creds), so a
// restarted control plane recognises a workspace task its predecessor placed
// only if it derives them the same way. A key regenerated at boot reproduces the
// exact failure that design removes -- every restart orphans every live task --
// and it would do it silently, since a fresh key is a perfectly valid key.
func TestTheSameStateDirYieldsTheSameKey(t *testing.T) {
	dir := t.TempDir()

	first, err := LoadOrCreate("", dir)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	second, err := LoadOrCreate("", dir)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if string(first) != string(second) {
		t.Fatal("a second start derived a different key, which orphans every running task")
	}
}

// The cross-service property, and the one no test in either package main could
// reach: the relay verifies with EXACTLY the bytes the Management API mints
// with. They agree on a directory in the README and on a filename in code, and
// nothing compared the two halves.
//
// This is what "point both at the same -state-dir" means, asserted rather than
// documented.
func TestTheRelayReadsWhatTheControlPlaneCreated(t *testing.T) {
	dir := t.TempDir()

	minted, err := LoadOrCreate("", dir)
	if err != nil {
		t.Fatalf("control plane: %v", err)
	}
	verified, err := Load("", dir)
	if err != nil {
		t.Fatalf("relay: %v", err)
	}
	if string(minted) != string(verified) {
		t.Fatal("the relay read a different key than the control plane created, " +
			"so every credential minted would be rejected as unauthorized")
	}
}

// The asymmetry itself. The relay must never invent a key: one it made up is one
// nothing else shares, and the symptom is `unauthorized` on a correctly minted
// credential -- which reads as a bad token and sends whoever is debugging it to
// look at the client.
//
// So it refuses to start, and the refusal has to name the control plane. A test
// on the error alone would pass on a message that says nothing useful.
func TestTheRelayRefusesToInventAKey(t *testing.T) {
	dir := t.TempDir()

	_, err := Load("", dir)
	if err == nil {
		t.Fatal("the relay accepted a state dir holding no key")
	}
	if !strings.Contains(err.Error(), "control plane") {
		t.Fatalf("the refusal does not name what to start first: %v", err)
	}
	if _, err := os.Stat(Path(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the relay created a key file; it must only ever read one")
	}
}

// No state dir at all is the same refusal, reached a different way. This is the
// case an operator hits first, because it is what happens when -state-dir is
// simply forgotten.
func TestTheRelayRefusesWithNoStateDir(t *testing.T) {
	_, err := Load("", "")
	if err == nil {
		t.Fatal("the relay started with no key and no state dir")
	}
	if !strings.Contains(err.Error(), "-signing-key") ||
		!strings.Contains(err.Error(), "-state-dir") {
		t.Fatalf("the refusal names neither way to fix it: %v", err)
	}
}

// The Management API's half of the asymmetry: finding none, it makes one,
// because it is the service that OWNS the key.
func TestTheControlPlaneCreatesAKeyItCannotFind(t *testing.T) {
	dir := t.TempDir()

	key, err := LoadOrCreate("", dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(key) < creds.MinKeyBytes {
		t.Fatalf("created a %d-byte key, need at least %d", len(key), creds.MinKeyBytes)
	}
	if _, err := creds.NewSigner(key); err != nil {
		t.Fatalf("the created key is not usable as a signer: %v", err)
	}
}

// A key file readable by anyone on the box is the whole deployment's
// credentials, and a mode is the kind of thing that regresses in a refactor
// without any behaviour changing.
func TestTheCreatedKeyIsNotReadableByOthers(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")

	if _, err := LoadOrCreate("", dir); err != nil {
		t.Fatalf("load: %v", err)
	}
	fi, err := os.Stat(Path(dir))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file is %04o, want 0600", perm)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("state dir is %04o, want 0700", perm)
	}
}

// A short key is refused rather than used. HMAC-SHA256 tolerates any length, so
// nothing downstream fails loudly -- it is simply weak, silently, forever
// (creds.MinKeyBytes). Both services, because both accept the flag.
func TestAShortKeyIsRefused(t *testing.T) {
	short := strings.Repeat("k", creds.MinKeyBytes-1)

	for name, load := range map[string]func(string, string) ([]byte, error){
		"control plane": LoadOrCreate,
		"relay":         Load,
	} {
		if _, err := load(short, ""); err == nil {
			t.Errorf("%s: accepted a %d-byte -signing-key", name, len(short))
		}
	}

	// And the same rule for a key that arrived as a file, where the message has
	// to carry the path -- an operator who cannot see WHICH file is short has
	// nothing to act on.
	dir := t.TempDir()
	if err := os.WriteFile(Path(dir), []byte(short), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, load := range map[string]func(string, string) ([]byte, error){
		"control plane": LoadOrCreate,
		"relay":         Load,
	} {
		_, err := load("", dir)
		if err == nil {
			t.Errorf("%s: accepted a short key file", name)
			continue
		}
		if !strings.Contains(err.Error(), Path(dir)) {
			t.Errorf("%s: refusal does not name the file: %v", name, err)
		}
	}
}

// An explicit key wins over the state directory, on both services. That
// ordering is what makes production work: the key comes from a secret store,
// and a state directory that happens to exist beside the binary must not
// silently take precedence over it.
func TestAnExplicitKeyWinsOverTheStateDir(t *testing.T) {
	dir := t.TempDir()
	onDisk := strings.Repeat("d", creds.MinKeyBytes)
	if err := os.WriteFile(Path(dir), []byte(onDisk), 0o600); err != nil {
		t.Fatal(err)
	}
	explicit := strings.Repeat("e", creds.MinKeyBytes)

	for name, load := range map[string]func(string, string) ([]byte, error){
		"control plane": LoadOrCreate,
		"relay":         Load,
	} {
		key, err := load(explicit, dir)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if string(key) != explicit {
			t.Errorf("%s: used the file instead of -signing-key", name)
		}
	}
}

// No state dir and no key is the one case where the control plane generates
// something ephemeral: nothing was durable anyway. Two calls therefore differ,
// which is exactly the failure TestTheSameStateDirYieldsTheSameKey guards
// against -- so it is pinned here as deliberate rather than left to look like
// the same bug.
func TestWithoutAStateDirTheKeyIsEphemeral(t *testing.T) {
	first, err := LoadOrCreate("", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreate("", "")
	if err != nil {
		t.Fatal(err)
	}
	if string(first) == string(second) {
		t.Fatal("two ephemeral keys matched, which cannot happen by chance")
	}
}
