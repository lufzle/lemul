package supervisor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The jail is the only thing standing between the explorer and the rest of the
// container, because the supervisor runs as root. These are the escapes worth
// naming rather than assuming.
func TestResolveInRootRefusesEscapes(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "README.md"), "hello")
	if err := os.Mkdir(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}

	// A file that must never be reachable, standing in for
	// /proc/self/environ and the gateway credential it holds.
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	mustWrite(t, secret, "GATEWAY KEY")

	// A symlink INSIDE the workspace pointing out of it. npm and git both
	// create symlinks in ordinary use, so this is not an exotic case -- and it
	// is precisely what a lexical ".." filter fails to catch.
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	realRoot, _ := filepath.EvalSymlinks(root)

	// A symlink cannot be clamped -- it is a real path that genuinely lands
	// outside -- so the only correct answer is a refusal.
	t.Run("symlink out of the root", func(t *testing.T) {
		for _, p := range []string{"escape", "escape/secret.txt"} {
			got, err := resolveInRoot(root, p)
			if err == nil {
				t.Fatalf("resolved %q to %q; want a refusal", p, got)
			}
			if !errors.Is(err, errOutsideRoot) {
				t.Errorf("resolveInRoot(%q) = %v, want errOutsideRoot", p, err)
			}
		}
	})

	// Traversal is CLAMPED rather than refused, which is the standard
	// Clean("/"+p) idiom and the friendlier behaviour -- clicking ".." at the
	// top of the tree should stay there rather than raise an error. The
	// property under test is not "it fails" but "it never lands outside", which
	// is the only one that protects anything.
	t.Run("traversal is clamped to the root", func(t *testing.T) {
		for _, p := range []string{"../", "../../../..", "src/../../..", "/../.."} {
			got, err := resolveInRoot(root, p)
			if err != nil {
				continue // refusing is also acceptable; escaping is not
			}
			if !withinRoot(realRoot, got) {
				t.Errorf("resolveInRoot(%q) = %q, which is outside %q", p, got, realRoot)
			}
		}
	})

	// The one that matters most: a traversal aimed at a real file outside must
	// not reach it. It resolves under the workspace instead, where there is no
	// such file.
	t.Run("traversal cannot reach a file outside", func(t *testing.T) {
		if got, err := resolveInRoot(root, "../"+filepath.Base(outside)+"/secret.txt"); err == nil {
			t.Fatalf("reached %q", got)
		}
		if got, err := resolveInRoot(root, secret); err == nil {
			t.Fatalf("an absolute outside path resolved to %q", got)
		}
	})
}

// An absolute path means "from the workspace root", not "from /". There is no
// case where a caller legitimately means the container's filesystem root, and
// treating it as workspace-relative is what lets a UI show "/src" honestly.
func TestResolveInRootTreatsAbsoluteAsWorkspaceRelative(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := resolveInRoot(root, "/src")
	if err != nil {
		t.Fatalf("resolveInRoot: %v", err)
	}
	realRoot, _ := filepath.EvalSymlinks(root)
	if want := filepath.Join(realRoot, "src"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolveInRootAllowsTheRootItself(t *testing.T) {
	root := t.TempDir()
	realRoot, _ := filepath.EvalSymlinks(root)

	for _, p := range []string{"", "/", ".", "./"} {
		got, err := resolveInRoot(root, p)
		if err != nil {
			t.Fatalf("resolveInRoot(%q): %v", p, err)
		}
		if got != realRoot {
			t.Errorf("resolveInRoot(%q) = %q, want %q", p, got, realRoot)
		}
	}
}

// A sibling directory whose name merely starts with the root's is the escape a
// plain string-prefix test lets through, and it is an easy one to write.
func TestWithinRootRequiresASeparator(t *testing.T) {
	if withinRoot("/workspace", "/workspace-secrets/creds") {
		t.Error("/workspace-secrets was accepted for root /workspace")
	}
	if !withinRoot("/workspace", "/workspace") {
		t.Error("the root itself was rejected")
	}
	if !withinRoot("/workspace", "/workspace/src/main.go") {
		t.Error("a genuine child was rejected")
	}
}

func TestResolveInRootWithoutARootConfigured(t *testing.T) {
	if _, err := resolveInRoot("", "anything"); err == nil {
		t.Fatal("no error with an empty root")
	}
}

// displayPath must never leak where the workspace sits on the host, since the
// console shows it to a user who has no business knowing the mount layout.
func TestDisplayPathIsWorkspaceRelative(t *testing.T) {
	root := t.TempDir()
	realRoot, _ := filepath.EvalSymlinks(root)

	if got := displayPath(root, realRoot); got != "/" {
		t.Errorf("root rendered as %q, want /", got)
	}
	got := displayPath(root, filepath.Join(realRoot, "src", "main.go"))
	if got != "/src/main.go" {
		t.Errorf("got %q, want /src/main.go", got)
	}
	if strings.Contains(got, realRoot) {
		t.Errorf("host path leaked into %q", got)
	}
}

func TestResolveInRootReportsMissingPaths(t *testing.T) {
	root := t.TempDir()
	_, err := resolveInRoot(root, "nope")
	if err == nil {
		t.Fatal("no error for a missing path")
	}
	if errors.Is(err, errOutsideRoot) {
		t.Error("a missing path was reported as an escape, which misleads")
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
