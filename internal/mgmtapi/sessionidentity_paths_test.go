package mgmtapi

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The homes root is ONE fact in TWO files: the Management API decides a member's
// home and announces it on the control tunnel, and image/entrypoint.sh creates
// the directory those homes go in and denies it by path in the managed
// permissions. Nothing compared them.
//
// Disagreement is silent in the worst direction. The supervisor creates a home
// wherever it is told, so sessions keep working -- while the managed `deny` rule
// still names the OTHER path, which means the guardrail that stops an agent
// reading a colleague's home is pointed at an empty directory.
var entrypointHomesRoot = regexp.MustCompile(`(?m)^HOMES_ROOT=\$\{LEMUL_HOMES_ROOT:-([^}]+)\}`)

func TestTheHomesRootMatchesTheImage(t *testing.T) {
	b, err := os.ReadFile("../../image/entrypoint.sh")
	if err != nil {
		t.Skipf("entrypoint not readable from here: %v", err)
	}

	m := entrypointHomesRoot.FindStringSubmatch(string(b))
	if m == nil {
		t.Fatal("could not find HOMES_ROOT's default in entrypoint.sh; " +
			"this test is now checking nothing")
	}
	if m[1] != homesRoot {
		t.Errorf("entrypoint.sh lays out %q and denies it in managed permissions, "+
			"while the control plane places homes in %q -- so the deny rule guards "+
			"a directory nobody uses", m[1], homesRoot)
	}
}

// Everything durable sits under one mount point, which on Fargate is where the
// workspace's EBS volume is attached. ECS creates that volume per task and
// deletes it when the task exits, so carrying a workspace forward means
// snapshotting the mount -- and a home outside it would survive nothing, which
// is the failure the comment on homesRoot exists to prevent.
func TestHomesAreOnTheWorkspaceMount(t *testing.T) {
	const workspaceMount = "/workspace"
	if !strings.HasPrefix(homesRoot, workspaceMount+"/") {
		t.Errorf("homesRoot is %q, outside %q", homesRoot, workspaceMount)
	}
}
