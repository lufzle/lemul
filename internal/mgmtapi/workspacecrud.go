package mgmtapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/lufzle/lemul/internal/directory"
	"github.com/lufzle/lemul/internal/store"
	"github.com/lufzle/lemul/internal/store/celldb"
	"github.com/lufzle/lemul/internal/tunnel"
	"github.com/lufzle/lemul/internal/wsname"
)

// Workspace create, rename and delete.
//
// None of these existed until Phase 4. A workspace appeared as a side effect of
// asking for a session in one, which meant a typo in a URL manufactured a
// record and there was no verb that did it deliberately -- so there was also
// nowhere to hang the permission check section 2.5 asks for.

// createWorkspaceRequest carries a name that is REQUIRED as a property and may
// be explicitly null.
//
// Absent and null are different answers -- "you forgot" versus "you pick" --
// and a plain *string cannot tell them apart, because a missing key and a JSON
// null both decode to nil. optionalString keeps the distinction in the type
// rather than in a second pass over the raw body.
type createWorkspaceRequest struct {
	Name optionalString `json:"name"`
	// AccessScope is who may use it. Absent defaults to 'owner' -- the closed
	// answer, so a client that does not know about this field cannot widen a
	// workspace by omission.
	AccessScope string `json:"access_scope,omitempty"`
}

// optionalString distinguishes an absent JSON key from an explicit null.
type optionalString struct {
	Value string
	// Set is true when the key was present at all, whatever it held.
	Set bool
	// Null is true when the key was present and held null.
	Null bool
}

func (o *optionalString) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Null = true
		return nil
	}
	return json.Unmarshal(b, &o.Value)
}

// handleCreateWorkspace creates one, generating a name when asked to.
//
// WHO may create one is an organization setting, default owners-only. A
// workspace is a Fargate task somebody pays for from the moment it is placed,
// so "every member may start one" is a cost decision that belongs to the
// organization rather than a default we pick for them.
//
// There is ONE creation path. The personal (`pw-`) workspace used to be a
// second, reached by a flag, named from the caller's email and conjured lazily
// by a bare `lem` -- which meant a command with no arguments could start a
// Fargate task, and a gate on this handler would have bounded nothing. "A
// workspace with access_scope = owner" says everything that concept said,
// without a reserved name prefix or a creation path nobody asked for.
//
// Creating one also writes the owner membership row, so "which workspaces may I
// see" stays one query rather than a union of ownership and membership.
//
// It answers 202, not 201, and PLACES THE TASK (section 2.6, increment 4). The
// record exists by the time this returns and the machine does not, which is
// what the code says: the workspace is accepted and on its way up. Placing here
// rather than at the first session is what stops one member paying a cold start
// for everybody else's machine -- and it is only safe because something now
// stops an idle workspace, or a workspace created on a Friday would bill until
// Monday (section 2.4, placement.go).
func (s *Server) handleCreateWorkspace(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	if !s.mayCreateWorkspace(w, r, sc) {
		return
	}

	var req createWorkspaceRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	access := req.AccessScope
	if access == "" {
		access = accessOwner
	}
	if !validAccessScopes[access] {
		http.Error(w, `"access_scope" must be one of: owner, members, org`,
			http.StatusBadRequest)
		return
	}

	// Required as a PROPERTY. Sending nothing is a mistake worth naming, since
	// the alternative -- treating absent as "generate one" -- means a client
	// with a typo'd field name silently gets a random workspace instead of the
	// one it asked for.
	if !req.Name.Set {
		http.Error(w, `"name" is required; send null to have one generated`,
			http.StatusBadRequest)
		return
	}

	if req.Name.Null {
		ws, err := s.createGeneratedWorkspace(r.Context(), sc, access)
		if err != nil {
			s.log.Error("creating a workspace", "org", sc.Org.Slug, "error", err)
			http.Error(w, "could not create the workspace", http.StatusInternalServerError)
			return
		}
		s.log.Info("workspace created", "org", sc.Org.Slug, "workspace", ws.Name,
			"generated", true, "by", sc.UserID)
		ws = s.startPlacement(r.Context(), sc, ws, "workspace created")
		writeJSON(w, http.StatusAccepted, s.workspaceRecord(ws))
		return
	}

	name := strings.TrimSpace(req.Name.Value)
	if err := validWorkspaceName(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ws, err := s.createWorkspace(r.Context(), sc, name, access)
	if directory.IsUniqueViolation(err) {
		http.Error(w, "a workspace called "+name+" already exists here", http.StatusConflict)
		return
	} else if err != nil {
		s.log.Error("creating a workspace", "org", sc.Org.Slug, "workspace", name, "error", err)
		http.Error(w, "could not create the workspace", http.StatusInternalServerError)
		return
	}
	s.log.Info("workspace created", "org", sc.Org.Slug, "workspace", name, "by", sc.UserID)
	ws = s.startPlacement(r.Context(), sc, ws, "workspace created")
	writeJSON(w, http.StatusAccepted, s.workspaceRecord(ws))
}

// nameAttempts bounds the retry on a generated name.
//
// Bounded rather than unbounded: with 11,760 combinations a collision is
// already unlikely, and a loop that never gives up turns a full keyspace into a
// hung request instead of an error somebody can read.
const nameAttempts = 8

// createGeneratedWorkspace retries a generated name against the unique
// constraint, which is the same shape CreateOrg already uses for slugs.
func (s *Server) createGeneratedWorkspace(ctx context.Context, sc scope, access string) (Workspace, error) {
	var err error
	for range nameAttempts {
		var ws Workspace
		// A fresh transaction per attempt, not a retry inside one: Postgres
		// aborts the whole transaction on any statement error, so a second
		// INSERT after a unique violation fails with "current transaction is
		// aborted" rather than with anything about the name.
		ws, err = s.createWorkspace(ctx, sc, wsname.Generate(), access)
		if err == nil {
			return ws, nil
		}
		if !directory.IsUniqueViolation(err) {
			return Workspace{}, err
		}
	}
	return Workspace{}, fmt.Errorf("could not find an unused workspace name in %d attempts: %w",
		nameAttempts, err)
}

// validWorkspaceName refuses what the database would refuse anyway, with a
// sentence rather than a constraint violation.
func validWorkspaceName(name string) error {
	switch {
	case name == "":
		return errors.New("a workspace name cannot be empty")
	case len(name) > 64:
		return errors.New("a workspace name is at most 64 characters")
	}
	// The name travels in a URL path segment, in the supervisor's argv and in
	// the tunnel registry key. Restricting it to what all three carry without
	// escaping is far cheaper than teaching three components to quote.
	//
	// It travels in a FOURTH place this list used to miss: `lem`'s own argv. A
	// name beginning with - is indistinguishable from a flag there, so the CLI
	// that minted it cannot address it afterwards -- measured 2026-08-03, when
	// `lem workspace create --help` created a workspace called --help (the verb
	// stops flag parsing, so the rune loop below saw only - and letters and was
	// happy) and no `lem workspace rm` could then name it. The record is
	// reachable by uuid, but a name the product's own client cannot type is a
	// name the product should not mint.
	if strings.HasPrefix(name, "-") {
		return errors.New("a workspace name cannot begin with -; it would read as a flag on the command line")
	}
	for _, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			return errors.New("a workspace name may hold lowercase letters, digits, - and _ only")
		}
	}
	return nil
}

type patchWorkspaceRequest struct {
	Name        string `json:"name,omitempty"`
	AccessScope string `json:"access_scope,omitempty"`
	// Policy is section 2.4's lifecycle configuration. A pointer, so "no policy
	// key at all" and "a policy object with every field absent" are the same
	// no-op rather than a reset to zero.
	Policy *workspacePolicyDoc `json:"policy,omitempty"`
}

// applyTo folds a partial policy onto a workspace record, leaving absent fields
// alone, and reports what is wrong rather than clamping. Clamping would accept a
// value and then not use it, which is the silent-failure shape this repo has
// been bitten by twice.
func (p *workspacePolicyDoc) applyTo(ws Workspace) (Workspace, string) {
	if p == nil {
		return ws, ""
	}
	if p.IdleTimeoutSecs != nil {
		if *p.IdleTimeoutSecs < 0 {
			return ws, `"idle_timeout_secs" cannot be negative; 0 disables the idle stop`
		}
		ws.IdleTimeoutSecs = int32(*p.IdleTimeoutSecs)
	}
	if p.WarmHoldSecs != nil {
		if *p.WarmHoldSecs < 0 {
			return ws, `"warm_hold_secs" cannot be negative`
		}
		ws.WarmHoldSecs = int32(*p.WarmHoldSecs)
	}
	if p.AdmissionPolicy != nil {
		if !validAdmissionPolicies[*p.AdmissionPolicy] {
			return ws, `"admission_policy" must be one of: unlimited, max_sessions, min_free_memory_pct`
		}
		ws.AdmissionPolicy = *p.AdmissionPolicy
	}
	if p.MaxSessions != nil {
		if *p.MaxSessions < 1 {
			return ws, `"max_sessions" must be at least 1`
		}
		ws.MaxSessions = int32(*p.MaxSessions)
	}
	if p.MinFreeMemoryPct != nil {
		if *p.MinFreeMemoryPct < 0 || *p.MinFreeMemoryPct > 100 {
			return ws, `"min_free_memory_pct" must be between 0 and 100`
		}
		ws.MinFreeMemoryPct = int32(*p.MinFreeMemoryPct)
	}
	return ws, ""
}

// validAdmissionPolicies mirrors the CHECK in schema.sql; the database is the
// authority and this is the early, legible refusal.
var validAdmissionPolicies = map[string]bool{
	admitUnlimited:   true,
	admitMaxSessions: true,
	admitMinFreePct:  true,
}

// handleRenameWorkspace is the reason id and name are separate columns.
//
// Refused while a task is live, and that is the id-vs-name trap rather than
// caution: the tunnel registry, the supervisor's -workspace argv, the preflight
// store and the derived workspace credential are ALL keyed on the name. Renaming
// under a running task orphans its tunnel set and invalidates the credential it
// is currently holding, so its next dial is answered 401 and it exits by design,
// taking its sessions with it.
func (s *Server) handleRenameWorkspace(w http.ResponseWriter, r *http.Request) {
	a, ok := s.requireWorkspaceOwner(w, r)
	if !ok {
		return
	}

	var req patchWorkspaceRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" && req.AccessScope == "" && req.Policy == nil {
		http.Error(w, `send "name", "access_scope", "policy", or any combination`,
			http.StatusBadRequest)
		return
	}
	// Everything is validated before anything is applied, so a bad pair cannot
	// leave a workspace renamed but not rescoped.
	if name != "" {
		if err := validWorkspaceName(name); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if req.AccessScope != "" && !validAccessScopes[req.AccessScope] {
		http.Error(w, `"access_scope" must be one of: owner, members, org`,
			http.StatusBadRequest)
		return
	}
	wantPolicy, bad := req.Policy.applyTo(a.WS)
	if bad != "" {
		http.Error(w, bad, http.StatusBadRequest)
		return
	}
	if req.Policy != nil {
		ws, err := s.setWorkspacePolicy(r.Context(), a.Scope, wantPolicy)
		if err != nil {
			s.log.Error("changing a workspace policy", "workspace", a.WS.Name, "error", err)
			http.Error(w, "could not change the policy", http.StatusInternalServerError)
			return
		}
		s.log.Info("workspace policy changed", "org", a.Org.Slug, "workspace", a.WS.Name,
			"idle_timeout_s", ws.IdleTimeoutSecs, "warm_hold_s", ws.WarmHoldSecs,
			"admission", ws.AdmissionPolicy, "max_sessions", ws.MaxSessions,
			"min_free_pct", ws.MinFreeMemoryPct, "by", a.UserID)
		a.WS = ws
	}
	if req.AccessScope != "" && req.AccessScope != a.WS.AccessScope {
		ws, err := s.setAccessScope(r.Context(), a.Scope, a.WS.ID, req.AccessScope)
		if err != nil {
			s.log.Error("changing a workspace access scope",
				"workspace", a.WS.Name, "error", err)
			http.Error(w, "could not change the access scope", http.StatusInternalServerError)
			return
		}
		s.log.Info("workspace access scope changed", "org", a.Org.Slug,
			"workspace", a.WS.Name, "from", a.WS.AccessScope, "to", req.AccessScope,
			"by", a.UserID)
		a.WS = ws
	}

	if name == "" || name == a.WS.Name {
		writeJSON(w, http.StatusOK, s.workspaceRecord(a.WS))
		return
	}
	if _, err := s.reg.PickWorkspace(a.TenantID, a.WS.Name); err == nil {
		http.Error(w, "this workspace has a running task, and everything outside the "+
			"database addresses it by name; stop its sessions and let the task go before renaming",
			http.StatusConflict)
		return
	}
	if a.WS.Status != WorkspaceStopped {
		http.Error(w, "this workspace is "+a.WS.Status+"; renaming is only safe while it is stopped",
			http.StatusConflict)
		return
	}

	ws, err := s.renameWorkspace(r.Context(), a.Scope, a.WS.ID, name)
	if directory.IsUniqueViolation(err) {
		http.Error(w, "a workspace called "+name+" already exists here", http.StatusConflict)
		return
	} else if err != nil {
		s.log.Error("renaming a workspace", "workspace", a.WS.Name, "error", err)
		http.Error(w, "could not rename the workspace", http.StatusInternalServerError)
		return
	}
	s.log.Info("workspace renamed",
		"org", a.Org.Slug, "from", a.WS.Name, "to", name, "by", a.UserID)
	writeJSON(w, http.StatusOK, s.workspaceRecord(ws))
}

// handleDeleteWorkspace drops a workspace, its membership and its sessions.
//
// Refuses while sessions exist unless ?force=1, matching the precedent
// handleDeleteSession sets for the one unrecoverable verb. Stops the task
// FIRST, because a deleted record with a running Fargate task behind it is
// unbounded customer spend nothing is left to account for -- the runner has
// implemented stop since Phase 1 and this is its first caller.
func (s *Server) handleDeleteWorkspace(w http.ResponseWriter, r *http.Request) {
	a, ok := s.requireWorkspaceOwner(w, r)
	if !ok {
		return
	}
	force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"

	sessions, err := s.listSessions(r.Context(), a.scope, a.WS.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(sessions) > 0 && !force {
		http.Error(w, fmt.Sprintf(
			"this workspace holds %d session(s), and deleting it drops their conversations; "+
				"delete them first, or pass ?force=1", len(sessions)),
			http.StatusConflict)
		return
	}

	s.stopWorkspaceTask(r.Context(), a)

	if err := s.deleteWorkspace(r.Context(), a.Scope, a.WS.ID); err != nil {
		s.log.Error("deleting a workspace", "workspace", a.WS.Name, "error", err)
		http.Error(w, "could not delete the workspace", http.StatusInternalServerError)
		return
	}
	s.log.Info("workspace deleted", "org", a.Org.Slug, "workspace", a.WS.Name,
		"sessions", len(sessions), "by", a.UserID)
	w.WriteHeader(http.StatusNoContent)
}

// stopWorkspaceTask asks the runner to stop a placed task, best effort.
//
// Best effort on purpose: a delete that cannot reach the runner must still drop
// the record, or a customer whose runner is down cannot delete anything. The
// task is then orphaned, which is worth a loud log rather than a failed request
// -- and it is bounded, because a task whose credential no longer resolves gets
// 401 on its next dial and exits.
func (s *Server) stopWorkspaceTask(ctx context.Context, a wsScope) {
	// Deleting, so nothing is preserved: a snapshot of a disk the customer asked
	// us to destroy is a bill for storing it. The workspace's existing snapshot
	// is collected separately, below.
	s.stopTask(ctx, a.TenantID, a.WS.Name, derefOr(a.WS.TaskRef), "deleting the workspace",
		dropDisk())
	if prev := derefOr(a.WS.SnapshotID); prev != "" {
		if runner, err := s.reg.PickRunner(a.TenantID); err == nil {
			if _, err := command(runner, tunnel.MsgDropSnapshot,
				tunnel.DropSnapshot{SnapshotID: prev}, 30*time.Second); err != nil {
				s.log.Warn("a deleted workspace left its snapshot behind",
					"workspace", a.WS.Name, "snapshot", prev, "error", err)
			}
		} else {
			s.log.Warn("a deleted workspace left its snapshot behind; no runner",
				"workspace", a.WS.Name, "snapshot", prev)
		}
	}
}

// stopTask asks the runner to stop a placed task, best effort.
//
// Two callers with the same needs and different scopes: deleting a workspace,
// which has a full request scope, and the idle reaper, which has only a tenant
// and a name. It takes those rather than a wsScope so neither has to invent the
// other's context -- the reaper has no organization slug and no acting user,
// and giving it a hollow one would put a fiction in the log.
//
// NOTE what this does NOT do: touch the generation. An idle or warm-hold stop
// is a stop, not a placement (internal/creds/creds.go).
func (s *Server) stopTask(ctx context.Context, tenantID, name, ref, why string, preserve preserveDisk) {
	if ref == "" {
		return
	}
	runner, err := s.reg.PickRunner(tenantID)
	if err != nil {
		s.log.Warn("a workspace has a placed task and no runner is connected",
			"workspace", name, "ref", ref, "why", why)
		return
	}
	env, err := command(runner, tunnel.MsgStopWorkspace, tunnel.StopWorkspace{
		WorkspaceID: name, Ref: ref, Snapshot: preserve.wanted(),
	}, 90*time.Second)
	if err != nil || env.Type == tunnel.MsgError {
		s.log.Warn("could not stop the workspace task",
			"workspace", name, "ref", ref, "why", why, "error", err)
		return
	}
	var stopped tunnel.Stopped
	_ = env.Decode(&stopped)
	s.log.Info("workspace task stopped",
		"workspace", name, "ref", ref, "why", why, "snapshot", stopped.SnapshotID)
	preserve.record(ctx, s, tenantID, name, stopped.SnapshotID)
}

// preserveDisk says whether a stop should carry the workspace's filesystem
// forward, and holds what it needs to record one.
//
// A type rather than a bool because the two cases need different things: a stop
// that preserves has a workspace row to write the snapshot onto, and one that
// does not is a DELETE, where snapshotting a disk the customer asked us to
// destroy would bill them for storing it.
type preserveDisk struct {
	scope *scope
	id    string
}

// keepDisk preserves the workspace's filesystem and records it on the row.
func keepDisk(sc scope, workspaceID string) preserveDisk {
	return preserveDisk{scope: &sc, id: workspaceID}
}

// dropDisk stops without preserving anything. Used by delete.
func dropDisk() preserveDisk { return preserveDisk{} }

func (p preserveDisk) wanted() bool { return p.scope != nil }

// record writes the new snapshot and then collects the one it displaced.
//
// THE ORDER IS THE RULE. A workspace's snapshot is its filesystem between
// tasks, so the row names the new one before anything touches the old one.
// Crashing between the two leaks a snapshot, which costs money and is visible;
// the other order loses every member's home, silently, and the workspace comes
// back blank.
func (p preserveDisk) record(ctx context.Context, s *Server, tenantID, name, snapshot string) {
	if p.scope == nil || snapshot == "" {
		return
	}
	previous, err := s.setWorkspaceSnapshot(ctx, *p.scope, p.id, snapshot)
	if err != nil {
		// The snapshot exists and nothing knows about it. Logged loudly with the
		// id, because that string is the only remaining handle on a workspace
		// somebody may be about to find empty.
		s.log.Error("a workspace disk was preserved and could not be recorded",
			"workspace", name, "snapshot", snapshot, "error", err)
		return
	}
	if previous == "" {
		return
	}
	runner, err := s.reg.PickRunner(tenantID)
	if err != nil {
		s.log.Warn("a displaced snapshot could not be collected", "snapshot", previous)
		return
	}
	if env, err := command(runner, tunnel.MsgDropSnapshot,
		tunnel.DropSnapshot{SnapshotID: previous}, 30*time.Second); err != nil ||
		env.Type == tunnel.MsgError {
		// A leak, not a loss: the row already names the snapshot that matters.
		s.log.Warn("a displaced snapshot was left behind and will bill until it "+
			"is removed", "workspace", name, "snapshot", previous, "error", err)
	}
}

// workspaceRecord maps a stored workspace to the document every endpoint
// returns, without the liveness fields that need a tunnel round trip.
//
// The ONE place that mapping lives. It was copied into the two console handlers
// as well, and three copies of a field list is three chances to add a column
// that only two of them report -- which is not a compile error, because a struct
// literal with named fields is perfectly happy to leave one zero. Callers that
// need more (owner display name, connectedness, session counts) start here and
// add.
func (s *Server) workspaceRecord(ws Workspace) workspaceDoc {
	return workspaceDoc{
		ID:          ws.Name,
		UUID:        ws.ID,
		TenantID:    ws.TenantID,
		Status:      ws.Status,
		Generation:  uint64(ws.Generation),
		Owner:       ws.OwnerUserID,
		TaskRef:     derefOr(ws.TaskRef),
		AccessScope: ws.AccessScope,
		Policy: workspacePolicyDoc{
			IdleTimeoutSecs:  ptr(int(ws.IdleTimeoutSecs)),
			WarmHoldSecs:     ptr(int(ws.WarmHoldSecs)),
			AdmissionPolicy:  ptr(ws.AdmissionPolicy),
			MaxSessions:      ptr(int(ws.MaxSessions)),
			MinFreeMemoryPct: ptr(int(ws.MinFreeMemoryPct)),
		},
		WarmHoldUntil:   optionalTimestamp(ws.WarmHoldUntil),
		IdlePinnedSince: optionalTimestamp(ws.IdlePinnedSince),
		LastError:       derefOr(ws.LastError),
		CreatedAt:       timestamp(ws.CreatedAt),
	}
}

func ptr[T any](v T) *T { return &v }

// setAccessScope changes who may use a workspace.
//
// Deliberately without the guards renaming needs. The access scope exists only
// in this row -- no registry key, no argv, no derived credential -- so changing
// it under a live task is safe, and refusing it while somebody is working would
// mean an owner cannot revoke access at the one moment they most want to.
//
// Sessions already running are NOT evicted. Narrowing the scope stops the next
// endpoint negotiation rather than killing a conversation mid-sentence, which
// matches how every other authorisation change here behaves.
func (s *Server) setAccessScope(ctx context.Context, sc store.Scope, id, access string) (Workspace, error) {
	var out Workspace
	err := s.st.InTenant(ctx, sc, func(q *celldb.Queries) error {
		ws, err := q.SetWorkspaceAccessScope(ctx, celldb.SetWorkspaceAccessScopeParams{
			ID: id, TenantID: sc.TenantID, AccessScope: access,
		})
		out = ws
		return err
	})
	return out, err
}

func (s *Server) renameWorkspace(ctx context.Context, sc store.Scope, id, name string) (Workspace, error) {
	var out Workspace
	err := s.st.InTenant(ctx, sc, func(q *celldb.Queries) error {
		ws, err := q.RenameWorkspace(ctx, celldb.RenameWorkspaceParams{
			ID: id, TenantID: sc.TenantID, Name: name,
		})
		out = ws
		return err
	})
	return out, err
}

func (s *Server) deleteWorkspace(ctx context.Context, sc store.Scope, id string) error {
	return s.st.InTenant(ctx, sc, func(q *celldb.Queries) error {
		// Idempotent: a retried DELETE must not become a 404, because the
		// caller already got the outcome it asked for.
		_, err := q.DeleteWorkspace(ctx, celldb.DeleteWorkspaceParams{ID: id, TenantID: sc.TenantID})
		return err
	})
}
