package docker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lufzle/lemul/internal/driver"
)

// THE rule, and the one that is invisible when it is wrong.
//
// /workspace holds the homes root and the shared directory (section 2.3). One
// volume shared by every local workspace works perfectly with one workspace and
// silently puts two on one filesystem the moment there are two -- each member's
// home visible to the other workspace's members, which is exactly the boundary
// a workspace is.
func TestEachWorkspaceGetsItsOwnVolume(t *testing.T) {
	d := &Driver{VolumePrefix: "lemul-vol-"}

	first := d.volumeMount(driver.Spec{WorkspaceID: "w1"})
	second := d.volumeMount(driver.Spec{WorkspaceID: "w2"})

	if first == second {
		t.Fatalf("two workspaces share %q", first)
	}
	for _, m := range []string{first, second} {
		if !strings.HasSuffix(m, ":/workspace") {
			t.Errorf("mount %q does not land at /workspace", m)
		}
		if !strings.HasPrefix(m, "lemul-vol-") {
			t.Errorf("mount %q is not a prefixed named volume", m)
		}
	}
}

// A NAMED VOLUME, never a host path, and this is the assertion that says so.
//
// Measured on 2026-08-02: over a host bind mount on macOS the entrypoint's
// `chgrp lemul` and `setfacl` both fail silently, leaving the shared directory
// 775 root:root with no default ACL -- the exact state that made it unwritable
// for a phase -- and every member's home lands root:root with access allowed
// regardless, so "can Bob read Alice's transcript?" answers ALLOWED locally and
// DENIED on real storage. A named volume is real ext4 and carries all three.
//
// The failure this guards against does not look like a bug. It looks like a
// local workspace that works.
func TestTheWorkspaceMountIsNotAHostPath(t *testing.T) {
	d := &Driver{VolumePrefix: "lemul-vol-"}

	mount := d.volumeMount(driver.Spec{WorkspaceID: "w1"})

	host := strings.TrimSuffix(mount, ":/workspace")
	if strings.ContainsRune(host, os.PathSeparator) {
		t.Errorf("the workspace is mounted from %q, which docker reads as a host "+
			"path -- on macOS that is virtiofs, which carries neither uid, nor the "+
			"setgid bit, nor ACLs", host)
	}
	if filepath.IsAbs(host) {
		t.Errorf("%q is an absolute path, so this is a bind mount", host)
	}
}

// The same workspace twice is the same volume, ACROSS GENERATIONS. A new
// generation is a REPLACEMENT task for the same workspace, so keying this on the
// idempotency key -- which is what the container name uses, and the obvious
// thing to reach for -- would hand every replacement an empty disk. That is the
// failure section 12.10 puts the homes on a volume to prevent, reintroduced by
// the local driver.
func TestAReplacementTaskReturnsToTheSameVolume(t *testing.T) {
	d := &Driver{VolumePrefix: "lemul-vol-"}

	first := d.volumeMount(driver.Spec{WorkspaceID: "w1", Generation: 1})
	second := d.volumeMount(driver.Spec{WorkspaceID: "w1", Generation: 2})

	if first != second {
		t.Errorf("generation 2 mounted %q, generation 1 mounted %q", second, first)
	}
}

// No prefix configured means no volume at all, and the container filesystem is
// the workspace. That is what the e2e suite uses: a test that asserts what a
// workspace holds should start from an empty one rather than from whatever the
// last run left behind.
func TestNoVolumePrefixMountsNothing(t *testing.T) {
	d := &Driver{}

	if mount := d.volumeMount(driver.Spec{WorkspaceID: "w1"}); mount != "" {
		t.Errorf("mounted %q with no VolumePrefix configured", mount)
	}
}

// A workspace id reaches a volume NAME here. Ids are uuids in the product but
// "w1" in the suite and whatever an operator names by hand, so the same
// sanitising the container name gets applies -- a separator in one would turn
// the named volume into a host path, which is the one thing this must never be.
func TestAWorkspaceIdCannotTurnTheVolumeIntoAHostPath(t *testing.T) {
	d := &Driver{VolumePrefix: "lemul-vol-"}

	mount := d.volumeMount(driver.Spec{WorkspaceID: "../../etc"})

	host := strings.TrimSuffix(mount, ":/workspace")
	if strings.ContainsRune(host, os.PathSeparator) {
		t.Errorf("a workspace id made the mount source %q, which docker reads as "+
			"a host path", host)
	}
}
