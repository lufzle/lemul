package supervisor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// Who a session runs as inside the workspace task.
//
// A workspace is a shared machine (section 2.3): several members of one
// organization hold sessions in the same Fargate task, on the same filesystem.
// What keeps one member out of another's files is a POSIX uid and mode 0700 --
// not Claude Code's permission system, which governs what the AGENT chooses to
// touch and is a guardrail rather than a fence. A user with a Bash tool is a
// user with a shell.
//
// The uid arrives from the Management API over the control tunnel
// (tunnel.SessionIdentity). It is never derived here: the workspace task holds
// no record of who is in an organization, and a uid computed from something
// local would have to be recomputed after every placement, against a volume
// that outlived the task -- leaving every home owned by a stranger.

// identity is one member's presence inside this task, resolved once and reused
// by every session they start.
type identity struct {
	uid      uint32
	username string
	home     string
	// configDir is CLAUDE_CODE's CLAUDE_CONFIG_DIR: skills, plugins, MCP
	// servers, output style and the conversation transcripts, all per member.
	// Inside their home, so the same 0700 that protects their files protects
	// their conversations.
	configDir string
}

// credential is the uid/gid this member's processes fork at, or nil where there
// is no boundary to draw.
//
// Delegated to sessionCredential rather than built here, because the two
// conditions it encodes are load-bearing and easy to get wrong: uid 0 means the
// boundary is off, and a supervisor that is not root cannot change uid at all.
// Returning a credential in either case makes fork/exec fail with EPERM, which
// presents as every session refusing to start.
func (i identity) credential() (*syscall.Credential, error) {
	return sessionCredential(i.uid, i.uid)
}

// identities is the map the control tunnel fills and attach reads.
type identities struct {
	mu sync.Mutex
	m  map[string]identity
	// provisioned remembers which uids already have a home on this task, so the
	// useradd/mkdir/chown work happens once per member per placement rather than
	// on every session. Not persisted: /etc/passwd is container-local and a
	// replacement task starts without it, while the homes themselves are on the
	// workspace volume and survive.
	provisioned map[uint32]bool
}

func newIdentities() *identities {
	return &identities{m: map[string]identity{}, provisioned: map[uint32]bool{}}
}

func (s *identities) put(id identity, sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[sessionID] = id
}

func (s *identities) get(sessionID string) (identity, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.m[sessionID]
	return id, ok
}

// markProvisioned reports whether this uid still needs its home built, and
// records that it will not next time.
func (s *identities) markProvisioned(uid uint32) (first bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.provisioned[uid] {
		return false
	}
	s.provisioned[uid] = true
	return true
}

// provisionHome makes a member's home exist, owned by them and readable by
// nobody else.
//
// Runs as root, from the supervisor, rather than from the image's entrypoint.
// The entrypoint cannot do it: at container start nothing knows who the members
// are, and membership changes without the task restarting.
//
// A REAL passwd entry, not just HOME= in the environment. sessionHome resolves a
// uid through user.LookupId, and beyond that a workspace where `whoami` fails is
// one where git refuses to commit and a shell prints "I have no name!" -- all of
// which are the kind of failure that does not name its cause. Idempotent, and
// re-run per placement because /etc/passwd is container-local while the home is
// on the volume.
func provisionHome(id identity) error {
	if os.Geteuid() != 0 {
		// Not root: there is no boundary to build and nothing we could chown
		// anyway. The local driver and the e2e suite land here, where the
		// supervisor and the session are already the same person.
		return nil
	}
	if err := ensurePasswdEntry(id); err != nil {
		return err
	}
	// 0700: this is the fence. Everything else in this file is arrangement.
	//
	// Only the directories WE create are chowned, and deliberately not the tree
	// beneath them. A recursive chown here would run as root over a directory its
	// own member controls, and os.Chown follows symlinks -- so a symlink dropped
	// in a home would have us hand its target to that member on the next
	// placement. os.Lchown closes the obvious half, but the path is still walked
	// component by component while the member can swap one, and os.Root offers no
	// chown to do it race-free (Go 1.25).
	//
	// It is also unnecessary. The member writes into their own home AS
	// themselves, so what they create is already theirs; the only thing needing
	// an owner is what this function makes.
	for _, dir := range []string{id.home, id.configDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
		if err := os.Lchown(dir, int(id.uid), int(id.uid)); err != nil {
			return fmt.Errorf("chown %s: %w", dir, err)
		}
	}
	return seedFirstRun(id)
}

// ensurePasswdEntry adds the OS user if this task does not have it yet.
func ensurePasswdEntry(id identity) error {
	if _, err := os.Stat("/etc/passwd"); err != nil {
		return nil // not a Linux image; nothing to add to
	}
	// -M: do not create the home, because it lives on the workspace volume and
	// may already hold a previous task's files. -o is not needed; uids are
	// unique per workspace by construction (workspace_member's UNIQUE).
	// Bounded: useradd takes a lock on /etc/passwd, and a wedged one would
	// otherwise hold up every session start in this task rather than just its
	// own.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "useradd",
		"-u", strconv.FormatUint(uint64(id.uid), 10),
		"-M", "-d", id.home,
		"-s", "/bin/bash",
		// Membership of the shared group is what makes /shared writable.
		//
		// Without it the directory is still 2775 root:lemul and a member falls
		// through to `other`, which is r-x -- so they can SEE the shared
		// directory and everything in it and cannot write a byte. Found by
		// running two real uids against a real image rather than by reading:
		// home isolation looked right, and the half that makes a shared
		// workspace worth having was silently missing.
		"-G", sharedGroup,
		id.username)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	// Exit 9 is "name already in use" and 4 is "uid already in use". Both mean
	// somebody else already did this, which is the common case on the second
	// session, not a failure.
	if ee, ok := err.(*exec.ExitError); ok {
		switch ee.ExitCode() {
		case 4, 9:
			return nil
		}
	}
	return fmt.Errorf("useradd %s (uid %d): %w: %s", id.username, id.uid, err, out)
}

// seedFirstRun writes the state that stops Claude Code opening its setup wizard.
//
// Per member, which is the part that is easy to miss. The image's entrypoint
// used to do this once for one shared config directory; with a directory per
// member, every new member would meet the onboarding wizard and the trust
// dialog on their first attach -- in the middle of a demo, most likely.
//
// Only the two directories we create are pre-trusted. Anything a member makes
// beyond that prompts once, which is correct rather than a gap: we cannot
// pre-trust a path we do not know about, and /shared holds whatever the customer
// puts there.
func seedFirstRun(id identity) error {
	path := filepath.Join(id.configDir, ".claude.json")
	if _, err := os.Stat(path); err == nil {
		return nil // theirs now; never overwrite what they have changed
	}
	doc := fmt.Sprintf(`{
  "hasCompletedOnboarding": true,
  "theme": "dark",
  "projects": {
    %q: {"hasTrustDialogAccepted": true, "hasCompletedProjectOnboarding": true},
    %q: {"hasTrustDialogAccepted": true, "hasCompletedProjectOnboarding": true}
  }
}`, id.home, sharedDir)
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		return fmt.Errorf("seed %s: %w", path, err)
	}
	return os.Chown(path, int(id.uid), int(id.uid))
}

// sharedDir is the one directory every member of a workspace can read and write.
//
// What goes in it is the customer's business -- a repo, a dataset, build
// artifacts, a dev database socket, nothing. It exists because sharing a
// filesystem is why several sessions live in one task at all (section 2.3), and
// it is the only place that sharing happens now that homes are private.
//
// UNDER /workspace, which is the mount point everything durable lives on. This
// value and image/entrypoint.sh's SHARED_DIR are one fact in two files, so
// TestTheSharedDirectoryMatchesTheImage compares them -- the group below already
// had that treatment, and the path is the half that reaches seedFirstRun.
const sharedDir = "/workspace/shared"

// sharedGroup owns sharedDir, and every member is added to it.
//
// Mirrored from image/entrypoint.sh, which creates the group and the directory
// at container start; this is the half that puts people in it. The two have to
// agree, and TestTheSharedGroupMatchesTheImage says so.
const sharedGroup = "lemul"
