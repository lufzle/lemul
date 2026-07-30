package e2e_test

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

type workspaceDocT struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	Generation uint64 `json:"generation"`
	TaskRef    string `json:"task_ref"`
	Connected  bool   `json:"connected"`
	Sessions   int    `json:"sessions"`
	Running    int    `json:"running"`
}

func (s *stack) workspaces() []workspaceDocT {
	s.t.Helper()
	resp, err := http.Get(s.baseURL + "/v1/workspaces")
	if err != nil {
		s.t.Fatalf("list workspaces: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		s.t.Fatalf("list workspaces: %s: %s", resp.Status, body)
	}
	var out struct {
		Workspaces []workspaceDocT `json:"workspaces"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		s.t.Fatalf("list workspaces: %v", err)
	}
	return out.Workspaces
}

func (s *stack) workspace(wid string) workspaceDocT {
	s.t.Helper()
	for _, w := range s.workspaces() {
		if w.ID == wid {
			return w
		}
	}
	s.t.Fatalf("workspace %s not listed", wid)
	return workspaceDocT{}
}

// TestGenerationSurvivesPlacement guards a bug that voided the §2.8 idempotency
// protection while looking like a cosmetic field.
//
// ensureWorkspace reads the workspace, calls NextGeneration, and then writes the
// record back to set status and task ref. The copy it writes was read BEFORE the
// increment, so it restored the old value and the counter never left 0.
//
// The consequence is not a duplicate task, which is what §2.8 warns about -- it
// is the opposite and worse. The generation is what the ECS RunTask client-token
// is derived from, so two genuinely different placements would share one token,
// and ECS answers a reused token with the ORIGINAL task: the stopped one being
// replaced. A workspace whose task died could never be placed again. It hid on
// the docker driver because deleting a container frees its name.
func TestGenerationSurvivesPlacement(t *testing.T) {
	s := newStack(t, "cat")
	s.newSession("w1")

	first := s.workspace("w1")
	if first.Generation == 0 {
		t.Fatalf("generation is %d after a placement; the increment was overwritten", first.Generation)
	}
	if !first.Connected {
		t.Fatal("workspace reports no task after a successful placement")
	}
}

// A second placement must take a NEW generation, because that is the only thing
// distinguishing its idempotency key from the previous task's.
func TestSecondPlacementTakesANewGeneration(t *testing.T) {
	s := newStack(t, "cat")
	s.newSession("w1")
	first := s.workspace("w1")

	// Drop the task the way a crashed one goes: the tunnel disappears and the
	// control plane has to place again on the next request.
	s.drv.StopAll(t.Context())
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && s.workspace("w1").Connected {
		time.Sleep(50 * time.Millisecond)
	}
	if s.workspace("w1").Connected {
		t.Fatal("task still connected after StopAll")
	}

	s.newSession("w1") // forces a second placement
	second := s.workspace("w1")

	if second.Generation <= first.Generation {
		t.Fatalf("second placement reused generation %d (first was %d); the RunTask "+
			"client-token would repeat and ECS would answer with the stopped task",
			second.Generation, first.Generation)
	}
}

// The console's overview distinguishes what we recorded from what is actually
// connected. Those are different facts and conflating them hides the case worth
// seeing -- a workspace recorded active whose task has died.
func TestWorkspaceListReportsLiveTaskState(t *testing.T) {
	s := newStack(t, "cat")
	sid := s.newSession("w1")
	c := s.attach(sid, 24, 80, "")
	c.writeBytes([]byte("hi\n"))
	c.await("hi", 3*time.Second)

	w := s.workspace("w1")
	if !w.Connected || w.Sessions != 1 || w.Running != 1 {
		t.Fatalf("with one live session: connected=%v sessions=%d running=%d",
			w.Connected, w.Sessions, w.Running)
	}

	if code, body := s.post("/v1/sessions/" + sid + "/stop"); code != http.StatusOK {
		t.Fatalf("stop: %d: %s", code, body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && s.workspace("w1").Running != 0 {
		time.Sleep(50 * time.Millisecond)
	}

	w = s.workspace("w1")
	if w.Running != 0 {
		t.Errorf("running=%d after stop, want 0", w.Running)
	}
	// The record outlives the process, so the count must not drop with it.
	if w.Sessions != 1 {
		t.Errorf("sessions=%d after stop, want the record to survive", w.Sessions)
	}
}
