package e2e_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The workspace explorer, driven through the real supervisor binary placed by
// the real driver -- so what is exercised is the product's own path from an
// HTTP request, down a yamux tunnel, into a /proc read on the other side.
//
// LEMUL_PROJECT_DIR is set before the workspace is placed because the local
// driver passes its own environment to the supervisor it execs. In a container
// the root is /workspace; on a developer's machine it has to be somewhere that
// exists, and that difference is the reason the root is configuration rather
// than a constant.

type dirListing struct {
	Path    string `json:"path"`
	Entries []struct {
		Name      string `json:"name"`
		IsDir     bool   `json:"is_dir"`
		Size      int64  `json:"size"`
		Mode      string `json:"mode"`
		IsSymlink bool   `json:"is_symlink"`
	} `json:"entries"`
	Total     int  `json:"total"`
	Truncated bool `json:"truncated"`
}

func (s *stack) getJSON(t *testing.T, path string, out any) (int, string) {
	t.Helper()
	resp, err := s.hGet(s.orgURL + path)
	if err != nil {
		t.Fatalf("get %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK && out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			t.Fatalf("get %s: decode: %v (body %q)", path, err, body)
		}
	}
	return resp.StatusCode, strings.TrimSpace(string(body))
}

// explorerStack places a workspace whose root is a directory we control, and
// returns the workspace id.
func explorerStack(t *testing.T) (*stack, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEMUL_PROJECT_DIR", root)

	s := newStack(t, "cat")
	s.newSession("wsx") // placing the task is what starts a supervisor
	return s, "wsx"
}

func TestExplorerListsTheWorkspace(t *testing.T) {
	s, wid := explorerStack(t)

	var got dirListing
	code, body := s.getJSON(t, "/workspaces/"+wid+"/fs?path=/", &got)
	if code != http.StatusOK {
		t.Fatalf("fs: %d %s", code, body)
	}
	if got.Path != "/" {
		t.Errorf("path = %q, want /", got.Path)
	}

	names := map[string]bool{}
	for _, e := range got.Entries {
		names[e.Name] = e.IsDir
	}
	if isDir, ok := names["README.md"]; !ok || isDir {
		t.Errorf("README.md missing or reported as a directory: %+v", got.Entries)
	}
	if isDir, ok := names["src"]; !ok || !isDir {
		t.Errorf("src missing or not reported as a directory: %+v", got.Entries)
	}

	// And one level down, which is what makes it a browser rather than a listing.
	code, body = s.getJSON(t, "/workspaces/"+wid+"/fs?path=/src", &got)
	if code != http.StatusOK {
		t.Fatalf("fs /src: %d %s", code, body)
	}
	if len(got.Entries) != 1 || got.Entries[0].Name != "main.go" {
		t.Errorf("/src = %+v, want just main.go", got.Entries)
	}
}

// The jail, exercised through the whole stack rather than only as a unit: this
// is the check that keeps the explorer away from /proc/self/environ and the
// gateway credential it holds (§12.4).
func TestExplorerRefusesToLeaveTheWorkspace(t *testing.T) {
	s, wid := explorerStack(t)

	// A path that exists on every machine and must never be reachable.
	code, body := s.getJSON(t, "/workspaces/"+wid+"/fs?path="+url.QueryEscape("/etc"), nil)
	if code == http.StatusOK {
		t.Fatalf("listed /etc through the explorer: %s", body)
	}

	code, body = s.getJSON(t, "/workspaces/"+wid+"/fs?path="+url.QueryEscape("../../../../etc"), nil)
	if code == http.StatusOK {
		t.Fatalf("traversal reached outside the workspace: %s", body)
	}
}

// Not merely "some processes": the supervisor must find its own session and
// attribute it, because that attribution is what §2.4's idle detector will use
// to tell a running tool from an idle workspace.
func TestExplorerAttributesProcessesToSessions(t *testing.T) {
	s, wid := explorerStack(t)

	var got struct {
		Available bool `json:"available"`
		Processes []struct {
			PID       int    `json:"pid"`
			Name      string `json:"name"`
			SessionID string `json:"session_id"`
		} `json:"processes"`
	}
	code, body := s.getJSON(t, "/workspaces/"+wid+"/processes", &got)
	if code != http.StatusOK {
		t.Fatalf("processes: %d %s", code, body)
	}
	if !got.Available {
		// darwin has no procfs, and saying so is the correct answer there.
		t.Skip("no procfs on this platform; the panel reports unavailable, which is the point")
	}
	if len(got.Processes) == 0 {
		t.Fatal("available with an empty process list")
	}
	// The local driver reads this host's /proc. On a GitHub runner that is
	// systemd + dockerd, not a workspace, so nothing is session-attributed.
	if got.Processes[0].PID == 1 && got.Processes[0].Name == "systemd" {
		t.Skip("explorer listed the host; attribution is asserted against a sandbox")
	}
	var attributed int
	for _, p := range got.Processes {
		if p.SessionID != "" {
			attributed++
		}
	}
	if attributed == 0 {
		t.Errorf("no process attributed to a session; the session's own `cat` should be: %+v", got.Processes)
	}
}

// Availability flags must be honest. A zero where we could not look is the
// console stating something false rather than admitting ignorance.
func TestExplorerResourcesReportAvailability(t *testing.T) {
	s, wid := explorerStack(t)

	var got map[string]any
	code, body := s.getJSON(t, "/workspaces/"+wid+"/resources", &got)
	if code != http.StatusOK {
		t.Fatalf("resources: %d %s", code, body)
	}
	for _, group := range []string{"cpu", "memory", "disk", "network"} {
		g, ok := got[group].(map[string]any)
		if !ok {
			t.Fatalf("%s group missing from %v", group, got)
		}
		if _, ok := g["available"]; !ok {
			t.Errorf("%s has no availability flag, so a zero cannot be read correctly", group)
		}
	}
	// Disk works everywhere -- it is statfs, not cgroups -- so it is the one
	// group that must be populated on any platform the suite runs on.
	if disk := got["disk"].(map[string]any); disk["available"] != true {
		t.Errorf("disk unavailable; statfs should work on any platform: %v", disk)
	}
}

// A read must never place a task. Looking at a stopped workspace should not
// bill anyone for a Fargate cold start (the handleListSessions doctrine).
func TestExplorerDoesNotStartAWorkspace(t *testing.T) {
	s := newStack(t, "cat")

	before := s.supervisorCount()
	code, _ := s.getJSON(t, "/workspaces/never-started/fs?path=/", nil)
	if code == http.StatusOK {
		t.Fatal("listed a workspace that has no task")
	}
	if after := s.supervisorCount(); after != before {
		t.Errorf("supervisors went %d -> %d; a read placed a task", before, after)
	}
}
