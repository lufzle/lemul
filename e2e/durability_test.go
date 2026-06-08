package e2e_test

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lufzle/lemul/internal/driver"
	"github.com/lufzle/lemul/internal/driver/docker"
)

// What survives a workspace stopping and starting again.
//
// This is section 2.4's "workspace compute" axis, and the one nothing tested.
// Every lifecycle test so far stops and resumes a SESSION, whose record lives in
// Postgres and therefore survives anything at all. A member's home, their
// CLAUDE_CONFIG_DIR and their conversation transcript live on the workspace's
// disk instead, and whether THAT survives is a property of the deployment rather
// than of any code the suite was exercising.
//
// The pair below is deliberate: one runs with a volume and one without, because
// the difference between them is the property. Until 2026-08-02 the second was
// what production did while every document described the first.
//
// The stop is section 2.4's AUTO-STOP CASCADE rather than an explicit verb --
// partly because there is no explicit verb (section 2.6 lists
// POST /workspaces/{wid}/stop and it has never been built), and mostly because
// the cascade is how this is actually reached: nobody stops a workspace, they go
// to lunch.
//
// Neither test uses real Claude Code. What is under test is whether the disk
// comes back, and a file written by a shell answers that exactly as well as a
// transcript does, without a gateway or a bill.

// durabilityStack brings up the docker driver with or without a per-workspace
// volume, and with the cascade turned down to seconds.
//
// The volume name is derived from the caller so two runs cannot inherit each
// other's disk. Volumes outlive containers -- which is the entire point of them,
// and also a very good way to write a test that passes only on the second run.
func durabilityStack(t *testing.T, volumePrefix string) *stack {
	t.Helper()
	requireDocker(t)
	image := workspaceImage()

	drv := docker.New(image, nil)
	drv.VolumePrefix = volumePrefix
	if volumePrefix != "" {
		t.Cleanup(func() { removeVolumes(t, volumePrefix) })
	}
	return newStackWith(t, stackOptions{
		SessionCmd:       []string{"sh"},
		Driver:           drv,
		Image:            image,
		HeadroomInterval: time.Second,
		SweepInterval:    time.Second,
		Start:            true,
	})
}

// survivesAStop writes a marker into the session's home, lets the cascade stop
// the workspace, starts it again and reports whether the marker came back.
//
// The marker goes in $HOME rather than at a path the test picks, because a
// member's home is where their Claude Code state actually lives -- writing
// somewhere we chose would prove something about that somewhere instead.
func survivesAStop(t *testing.T, s *stack, ws, marker string) bool {
	t.Helper()

	s.newWorkspace(ws)
	s.setPolicy(ws, 3, 3)

	sid := s.startSession(ws, "")
	c := s.attach(sid, 24, 80, "")
	c.writeBytes([]byte("echo " + marker + " > $HOME/marker.txt; echo WROTE\r"))
	c.await("WROTE", 15*time.Second)
	c.detach()

	// Idle-stop the session, then let the warm hold expire and take the task
	// with it. Waited on the RECORD, which is what the next placement reads.
	s.awaitSession(ws, sid, "stopped", 45*time.Second)
	deadline := time.Now().Add(60 * time.Second)
	for {
		if w := s.workspace(ws); w.Status == "stopped" && !w.Connected {
			break
		}
		if time.Now().After(deadline) {
			w := s.workspace(ws)
			t.Fatalf("the workspace never stopped after its warm hold: status=%q "+
				"connected=%v", w.Status, w.Connected)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// A session in a stopped workspace starts it again (section 2.4), which
	// places a REPLACEMENT task: a new container, and on Fargate a new disk
	// unless something carried the old one across.
	sid2 := s.startSession(ws, "")
	c2 := s.attach(sid2, 24, 80, "")
	c2.writeBytes([]byte("cat $HOME/marker.txt 2>/dev/null || echo NO_MARKER\r"))
	got := c2.awaitEither(marker, "NO_MARKER", 20*time.Second)
	c2.detach()
	return got == marker
}

// THE regression, and the reason the workspace volume is the next piece of work.
//
// With no volume the workspace's disk is the container's writable layer, which
// is exactly what a Fargate task whose definition carries no `volume` has --
// which is every task we have ever placed. So the cascade that exists to stop
// paying for an idle workspace also destroys every member's home in it, about
// five minutes after the last session ends.
//
// And it does not fail loudly. On the next start `sessionArgs` finds no
// transcript on the fresh disk, so it passes --session-id rather than --resume,
// and Claude Code opens a BRAND NEW conversation under the same id
// (section 12.7). The user is told their session resumed and shown an empty one.
//
// This asserts the loss rather than the fix, on purpose: it is what production
// does today, and a suite that quietly described the intention instead would be
// the thing section 2.5's lesson warns about. When the volume lands, this
// becomes the assertion for "somebody removed it and nothing noticed".
func TestWithoutAVolumeTheCascadeDestroysEveryHome(t *testing.T) {
	s := durabilityStack(t, "")

	if survivesAStop(t, s, "dur-novol", "MARKER_A") {
		t.Error("the marker survived with no volume configured, so this test is " +
			"no longer measuring the storage model Fargate actually runs")
	}
}

// The same flow with a volume, which is the property sections 2.3 and 12.10 both
// rest on: a member's home is on the volume BECAUSE a replacement task starts
// with an empty container filesystem.
//
// Locally that volume is a docker named volume; on Fargate it is EBS, created
// per task and carried across a stop by a snapshot. What this pins is the shape
// -- the workspace's disk is not the container's disk -- which is the half a
// local run can honestly check.
func TestWithAVolumeHomesSurviveTheCascade(t *testing.T) {
	prefix := fmt.Sprintf("lemul-test-%d-", time.Now().UnixNano())
	s := durabilityStack(t, prefix)

	if !survivesAStop(t, s, "dur-vol", "MARKER_B") {
		t.Error("a member's home did not survive the workspace stopping, so every " +
			"\"on the volume\" claim in section 2.3 is false and a resumed session " +
			"comes back with an empty conversation")
	}
}

// removeVolumes drops every docker volume a test created.
//
// Volumes deliberately outlive their containers, so nothing else cleans these
// up -- and a test that left them behind would both leak disk and, worse, hand
// its next run a workspace that already has the marker in it.
func removeVolumes(t *testing.T, prefix string) {
	t.Helper()
	out, err := exec.Command("docker", "volume", "ls", "-q",
		"--filter", "name="+prefix).Output()
	if err != nil {
		t.Logf("listing volumes to clean up: %v", err)
		return
	}
	for _, name := range strings.Fields(string(out)) {
		if err := exec.Command("docker", "volume", "rm", "-f", name).Run(); err != nil {
			t.Logf("removing volume %s: %v", name, err)
		}
	}
}

// snapshottingDriver is the docker driver plus a fake disk, so the control
// plane's half of section 9 item 0 is testable without AWS.
//
// What is under test is not whether a snapshot works -- only the ecs driver can
// answer that, and only against a real account. It is whether the Management
// API asks for one at the right moment, records it before touching the old one,
// hands it to the next placement, and does NOT take one when a workspace is
// being deleted. All four are decisions in our code.
//
// It wraps DOCKER rather than local for the reason idle_test.go does: /proc does
// not exist on darwin, so an unprivileged local supervisor reports every session
// as tool-running and the cascade never fires -- a test built on it would pass
// by never running the thing it is about.
type snapshottingDriver struct {
	*docker.Driver

	mu       sync.Mutex
	n        int
	taken    []string // snapshots created, in order
	dropped  []string // snapshots collected, in order
	released []string // task refs whose disk was let go, snapshotted or not
	restored []string // Spec.SnapshotID seen at each placement
}

func (d *snapshottingDriver) Start(ctx context.Context, s driver.Spec) (string, error) {
	d.mu.Lock()
	d.restored = append(d.restored, s.SnapshotID)
	d.mu.Unlock()
	return d.Driver.Start(ctx, s)
}

// Preserve models the ecs driver: it releases the disk either way, and only
// snapshots when asked. `released` is what proves a DELETE still lets go of the
// volume -- the case that used to skip this call entirely.
func (d *snapshottingDriver) Preserve(_ context.Context, ref string, keep bool) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ref == "" {
		return "", nil
	}
	d.released = append(d.released, ref)
	if !keep {
		return "", nil
	}
	d.n++
	id := fmt.Sprintf("snap-%d", d.n)
	d.taken = append(d.taken, id)
	return id, nil
}

func (d *snapshottingDriver) DropSnapshot(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dropped = append(d.dropped, id)
	return nil
}

func (d *snapshottingDriver) state() (taken, dropped, restored []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.taken...),
		append([]string(nil), d.dropped...),
		append([]string(nil), d.restored...)
}

// The whole round trip: the cascade stops a workspace and preserves its disk,
// and the next placement restores from exactly what was preserved.
//
// The second half is the one that matters. A snapshot taken and then not handed
// back is indistinguishable from no snapshot at all -- the workspace still comes
// back blank, and now it also bills for the storage.
func TestTheCascadePreservesTheDiskAndTheNextPlacementRestoresIt(t *testing.T) {
	requireDocker(t)
	image := workspaceImage()
	drv := &snapshottingDriver{Driver: docker.New(image, nil)}

	s := newStackWith(t, stackOptions{
		SessionCmd:       []string{"sh"},
		Driver:           drv,
		Image:            image,
		HeadroomInterval: time.Second,
		SweepInterval:    time.Second,
		Start:            true,
	})
	const ws = "snap-round-trip"
	s.newWorkspace(ws)
	s.setPolicy(ws, 3, 3)

	sid := s.startSession(ws, "")
	s.awaitSession(ws, sid, "stopped", 45*time.Second)
	awaitStopped(t, s, ws)

	taken, _, restored := drv.state()
	if len(taken) != 1 {
		t.Fatalf("the cascade took %d snapshots, want 1 -- an idle stop that "+
			"preserves nothing destroys every member's home", len(taken))
	}
	if restored[0] != "" {
		t.Errorf("the FIRST placement restored %q; a new workspace has no disk to "+
			"restore and passing one would refuse the placement", restored[0])
	}

	// Start it again. This is where a preserved disk either comes back or does
	// not, and it is the assertion the whole feature exists for.
	s.startSession(ws, "")
	_, _, restored = drv.state()
	if len(restored) < 2 {
		t.Fatalf("the workspace was not placed again: %v", restored)
	}
	if got := restored[len(restored)-1]; got != taken[0] {
		t.Errorf("the replacement task restored %q, want %q -- the workspace comes "+
			"back blank and the snapshot bills forever", got, taken[0])
	}
}

// THE ordering rule. A workspace's snapshot is its filesystem between tasks, so
// the row must name the new one before anything deletes the old one.
//
// Asserted as "the displaced snapshot is dropped, and the current one never is",
// because that is the observable form of the rule: a run that dropped first
// would, on a crash, leave a row pointing at nothing -- and dropping the CURRENT
// one is that same bug with no crash required.
func TestADisplacedSnapshotIsCollectedAndTheCurrentOneIsNot(t *testing.T) {
	requireDocker(t)
	image := workspaceImage()
	drv := &snapshottingDriver{Driver: docker.New(image, nil)}

	s := newStackWith(t, stackOptions{
		SessionCmd:       []string{"sh"},
		Driver:           drv,
		Image:            image,
		HeadroomInterval: time.Second,
		SweepInterval:    time.Second,
		Start:            true,
	})
	const ws = "snap-gc"
	s.newWorkspace(ws)
	s.setPolicy(ws, 3, 3)

	// Two full cycles, so the second stop has something to displace.
	for range 2 {
		sid := s.startSession(ws, "")
		s.awaitSession(ws, sid, "stopped", 45*time.Second)
		awaitStopped(t, s, ws)
	}

	taken, dropped, _ := drv.state()
	if len(taken) != 2 {
		t.Fatalf("took %v, want two snapshots", taken)
	}
	if len(dropped) != 1 || dropped[0] != taken[0] {
		t.Fatalf("dropped %v, want just the displaced %q", dropped, taken[0])
	}
	for _, d := range dropped {
		if d == taken[1] {
			t.Error("the CURRENT snapshot was collected, which is the workspace's " +
				"filesystem -- it comes back blank")
		}
	}
}

// Deleting a workspace must not preserve its disk. A snapshot of something the
// customer asked us to destroy is a bill for storing it, and it would outlive
// every record that could explain what it was.
func TestDeletingAWorkspaceTakesNoSnapshotAndCollectsTheOldOne(t *testing.T) {
	requireDocker(t)
	image := workspaceImage()
	drv := &snapshottingDriver{Driver: docker.New(image, nil)}

	s := newStackWith(t, stackOptions{
		SessionCmd:       []string{"sh"},
		Driver:           drv,
		Image:            image,
		HeadroomInterval: time.Second,
		SweepInterval:    time.Second,
		Start:            true,
	})
	const ws = "snap-delete"
	s.newWorkspace(ws)
	s.setPolicy(ws, 3, 3)

	// One full cycle, so the workspace owns a snapshot to be collected.
	sid := s.startSession(ws, "")
	s.awaitSession(ws, sid, "stopped", 45*time.Second)
	awaitStopped(t, s, ws)
	before, _, _ := drv.state()
	if len(before) != 1 {
		t.Fatalf("setup took %v, want one snapshot", before)
	}

	// Then start it again and delete it while the task is UP.
	//
	// Deleting a stopped workspace would not test this at all: stopTask returns
	// immediately when there is no task reference, so nothing would take a
	// snapshot whatever the delete asked for. Mutation-checked -- with delete
	// wired to preserve, the stopped-workspace version of this test passes.
	s.startSession(ws, "")
	if w := s.workspace(ws); !w.Connected {
		t.Fatal("the workspace is not up, so the delete path under test is skipped")
	}

	if code, body := s.del("/workspaces/" + ws + "?force=1"); code != http.StatusNoContent {
		t.Fatalf("deleting the workspace: %d: %s", code, body)
	}

	taken, dropped, _ := drv.state()
	if len(taken) != len(before) {
		t.Errorf("deleting took another snapshot (%v), which bills for storing a "+
			"disk the customer asked us to destroy", taken)
	}
	if len(dropped) != 1 || dropped[0] != before[0] {
		t.Errorf("dropped %v, want the deleted workspace's own snapshot %q",
			dropped, before[0])
	}
}

// awaitStopped waits for the cascade to take the task down.
func awaitStopped(t *testing.T, s *stack, ws string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		if w := s.workspace(ws); w.Status == "stopped" && !w.Connected {
			return
		}
		if time.Now().After(deadline) {
			w := s.workspace(ws)
			t.Fatalf("the workspace never stopped: status=%q connected=%v",
				w.Status, w.Connected)
		}
		time.Sleep(300 * time.Millisecond)
	}
}
