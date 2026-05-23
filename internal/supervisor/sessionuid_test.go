package supervisor

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The boundary is off unless a uid is configured AND this process is root.
// Both halves matter: the first is how an older image reports itself, the second
// is the local driver and the e2e suite, where dropping privilege is neither
// possible nor useful.
func TestSessionCredentialRequiresUIDAndRoot(t *testing.T) {
	cred, err := sessionCredential(0, 0)
	if err != nil {
		t.Fatalf("uid 0: %v", err)
	}
	if cred != nil {
		t.Fatalf("uid 0 should disable the boundary, got %+v", cred)
	}

	cred, err = sessionCredential(1000, 1000)
	if err != nil {
		t.Fatalf("uid 1000: %v", err)
	}
	if os.Geteuid() == 0 {
		if cred == nil {
			t.Fatal("running as root: expected a credential")
		}
		if cred.Uid != 1000 || cred.Gid != 1000 {
			t.Fatalf("got %d:%d, want 1000:1000", cred.Uid, cred.Gid)
		}
	} else if cred != nil {
		t.Fatalf("not root: expected no credential, got %+v", cred)
	}
}

// A gid of zero follows the uid rather than meaning "group root", which would
// hand every session a privileged group by omission.
func TestSessionCredentialDefaultsGIDToUID(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to produce a credential")
	}
	cred, err := sessionCredential(1000, 0)
	if err != nil {
		t.Fatalf("sessionCredential: %v", err)
	}
	if cred.Gid != 1000 {
		t.Fatalf("gid = %d, want it to follow the uid", cred.Gid)
	}
}

func TestCheckSessionDirSkipsWithoutABoundary(t *testing.T) {
	dir := t.TempDir()
	// No credential means no boundary, so there is nothing to check and nothing
	// to fork.
	if err := checkSessionDir(dir, nil); err != nil {
		t.Fatalf("nil credential should pass: %v", err)
	}
	if err := checkSessionDir("", &syscall.Credential{Uid: 1000}); err != nil {
		t.Fatalf("empty path should pass: %v", err)
	}
}

func TestCheckSessionDirReportsAMissingDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	if err := checkSessionDir(missing, &syscall.Credential{Uid: 1000}); err == nil {
		t.Fatal("expected an error for a missing directory")
	}
}

// The probe half, which is what makes the check trustworthy: it answers by
// writing rather than by reading mode bits, because a bind mount reports 0755
// root:root and then honours the write anyway.
func TestRunWritableCheck(t *testing.T) {
	dir := t.TempDir()
	if err := RunWritableCheck(dir); err != nil {
		t.Fatalf("writable dir should pass: %v", err)
	}
	// It must leave nothing behind -- it runs on every task start.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("probe left %d file(s) behind: %v", len(entries), entries)
	}

	if err := RunWritableCheck(filepath.Join(dir, "nope")); err == nil {
		t.Fatal("expected an error for a missing directory")
	}

	if os.Getuid() != 0 {
		ro := filepath.Join(dir, "ro")
		if err := os.Mkdir(ro, 0o500); err != nil {
			t.Fatal(err)
		}
		if err := RunWritableCheck(ro); err == nil {
			t.Fatal("expected an error for a read-only directory")
		}
	}
}

// THE bug, as a test: a session's process must carry the shared group.
//
// Go runs setgroups() in the child whenever a Credential is set, so an empty
// Groups CLEARS every supplementary group rather than inheriting one. That made
// /shared -- the entire point of a shared workspace -- readable and unwritable
// by every member, on every driver, while useradd and /etc/group both said the
// member was in the group.
//
// Two things about this test are the actual fix. It does NOT require root, so
// it runs everywhere instead of skipping on every developer machine as the two
// tests above do; and it asserts the credential the supervisor FORKS WITH,
// which is the layer e2e/isolation_test.go cannot see -- that one runs its
// checks through `su`, and su initialises supplementary groups from /etc/group,
// so the image passes while the product does not.
func TestSessionCredentialCarriesTheSharedGroup(t *testing.T) {
	const sharedGID = 1001
	cred := credentialFor(2000, 2000, func(name string) (uint32, error) {
		if name != sharedGroup {
			t.Errorf("looked up group %q, want %q", name, sharedGroup)
		}
		return sharedGID, nil
	})
	if cred.Uid != 2000 || cred.Gid != 2000 {
		t.Fatalf("got %d:%d, want 2000:2000", cred.Uid, cred.Gid)
	}
	if len(cred.Groups) == 0 {
		t.Fatal("no supplementary groups: setgroups() will CLEAR the member's " +
			"groups, so /shared (2775 root:lemul) falls through to `other` and " +
			"no member can write to it")
	}
	if cred.Groups[0] != sharedGID {
		t.Errorf("supplementary group %d, want the shared group %d", cred.Groups[0], sharedGID)
	}
}

// An image with no shared group still has to produce a working session.
//
// It loses /shared, which is a feature; it must not lose the uid boundary,
// which is the security property. Returning no credential here would run the
// session as ROOT, so the failure has to stay narrow.
func TestSessionCredentialSurvivesAMissingSharedGroup(t *testing.T) {
	cred := credentialFor(2000, 2000, func(string) (uint32, error) {
		return 0, errors.New("no such group")
	})
	if cred == nil {
		t.Fatal("a missing shared group dropped the credential entirely; " +
			"the session would run as root")
	}
	if cred.Uid != 2000 {
		t.Errorf("uid = %d, want 2000", cred.Uid)
	}
	if len(cred.Groups) != 0 {
		t.Errorf("groups = %v, want none when the group does not resolve", cred.Groups)
	}
}

// Group 0 is never a supplementary group for a session. Belt and braces: a
// lookup that returned zero would hand every member the root group, which is
// the one value that turns this fix into a privilege escalation.
func TestSessionCredentialRefusesGroupZero(t *testing.T) {
	cred := credentialFor(2000, 2000, func(string) (uint32, error) { return 0, nil })
	for _, g := range cred.Groups {
		if g == 0 {
			t.Fatal("group 0 was added as a supplementary group; every session " +
				"would be in the root group")
		}
	}
}
