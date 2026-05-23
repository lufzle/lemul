package supervisor

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The shared directory's path is ONE fact held in TWO files: image/entrypoint.sh
// creates it and renders it into the managed permissions, and identity.go
// pre-trusts it in every member's first-run state. Nothing compared them.
//
// That is the shape of the bug closed the same day this was written -- a value
// configured independently in two places, each perfectly valid alone, with no
// symptom when they disagree. Here the symptom would be especially quiet:
// Claude Code opens its trust dialog on a directory that was not pre-trusted, so
// a member's first access to the real shared directory prompts, in the middle of
// whatever they were doing, and nothing anywhere names the cause.
//
// The shared GROUP already had this treatment. The path is the half that
// matters more, because it is the one that reaches seedFirstRun.
var entrypointSharedDir = regexp.MustCompile(`(?m)^SHARED_DIR=\$\{LEMUL_SHARED_DIR:-([^}]+)\}`)

func TestTheSharedDirectoryMatchesTheImage(t *testing.T) {
	b, err := os.ReadFile("../../image/entrypoint.sh")
	if err != nil {
		t.Skipf("entrypoint not readable from here: %v", err)
	}

	m := entrypointSharedDir.FindStringSubmatch(string(b))
	if m == nil {
		t.Fatal("could not find SHARED_DIR's default in entrypoint.sh; " +
			"this test is now checking nothing")
	}
	if m[1] != sharedDir {
		t.Errorf("entrypoint.sh creates %q, the supervisor pre-trusts %q. "+
			"They are one directory, so a member's first access to the real one "+
			"opens the trust dialog and nothing says why", m[1], sharedDir)
	}
}

// Everything durable has to sit under ONE mount point, and this is the rule that
// says so in code rather than in a comment somebody has to find.
//
// /workspace is where the workspace's storage is attached -- an EBS volume on
// Fargate, which ECS creates per task and deletes when the task exits, so
// carrying a workspace forward means snapshotting that mount and launching the
// replacement from the snapshot. A directory outside it is therefore not merely
// at a different path, it has a different DURABILITY: it survives nothing, and a
// snapshot of the workspace does not contain it.
//
// The shared directory sat at /shared for a whole phase for exactly this reason
// and nothing noticed, because with no volume configured at all the two are
// equally ephemeral and behave identically. This fails the moment somebody moves
// it back out.
func TestTheSharedDirectoryIsOnTheWorkspaceMount(t *testing.T) {
	if !strings.HasPrefix(sharedDir, workspaceMount+"/") {
		t.Errorf("sharedDir is %q, which is outside %q -- so it survives no task "+
			"replacement and no snapshot of the workspace contains it",
			sharedDir, workspaceMount)
	}
}
