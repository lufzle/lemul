package supervisor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The workspace explorer lets an operator list directories inside a running
// task. This file is the reason that is safe to expose.
//
// The supervisor runs as ROOT -- image/Dockerfile carries no USER directive and
// the docker driver passes no --user -- so the operating system stops it reading
// nothing at all. Confinement here is policy, and policy is the only copy there
// is. What sits outside the workspace root and must never be reachable:
//
//   - /proc/self/environ, which holds the GATEWAY KEY. Section 12.4's entire
//     design is that no session can reach that credential; a path escape would
//     hand it to anyone who can call the API.
//   - /etc/claude-code/managed-settings.json, and every other host path a driver
//     bind-mounts into the container.
//
// Enforced here rather than in the control plane or the console because of the
// viewer-mode lesson in section 2.5: input-dropping was enforced at the relay
// and the supervisor and still had a third door. Only the innermost check
// counts, and this is the innermost.

var (
	errOutsideRoot = errors.New("path is outside the workspace")
	errNotADir     = errors.New("not a directory")
)

// resolveInRoot turns a caller-supplied path into an absolute path proven to sit
// inside root, or returns an error.
//
// rel is interpreted relative to root. An absolute rel is treated as
// root-relative rather than rejected outright, so that a UI showing "/src" means
// the workspace's src -- there is no case in which a caller legitimately means
// the container's filesystem root.
//
// Symlinks are resolved BEFORE the containment check, which is the part a
// lexical ".." filter misses: a symlink inside the workspace pointing at / is
// created by ordinary use (npm and git both make them) and would otherwise walk
// straight out.
func resolveInRoot(root, rel string) (string, error) {
	if root == "" {
		return "", errors.New("no workspace root configured")
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("workspace root %s: %w", root, err)
	}

	// Cleaning against a leading "/" CLAMPS traversal rather than refusing it:
	// "../../etc/passwd" becomes "/etc/passwd" and is then joined under the
	// root, so it names the workspace's own etc/passwd or nothing at all. That
	// is deliberate on both counts -- it cannot escape, and clicking ".." at the
	// top of the tree stays put instead of raising an error at a user who did
	// something reasonable.
	clean := filepath.Clean("/" + strings.TrimPrefix(filepath.ToSlash(rel), "/"))
	joined := filepath.Join(realRoot, filepath.FromSlash(clean))

	// EvalSymlinks needs the path to exist. When it does not, the error is the
	// caller's answer anyway -- and resolving the deepest existing ancestor
	// instead would report "not found" for a path that had escaped, which reads
	// as a missing file rather than a refused one.
	real, err := filepath.EvalSymlinks(joined)
	if err != nil {
		return "", err
	}
	if !withinRoot(realRoot, real) {
		return "", errOutsideRoot
	}
	return real, nil
}

// withinRoot reports whether path is root or sits beneath it.
//
// The separator matters: a plain string prefix test would accept
// /workspace-secrets for the root /workspace, which is a real escape and an easy
// one to write by accident.
func withinRoot(root, path string) bool {
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(os.PathSeparator))
}

// displayPath renders an absolute path back as workspace-relative, so nothing
// the console shows leaks where the workspace sits on the host.
func displayPath(root, abs string) string {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		realRoot = root
	}
	rel, err := filepath.Rel(realRoot, abs)
	if err != nil || rel == "." {
		return "/"
	}
	return "/" + filepath.ToSlash(rel)
}
