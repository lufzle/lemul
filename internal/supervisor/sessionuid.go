package supervisor

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// sessionCredential decides what uid a session's process runs at.
//
// The supervisor holds the gateway credential in its own environment, so the
// thing that keeps a session from reading it back out of /proc is a uid
// difference and nothing else (section 12.6, measured). childEnv removes the
// variable from the child's environment block; this removes the child's ability
// to go and find it anywhere else.
//
// Two conditions, both required:
//
//   - a uid is configured. Zero means the boundary is off, which is what a
//     workspace image built before this existed will report.
//   - this process is root. Only root can change uid, and a supervisor that is
//     already unprivileged has nothing to drop from -- that is the local driver
//     and the e2e suite, where supervisor and session are the same person
//     already and the gateway key, if any, is the developer's own.
//
// Refusing to run rather than silently continuing as root is deliberate: a
// misconfigured uid would otherwise produce a workspace that looks correct and
// leaks, which is the failure mode a security control must not have.
func sessionCredential(uid, gid uint32) (*syscall.Credential, error) {
	if uid == 0 {
		return nil, nil
	}
	if os.Geteuid() != 0 {
		// Not root: nothing to drop. Say so once at startup rather than per
		// session, so a developer is not told off on every attach.
		return nil, nil
	}
	return credentialFor(uid, gid, lookupGroupID), nil
}

// credentialFor builds the credential a session's process is forked with.
//
// Split out from the root check above so it can be tested WITHOUT root, and
// that is not tidiness -- it is the fix for why this was broken. The only
// tests of this code were gated on `os.Geteuid() == 0`, so on every developer
// machine and in CI they skipped, and the one field that mattered was asserted
// by nothing anywhere. A security-shaped function that can only be tested as
// root will, in practice, not be tested.
//
// The group lookup is injected for the same reason: the shared group exists in
// the workspace image and not on the machine running the tests.
func credentialFor(uid, gid uint32, lookupGID func(string) (uint32, error)) *syscall.Credential {
	if gid == 0 {
		gid = uid
	}
	// SUPPLEMENTARY GROUPS HAVE TO BE SET HERE, and the empty case is not a
	// no-op. Go runs setgroups() in the child whenever Credential is set and
	// NoSetGroups is false, so leaving Groups nil CLEARS every supplementary
	// group the member has -- it does not inherit them.
	//
	// That is what made /shared unwritable by anybody. useradd puts the member
	// in `lemul` (identity.go) and /etc/group agrees, but the PTY process was
	// spawned with no supplementary groups at all, so a member fell through
	// 2775 root:lemul to `other` -- r-x. They could see /shared and everything
	// in it and could not write a byte, which is the exact failure the -G flag
	// was added to prevent, reintroduced one layer down.
	//
	// It survived because nothing tested this layer: e2e/isolation_test.go runs
	// its checks through `su`, and su initialises supplementary groups from
	// /etc/group. So the image was right, the test was right about the image,
	// and the process the product actually forks was never the thing under
	// test. Found by two people using a real workspace.
	cred := &syscall.Credential{Uid: uid, Gid: gid}
	// Group 0 is refused rather than trusted. A lookup that resolved the shared
	// group to 0 -- a malformed /etc/group, a stub resolver -- would put every
	// session in the ROOT group, turning a fix for a missing permission into a
	// granted one. Caught by its own test rather than by review.
	if sharedGID, err := lookupGID(sharedGroup); err == nil && sharedGID != 0 {
		cred.Groups = []uint32{sharedGID}
	} else if err == nil {
		slog.Warn("shared group resolved to gid 0; refusing to put sessions in the root group",
			"group", sharedGroup)
	} else {
		// Not fatal. A workspace whose image predates the shared group still
		// has to start; what it loses is /shared, not isolation -- and the uid
		// boundary, which is the security property, is unaffected either way.
		slog.Warn("shared group not found; /shared will not be writable by sessions",
			"group", sharedGroup, "error", err)
	}
	return cred
}

// lookupGroupID resolves a group name to its gid.
//
// os/user rather than parsing /etc/group by hand: the pure-Go implementation
// already reads that file, and the cgo one asks nsswitch, so this keeps working
// if a future image resolves groups anywhere but a flat file.
func lookupGroupID(name string) (uint32, error) {
	g, err := user.LookupGroup(name)
	if err != nil {
		return 0, err
	}
	id, err := strconv.ParseUint(g.Gid, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("group %s has a non-numeric gid %q: %w", name, g.Gid, err)
	}
	return uint32(id), nil
}

// writableCheckFlag runs this binary as a one-shot write probe. See
// checkSessionDir.
const writableCheckFlag = "-writable-check"

// writableCheckTimeout bounds the probe. It writes one small file, so anything
// approaching this is a wedged filesystem rather than a slow one -- which is
// itself worth reporting as "not writable".
const writableCheckTimeout = 10 * time.Second

// RunWritableCheck is the probe half: create and remove a file in dir, and
// report success as an exit code. Called only by checkSessionDir's fork.
func RunWritableCheck(dir string) error {
	f, err := os.CreateTemp(dir, ".lemul-writable-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// checkSessionDir reports whether a directory the session must write is
// actually writable at the session's uid.
//
// Worth checking at startup because the failure is otherwise silent and late:
// Claude Code starts, cannot write its conversation, and the symptom surfaces as
// a session that will not resume rather than as a permissions problem.
//
// It forks a probe at the session's credential rather than reading mode bits,
// because mode bits lie. The first version of this reasoned from st_uid and the
// permission mask and reported /workspace as unwritable on every docker-driver
// workspace -- a macOS bind mount presents 0755 root:root and then honours the
// write anyway. A warning that is wrong on the common path is worse than no
// warning, because it teaches people to skip the one that is right.
func checkSessionDir(path string, cred *syscall.Credential) error {
	if cred == nil || path == "" {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	self, err := os.Executable()
	if err != nil {
		return nil // cannot probe; say nothing rather than guess
	}
	// Bounded, because this runs during startup: a probe that never exits would
	// stop the supervisor registering its tunnel at all, and the workspace would
	// look like it failed to place rather than like a stuck write check.
	ctx, cancel := context.WithTimeout(context.Background(), writableCheckTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, writableCheckFlag, path)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s is not writable by the session uid %d:%d "+
			"(the image entrypoint should chown it): %s",
			path, cred.Uid, cred.Gid, strings.TrimSpace(string(out)))
	}
	return nil
}
