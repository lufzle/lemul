package mgmtapi

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
	// ID is the workspace NAME, which is what every client addresses a
	// workspace by -- the URL, `lem connect`, the console's routes. The schema
	// now also gives a workspace a uuid, reported separately: separating the two
	// is what lets Phase 6 rename a workspace without breaking anything holding
	// a reference to it.
	ID         string `json:"id"`
	UUID       string `json:"uuid,omitempty"`
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
	// Owner is the user id recorded at creation. It bounds what a plain member
	// SEES -- listWorkspaces splits on it -- alongside §2.5's owner-scoped
	// attach, which closed in Phase 4.
	Owner string `json:"owner,omitempty"`
	// AccessScope is who may use this workspace: owner | members | org. Not
	// omitempty -- a client that cannot see the field would have to guess, and
	// the safe guess and the true value are not the same for an org-wide one.
	AccessScope string `json:"access_scope"`
	// OwnerName is a display label for Owner when one is known -- resolved, not
	// stored on the record, so it follows a person who changes their address.
	OwnerName string `json:"owner_name,omitempty"`
	// Policy is section 2.4's lifecycle configuration. Nested rather than five
	// more flat fields, because it is read, edited and PATCHed as one thing.
	Policy workspacePolicyDoc `json:"policy"`
	// WarmHoldUntil is set while the task is up with no session left in it --
	// section 2.4's grace period before scaling to zero. Its presence IS the
	// warm-hold state; there is deliberately no such workspace status, because
	// every comparison against status would have had to learn a fourth value.
	WarmHoldUntil string `json:"warm_hold_until,omitempty"`
	// IdlePinnedSince is set while this workspace would be stopping and the only
	// thing holding it open is a background process inside a session -- a dev
	// server somebody left running, typically. It changes no behaviour: that
	// process was started on purpose and stopping the session would kill it.
	// It exists so the cost is VISIBLE on the row, since the failure mode is
	// nobody noticing rather than anybody deciding.
	IdlePinnedSince string `json:"idle_pinned_since,omitempty"`
	// LastError is why the LAST placement attempt failed, and it is the whole
	// reason placement could stop being something a caller waits for: a
	// background operation has no response left to fail on. Absent when the
	// last attempt did not fail -- it describes one attempt, never a history,
	// so a message still here after a successful placement would be read as the
	// reason for the current state and would not be it.
	LastError string `json:"last_error,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}

// workspacePolicyDoc is section 2.4's lifecycle policy on the wire.
//
// Pointers on the PATCH side and values on the GET side would be two types for
// one thing, so it is one type with pointers: absent means "leave it alone",
// which a zero cannot express when zero is itself meaningful (0 disables the
// idle timeout).
type workspacePolicyDoc struct {
	IdleTimeoutSecs  *int    `json:"idle_timeout_secs,omitempty"`
	WarmHoldSecs     *int    `json:"warm_hold_secs,omitempty"`
	AdmissionPolicy  *string `json:"admission_policy,omitempty"`
	MaxSessions      *int    `json:"max_sessions,omitempty"`
	MinFreeMemoryPct *int    `json:"min_free_memory_pct,omitempty"`
}

type workspaceListResponse struct {
	Workspaces []workspaceDoc `json:"workspaces"`
}

// handleListWorkspaces backs the console's overview.
//
// What it returns depends on the caller: an organization owner sees everything
// in it, anyone else sees what they own or were added to (see listWorkspaces).
// §2.6's remaining filters (?repo=, ?status=) are still outstanding.
func (s *Server) handleListWorkspaces(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	records, err := s.listWorkspaces(r.Context(), sc)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	names := s.displayNames(r.Context(), sc)
	docs := make([]workspaceDoc, 0, len(records))
	for _, ws := range records {
		d := s.workspaceRecord(ws)
		d.OwnerName = names[ws.OwnerUserID]
		// By NAME, not by id. The registry is keyed on the workspace name the
		// supervisor was started with (-workspace), while the database is keyed
		// on the uuid -- two different identifiers for the same thing, which is
		// exactly the trap the id/name split introduces.
		if _, terr := s.reg.PickWorkspace(sc.TenantID, ws.Name); terr == nil {
			d.Connected = true
		}
		if sessions, serr := s.listSessions(r.Context(), sc, ws.ID); serr == nil {
			d.Sessions = len(sessions)
			if d.Connected {
				// By name again: liveSessions asks the REGISTRY for the tunnel.
				live := s.liveSessions(sc, ws.Name)
				for _, rec := range sessions {
					if _, ok := live[rec.ID]; ok {
						d.Running++
					}
				}
			}
		}
		if !ws.CreatedAt.IsZero() {
			// Records written before the field existed have a zero time, and
			// formatting that would date every one of them to year one.
			d.CreatedAt = ws.CreatedAt.UTC().Format(time.RFC3339)
		}
		docs = append(docs, d)
	}
	writeJSON(w, http.StatusOK, workspaceListResponse{Workspaces: docs})
}

type statusResponse struct {
	// CellID rather than a tenant: this endpoint describes the DEPLOYMENT, and
	// a deployment now serves many organizations. Runner counts are per
	// organization and belong with the organization, not here.
	CellID string `json:"cell_id"`
	// Inference names what actually serves models, and Telemetry reports whether
	// that optional infrastructure is wired up. Telemetry is genuinely optional
	// (§5), so "off" is a valid state rather than a fault.
	Inference string `json:"inference"`
	// GatewayURL is shown beside the name so a mislabel is catchable and an
	// operator can see WHICH gateway. Configuration, not a credential: §12.4
	// keeps the gateway KEY out of reach, and an admin endpoint is not an
	// exception to that.
	GatewayURL       string `json:"gateway_url,omitempty"`
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
	// Decision #12 leaves exactly two shapes: brokered through a customer-hosted
	// gateway, which is the supported path, or Claude Code signing SigV4 against
	// Bedrock directly, which §12.5 demoted to a draft.
	//
	// A configured gateway is named LiteLLM because that is what this product
	// deploys and what the spike validated. It is an assumption rather than a
	// detection -- point -gateway-url at something else and this label is wrong,
	// which is why the URL travels with it.
	inference := "Bedrock"
	if s.opt.GatewayURL != "" {
		inference = "LiteLLM"
	}
	writeJSON(w, http.StatusOK, statusResponse{
		CellID:           s.opt.CellID,
		Inference:        inference,
		GatewayURL:       s.opt.GatewayURL,
		Telemetry:        s.opt.OTelEndpoint != "",
		BedrockPreflight: s.opt.BedrockPreflight,
		Image:            s.opt.Image,
		Pins:             s.opt.Pins,
		Now:              time.Now().UTC().Format(time.RFC3339),
	})
}

// handleGetWorkspace serves one workspace, 404ing for anything the caller has
// no standing in -- which is the same answer as one that does not exist, so a
// URL-bar typo cannot be told from somebody else's workspace.
func (s *Server) handleGetWorkspace(w http.ResponseWriter, r *http.Request) {
	a, ok := s.requireWorkspace(w, r)
	if !ok {
		return
	}
	sc, wid, ws := a.scope, a.WS.Name, a.WS
	d := s.workspaceRecord(ws)
	if _, terr := s.reg.PickWorkspace(sc.TenantID, wid); terr == nil {
		d.Connected = true
	}
	sessions, _ := s.listSessions(r.Context(), sc, ws.ID)
	d.Sessions = len(sessions)
	if d.Connected {
		live := s.liveSessions(sc, wid)
		for _, rec := range sessions {
			if _, running := live[rec.ID]; running {
				d.Running++
			}
		}
	}
	writeJSON(w, http.StatusOK, d)
}
