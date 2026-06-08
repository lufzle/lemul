package e2e_test

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lufzle/lemul/internal/driver/docker"
)

// The isolation a shared workspace rests on, against the real image.
//
// Every property here is a POSIX one -- uid, mode 0700, a setgid directory and a
// default ACL -- and none of it is expressible in a unit test, because there is
// nothing to assert about it except what the kernel does. So this runs the same
// arrangement the supervisor builds (internal/supervisor/identity.go) inside the
// real sandbox image and asks the kernel.
//
// It is here rather than in a Go-level test of provisionHome for a reason worth
// keeping: provisionHome no-ops unless it is root, so a test of it running as an
// ordinary user proves that it does nothing. The mechanism only exists as root
// in a container.
//
// Written after a manual run of this matrix found the half that was missing:
// home isolation was right from the start, while NO member could write /shared,
// because useradd never added them to the group that owns it. A shared workspace
// where the shared directory is read-only is not one.
func TestMembersAreIsolatedInAWorkspaceImage(t *testing.T) {
	requireDocker(t)

	// Mirrors provisionHome plus the entrypoint's directory layout. Kept as one
	// script so the arrangement under test is visible in one place.
	const script = `
set -e
groupadd lemul 2>/dev/null || true
mkdir -p /workspace/shared && chgrp lemul /workspace/shared && chmod 2775 /workspace/shared
setfacl -d -m g:lemul:rwx /workspace/shared
mkdir -p /workspace/homes
for u in 2000 2001; do
  useradd -u $u -M -d /workspace/homes/lem$u -s /bin/bash -G lemul lem$u
  mkdir -p /workspace/homes/lem$u/.claude
  chown $u:$u /workspace/homes/lem$u /workspace/homes/lem$u/.claude
  chmod 700 /workspace/homes/lem$u
  echo "conversation-of-$u" > /workspace/homes/lem$u/.claude/transcript.jsonl
  chown $u:$u /workspace/homes/lem$u/.claude/transcript.jsonl
done

check() {
  label=$1; user=$2; cmd=$3
  if su "$user" -s /bin/sh -c "$cmd" >/dev/null 2>&1; then echo "$label=ALLOWED"; else echo "$label=DENIED"; fi
}
check own_home           lem2000 "cat /workspace/homes/lem2000/.claude/transcript.jsonl"
check others_transcript  lem2000 "cat /workspace/homes/lem2001/.claude/transcript.jsonl"
check others_home_listing lem2000 "ls /workspace/homes/lem2001"
check shared_write       lem2000 "echo hi > /workspace/shared/a.txt"
check shared_cross_edit  lem2001 "echo more >> /workspace/shared/a.txt"
check supervisor_environ lem2000 "cat /proc/1/environ"
`
	out := dockerRun(t, script)

	// ALLOWED and DENIED are both assertions here. A test that only checked the
	// denials would pass on an image where nothing worked at all.
	for _, want := range []struct{ key, expect, why string }{
		{"own_home", "ALLOWED",
			"a member cannot read their own conversation"},
		{"others_transcript", "DENIED",
			"one member can read another's Claude Code conversation off disk, " +
				"which is the API-level rule in section 2.5 walked around one layer down"},
		{"others_home_listing", "DENIED",
			"one member can enumerate another's home"},
		{"shared_write", "ALLOWED",
			"no member can write /shared, so the workspace shares nothing"},
		{"shared_cross_edit", "ALLOWED",
			"a member cannot edit a colleague's file in /shared -- setgid alone " +
				"leaves new files 0644, and the default ACL is what fixes the mode"},
		{"supervisor_environ", "DENIED",
			"a session can read the supervisor's environment, which holds the " +
				"gateway credential (section 12.6)"},
	} {
		got := want.key + "=" + want.expect
		if !strings.Contains(out, got) {
			t.Errorf("%s: expected %s\n%s", want.why, got, out)
		}
	}
}

// workspaceImage names the sandbox image, overridable the same way the gateway
// test does it.
func workspaceImage() string { return envOr("LEMUL_E2E_IMAGE", "lemul-workspace:dev") }

func requireDocker(t *testing.T) {
	t.Helper()
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker is not available")
	}
	out, err := exec.Command("docker", "image", "inspect", workspaceImage()).CombinedOutput()
	if err != nil {
		t.Skipf("%s is not built (image/build.sh): %s", workspaceImage(), out)
	}
}

func dockerRun(t *testing.T, script string) string {
	t.Helper()
	cmd := exec.Command("docker", "run", "--rm", "--entrypoint", "sh", workspaceImage(), "-c", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	return string(out)
}

// Section 2.4's fourth idle condition, against the real image.
//
// "No tool executing" is what stops a one-hour build being reaped, since a build
// produces no PTY output and no model calls while it runs. The supervisor tells
// a tool from a startup child by START TIME (supervisor/activity.go), and that
// is an empirical claim about what this image spawns -- so it is asserted here
// rather than only in a unit test over hand-built ProcInfo values.
//
// What this run established, on 2026-08-02 against the pinned 2.1.220:
//
//   - a real interactive `claude` spawns NO long-lived children of its own, so
//     everything under a session is either the session or a tool;
//   - a late child is comfortably outside the settle window, which is the case
//     the condition exists for.
//
// The other half of the trap is pinned separately and in the other direction:
// supervisor.TestMCPServersWouldBreakToolDetection fails if the image ever gains
// an MCP server, because 2.1.220 starts those LAZILY and start time stops
// separating them from tools at that point.
func TestARealSessionSpawnsNoLongLivedChildren(t *testing.T) {
	requireDocker(t)

	// Mirrors what the supervisor sets up: a member's own config directory with
	// first-run state seeded, so claude reaches its prompt instead of the
	// onboarding wizard. The gateway is deliberately unreachable -- the process
	// tree at the prompt is what is under test, not a completed turn.
	const script = `
set -e
H=/workspace/homes/lem2000
mkdir -p $H/.claude
cat > $H/.claude/.claude.json <<'J'
{"hasCompletedOnboarding":true,"theme":"dark","hasTrustDialogAccepted":true,
 "projects":{"/workspace/homes/lem2000":{"hasTrustDialogAccepted":true,
 "hasCompletedProjectOnboarding":true}}}
J
cd $H
CLAUDE_CONFIG_DIR=$H/.claude ANTHROPIC_BASE_URL=http://127.0.0.1:9/ \
  ANTHROPIC_AUTH_TOKEN=sk-placeholder script -q -c claude /dev/null >/tmp/cc.log 2>&1 &

# Well past the 15 s settle window, so anything Claude Code starts for itself
# has had time to appear.
sleep 25
CC=$(pgrep -n -x claude || echo none)
echo "claude_pid=$CC"
# Descendants of claude, excluding itself.
echo "descendants=$(pgrep -P "$CC" 2>/dev/null | wc -l | tr -d ' ')"

# Now a tool: a child started long after the session, which is exactly the shape
# of a build. Its age must be far below claude's for start time to separate them.
sleep 300 &
TOOL=$!
sleep 2
echo "claude_age=$(ps -o etimes= -p "$CC" | tr -d ' ')"
echo "tool_age=$(ps -o etimes= -p "$TOOL" | tr -d ' ')"
`
	out := dockerRun(t, script)

	if strings.Contains(out, "claude_pid=none") {
		t.Fatalf("claude did not start in the image:\n%s", out)
	}
	// The precondition for start time being usable at all. If Claude Code ever
	// starts a long-lived child of its own, this is where it shows up -- and the
	// settle window would then need the same argv treatment MCP servers will.
	if !strings.Contains(out, "descendants=0") {
		t.Errorf("a real session spawned long-lived children of its own.\n"+
			"Section 2.4's tool check assumes anything under a session is a tool; "+
			"a persistent child breaks that the same way an MCP server does.\n%s", out)
	}

	claudeAge := ageFrom(t, out, "claude_age=")
	toolAge := ageFrom(t, out, "tool_age=")
	if claudeAge < 25 {
		t.Errorf("claude_age=%d, expected the session to be well past the settle "+
			"window by now:\n%s", claudeAge, out)
	}
	// The distinction the condition rests on: a tool is much younger than its
	// session, so a start-time comparison separates them with room to spare.
	if toolAge > 15 {
		t.Errorf("tool_age=%d is inside the 15 s settle window, so a real tool "+
			"would be mistaken for a startup child and a build would be reaped:\n%s",
			toolAge, out)
	}
}

func ageFrom(t *testing.T, out, key string) int {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), key); ok {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				t.Fatalf("parsing %s%q: %v", key, v, err)
			}
			return n
		}
	}
	t.Fatalf("no %s in output:\n%s", key, out)
	return 0
}

// MCP is OFF, and enforced rather than merely unconfigured.
//
// This is a precondition for section 2.4's idle detection, not a permissions
// preference. An MCP server is a long-lived child of a session and 2.1.220
// starts them LAZILY, so one running under a session reads as a tool that never
// finishes -- the session is then never idle-stopped, the workspace's last
// session never goes, warm hold never starts, and the task bills indefinitely.
//
// It takes two things that are useless apart: `allowManagedMcpServersOnly` in
// the managed settings, and an empty allowlist in managed-mcp.json. The image
// shipped the allowlist alone until 2026-08-02 -- at the wrong filename, so it
// was inert twice over -- and a member could add a server to their own config
// unopposed.
//
// So this runs the real entrypoint and asks Claude Code, rather than asserting
// over the files. Both member-controlled routes are exercised: user scope in
// their own config, and a project .mcp.json.
func TestMCPServersAreRefusedInTheImage(t *testing.T) {
	requireDocker(t)

	const script = `
set -e
export CLAUDE_CONFIG_DIR=/root/cfg; mkdir -p $CLAUDE_CONFIG_DIR /workspace
printf '#!/bin/sh\nexec sleep 100000\n' > /usr/local/bin/probe-mcp
chmod +x /usr/local/bin/probe-mcp

# Both routes a member actually controls.
cat > $CLAUDE_CONFIG_DIR/.claude.json <<'J'
{"hasCompletedOnboarding":true,"enableAllProjectMcpServers":true,
 "mcpServers":{"user_scope":{"type":"stdio","command":"/usr/local/bin/probe-mcp","args":[],"env":{}}}}
J
cat > /workspace/.mcp.json <<'J'
{"mcpServers":{"project_scope":{"type":"stdio","command":"/usr/local/bin/probe-mcp","args":[],"env":{}}}}
J

# The REAL entrypoint renders the managed settings, so this exercises the
# shipped path rather than a hand-written file. It ends by exec-ing the
# supervisor, which would dial a control plane forever -- stubbed, so everything
# above that exec still runs exactly as it does in a real task.
printf '#!/bin/sh\nexit 0\n' > /usr/local/bin/supervisor
LEMUL_WORKSPACE_ID=probe LEMUL_SESSION_UID=1000 entrypoint.sh >/dev/null 2>&1 || true
grep -q allowManagedMcpServersOnly /etc/claude-code/managed-settings.json \
  && echo "flag=SET" || echo "flag=MISSING"

cd /workspace
timeout 90 claude mcp list 2>&1 | grep -qiE 'user_scope|project_scope' \
  && echo "mcp=REACHABLE" || echo "mcp=REFUSED"
`
	out := dockerRun(t, script)

	if !strings.Contains(out, "flag=SET") {
		t.Errorf("the entrypoint did not write allowManagedMcpServersOnly:\n%s", out)
	}
	if !strings.Contains(out, "mcp=REFUSED") {
		t.Errorf("a member's own MCP server was reachable in the real image.\n"+
			"Section 2.4's tool check assumes no MCP server can run; one that "+
			"can starts lazily, never exits, and switches idle detection off for "+
			"that session -- so the workspace never stops.\n%s", out)
	}
}

// THE LAYER THE TEST ABOVE CANNOT SEE: the credential the supervisor actually
// forks a session with.
//
// TestMembersAreIsolatedInAWorkspaceImage asserts the same properties and
// passed throughout the entire time /shared was broken for every member on
// every driver. It runs its checks through `su`, and su initialises
// supplementary groups from /etc/group -- so it proves the IMAGE is set up
// correctly and says nothing about the process the product forks. That process
// was getting syscall.Credential{Uid, Gid} with no Groups, and Go runs
// setgroups() in the child whenever a Credential is set, which CLEARS the
// member's supplementary groups rather than inheriting them. The member fell
// through 2775 root:lemul to `other` -- r-x -- and could not write a byte.
//
// So the two tests decompose the claim rather than duplicating it: that one
// says the image is right, this one says the session gets what the image
// grants. Neither is sufficient alone, and the gap between them is exactly
// where the bug lived.
//
// It needs the DOCKER driver, and that is not a preference. sessionCredential
// returns nil unless the supervisor is root, which it is only inside the image
// -- on the local driver there is no credential to get wrong, so this would
// pass by never exercising the mechanism.
func TestASessionPTYCarriesTheSharedGroup(t *testing.T) {
	requireDocker(t)
	image := workspaceImage()
	s := newStackWith(t, stackOptions{
		// A shell, because what is under test is the credential the PTY is
		// forked with -- and a shell reports it directly. Claude Code would
		// need a reachable gateway to prove anything at all.
		SessionCmd: []string{"sh"},
		Driver:     docker.New(image, nil),
		Image:      image,
	})

	sid := s.newSession("shared-group")
	c := s.attach(sid, 24, 80, "")

	// `id` first: it names the failure directly if the group is missing, rather
	// than leaving a bare permission error to be interpreted.
	c.writeBytes([]byte("id; echo probe > /workspace/shared/probe.txt; echo rc=$?\r"))
	got := c.await("rc=", 60*time.Second)

	if !strings.Contains(got, "("+sharedGroupName+")") {
		t.Errorf("the session's process is not in the %s group.\n"+
			"setgroups() in the child clears supplementary groups when "+
			"Credential.Groups is empty, so /shared (2775 root:%s) falls "+
			"through to `other` and no member can write to it.\nscreen:\n%s",
			sharedGroupName, sharedGroupName, tail(got, 600))
	}
	if !strings.Contains(got, "rc=0") {
		t.Errorf("a session could not write /shared, which is the whole point "+
			"of a shared workspace.\nscreen:\n%s", tail(got, 600))
	}
}

// sharedGroupName mirrors supervisor.sharedGroup. Duplicated rather than
// exported: this suite is a black-box client of the image, and a test that
// imported the constant would still pass if both moved together.
const sharedGroupName = "lemul"
