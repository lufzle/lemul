package controlplane

import (
	"net/http"
	"time"
)

// Read models for the operator console (§8). Everything here is derived from
// state the control plane already holds -- these endpoints add a view, never a
// second source of truth.
//
// The load-bearing distinction they surface is the one §2.4 keeps insisting on:
// a workspace's RECORD and its running TASK are different things. The store says
// what we believe, the tunnel registry says what is actually connected, and the
// console has to show both or an operator cannot tell "never started" from
// "started and died".

type workspaceDoc struct {
	ID         string `json:"id"`
	TenantID   string `json:"tenant_id"`
	Status     string `json:"status"`
	Generation uint64 `json:"generation"`
	TaskRef    string `json:"task_ref,omitempty"`
	// Connected is the honest answer, and it can disagree with Status. Status is
	// what we last wrote down; Connected is whether a task is holding a tunnel
	// right now. A workspace recorded active whose task died reads
	// active+disconnected, which is exactly the state worth seeing.
	Connected bool `json:"connected"`
	// Sessions counts durable records, not processes. Running counts processes,
	// and is only meaningful when connected -- the supervisor is the only thing
	// that knows a PTY exists (§8, handleListSessions).
	Sessions int `json:"sessions"`
	Running  int `json:"running"`
}

type workspaceListResponse struct {
	Workspaces []workspaceDoc `json:"workspaces"`
}

// handleListWorkspaces backs the console's overview. §2.6 specifies filters
// (?owner=, ?repo=, ?status=); they arrive with the user model in Phase 3, since
// filtering by owner is meaningless while there is exactly one tenant and no
// users at all.
func (s *Server) handleListWorkspaces(w http.ResponseWriter, r *http.Request) {
	records, err := s.st.ListWorkspaces()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	docs := make([]workspaceDoc, 0, len(records))
	for _, ws := range records {
		d := workspaceDoc{
			ID:         ws.ID,
			TenantID:   ws.TenantID,
			Status:     ws.Status,
			Generation: ws.Generation,
			TaskRef:    ws.TaskRef,
		}
		if _, terr := s.reg.PickWorkspace(ws.ID); terr == nil {
			d.Connected = true
		}
		if sessions, serr := s.st.ListSessions(ws.ID); serr == nil {
			d.Sessions = len(sessions)
			if d.Connected {
				live := s.liveSessions(ws.ID)
				for _, rec := range sessions {
					if _, ok := live[rec.ID]; ok {
						d.Running++
					}
				}
			}
		}
		docs = append(docs, d)
	}
	writeJSON(w, http.StatusOK, workspaceListResponse{Workspaces: docs})
}

type statusResponse struct {
	TenantID string `json:"tenant_id"`
	Runners  int    `json:"runners"`
	// Inference and Telemetry report which optional infrastructure is wired up.
	// Both are genuinely optional (§5, decision #12), so "off" is a valid state
	// rather than a fault -- the console must not show it as one.
	Inference        string `json:"inference"`
	Telemetry        bool   `json:"telemetry"`
	BedrockPreflight bool   `json:"bedrock_preflight"`
	Image            string `json:"image,omitempty"`
	Pins             string `json:"pins,omitempty"`
	Now              string `json:"now"`
}

// handleStatus is the console header: is a runner connected at all, and what is
// this control plane configured to do? A console that cannot answer "is anything
// listening" sends the operator to the logs for the first question they ask.
//
// Deliberately free of credentials. The gateway URL is configuration, but the
// gateway KEY is exactly what §12.4 keeps out of reach, and an admin endpoint is
// not an exception to that.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	inference := "none"
	if s.opt.GatewayURL != "" {
		inference = "gateway"
	} else if s.opt.BedrockPreflight {
		inference = "bedrock"
	}
	writeJSON(w, http.StatusOK, statusResponse{
		TenantID:         s.opt.TenantID,
		Runners:          s.reg.RunnerCount(s.opt.TenantID),
		Inference:        inference,
		Telemetry:        s.opt.OTelEndpoint != "",
		BedrockPreflight: s.opt.BedrockPreflight,
		Image:            s.opt.Image,
		Pins:             s.opt.Pins,
		Now:              time.Now().UTC().Format(time.RFC3339),
	})
}

// handleGetWorkspace serves one workspace, 404ing rather than creating it.
//
// ensureWorkspace creates on demand because a session request means someone
// wants one; a console GET means someone is looking, and answering "here is the
// workspace you just invented" would let a typo in the URL bar manufacture
// records.
func (s *Server) handleGetWorkspace(w http.ResponseWriter, r *http.Request) {
	wid := r.PathValue("wid")
	ws, err := s.st.GetWorkspace(wid)
	if err != nil {
		http.Error(w, "no such workspace", http.StatusNotFound)
		return
	}
	d := workspaceDoc{
		ID:         ws.ID,
		TenantID:   ws.TenantID,
		Status:     ws.Status,
		Generation: ws.Generation,
		TaskRef:    ws.TaskRef,
	}
	if _, terr := s.reg.PickWorkspace(wid); terr == nil {
		d.Connected = true
	}
	sessions, _ := s.st.ListSessions(wid)
	d.Sessions = len(sessions)
	if d.Connected {
		live := s.liveSessions(wid)
		for _, rec := range sessions {
			if _, ok := live[rec.ID]; ok {
				d.Running++
			}
		}
	}
	writeJSON(w, http.StatusOK, d)
}
