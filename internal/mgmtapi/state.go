package mgmtapi

import (
	"context"
	"errors"
	"time"

	"github.com/lufzle/lemul/internal/auth"
	"github.com/lufzle/lemul/internal/registry"
	"github.com/lufzle/lemul/internal/store"
	"github.com/lufzle/lemul/internal/store/celldb"
)

// The control plane's view of its cell database.
//
// Every method opens a tenant-scoped transaction, so nothing above this file
// has to remember to -- and nothing above it CAN forget, because the queries
// are not reachable any other way (internal/store InTenant).
//
// Every method also takes a scope rather than reading one off the server. That
// is the whole of what Phase 3 changed here: the organization and the acting
// user are resolved per request (principal.go), so a handler cannot run a query
// without having first said, and proved, which organization it is acting in.

// Workspace is what the handlers work with. It is celldb.Workspace by another
// name, kept separate so the generated type does not leak into the API layer.
type Workspace = celldb.Workspace

// Session likewise.
type Session = celldb.Session

// Workspace status values, on the compute axis of section 2.4. Mirrored from
// the CHECK constraint in schema.sql; the database is the authority.
const (
	WorkspaceStopped  = "stopped"
	WorkspaceStarting = "starting"
	WorkspaceActive   = "active"
)

// storeScopeFor is the scope for the few reads that are about a person rather
// than about an organization. It uses their home organization, which every user
// has and cannot leave, so there is always one to name.
func storeScopeFor(p auth.Principal) store.Scope {
	return store.Scope{TenantID: p.HomeOrgID, UserID: p.UserID}
}

// getWorkspaceByName resolves the name a client used.
//
// Phase 1 addressed workspaces by name and the URL still carries one. The
// schema separates id from name, which is what lets Phase 4 give a workspace a
// stable identity that renaming does not break.
func (s *Server) getWorkspaceByName(ctx context.Context, sc scope, name string) (Workspace, error) {
	var out Workspace
	err := s.st.InTenant(ctx, sc.Scope, func(q *celldb.Queries) error {
		w, err := q.GetWorkspaceByName(ctx, celldb.GetWorkspaceByNameParams{
			Name: name, TenantID: sc.TenantID,
		})
		out = w
		return err
	})
	return out, err
}

// createWorkspace makes one on demand, as Phase 1 did.
//
// Creating it also writes an owner membership row, so "which workspaces may I
// see" is one query against workspace_member rather than a union of ownership
// and membership. Phase 4 adds the real create API and the permission check;
// this keeps the existing behaviour working on the new schema.
func (s *Server) createWorkspace(ctx context.Context, sc scope, name, access string) (Workspace, error) {
	// Defaulting here rather than at each call site: an empty string is not a
	// storable access scope (the CHECK refuses it), so a caller that forgets
	// must get the closed answer rather than a constraint violation.
	if access == "" {
		access = accessOwner
	}
	var out Workspace
	err := s.st.InTenant(ctx, sc.Scope, func(q *celldb.Queries) error {
		w, err := q.CreateWorkspace(ctx, celldb.CreateWorkspaceParams{
			ID:          newUUID(),
			TenantID:    sc.TenantID,
			Name:        name,
			OwnerUserID: sc.UserID,
			AccessScope: access,
		})
		if err != nil {
			return err
		}
		if _, err := q.AddMember(ctx, celldb.AddMemberParams{
			WorkspaceID: w.ID, UserID: sc.UserID, TenantID: sc.TenantID, Role: roleOwner,
		}); err != nil {
			return err
		}
		out = w
		return nil
	})
	return out, err
}

// workspaceName resolves a workspace uuid to the name everything OUTSIDE the
// database is keyed on.
//
// Splitting id from name introduced two identifiers for one thing, and the
// registry, the supervisor's -workspace argv and the client-facing URL all use
// the NAME while the schema uses the uuid. Passing one where the other was
// expected placed a SECOND task that registered its tunnel under the uuid --
// visible only as two "workspace tunnel up" lines and a session that never
// looked live.
func (s *Server) workspaceName(ctx context.Context, sc scope, id string) (string, error) {
	var name string
	err := s.st.InTenant(ctx, sc.Scope, func(q *celldb.Queries) error {
		w, err := q.GetWorkspace(ctx, celldb.GetWorkspaceParams{ID: id, TenantID: sc.TenantID})
		name = w.Name
		return err
	})
	return name, err
}

// tunnelForSession finds the live tunnel for a session's workspace.
//
// Exists so the uuid -> name conversion happens in ONE place. Every registry
// lookup, the supervisor's -workspace argv, the preflight store and the
// client-facing URL are keyed on the NAME; only the schema uses the uuid.
// Passing one where the other belongs is silent -- it places a second task, or
// waits out a preflight for a workspace that never reports one -- so the
// conversion should not be open-coded at each site.
func (s *Server) tunnelForSession(ctx context.Context, sc scope, sess Session) (*registry.Tunnel, string, error) {
	name, err := s.workspaceName(ctx, sc, sess.WorkspaceID)
	if err != nil {
		return nil, "", err
	}
	t, err := s.reg.PickWorkspace(sc.TenantID, name)
	return t, name, err
}

func (s *Server) setWorkspaceStatus(ctx context.Context, sc scope, id, status, taskRef string) error {
	return s.st.InTenant(ctx, sc.Scope, func(q *celldb.Queries) error {
		ref := &taskRef
		if taskRef == "" {
			ref = nil
		}
		return q.SetWorkspaceStatus(ctx, celldb.SetWorkspaceStatusParams{
			ID: id, TenantID: sc.TenantID, Status: status, TaskRef: ref,
		})
	})
}

// failWorkspacePlacement records why a placement failed, together with the
// status that says whether a task may nonetheless exist (queries.sql).
//
// It is the counterpart of setWorkspaceStatus, which CLEARS the message: between
// them, last_error always describes the most recent attempt and never an older
// one. Splitting it that way rather than having one call site remember to clear
// is deliberate -- there are five status writers and one failure writer, and the
// rule has to hold for all six.
func (s *Server) failWorkspacePlacement(ctx context.Context, sc scope, id, status, taskRef, reason string) error {
	return s.st.InTenant(ctx, sc.Scope, func(q *celldb.Queries) error {
		ref := &taskRef
		if taskRef == "" {
			ref = nil
		}
		return q.FailWorkspacePlacement(ctx, celldb.FailWorkspacePlacementParams{
			ID: id, TenantID: sc.TenantID, Status: status, TaskRef: ref, LastError: &reason,
		})
	})
}

// setWorkspacePolicy writes section 2.4's lifecycle policy, all five fields in
// one statement so a PATCH cannot half-apply.
func (s *Server) setWorkspacePolicy(ctx context.Context, sc store.Scope, ws Workspace) (Workspace, error) {
	var out Workspace
	err := s.st.InTenant(ctx, sc, func(q *celldb.Queries) error {
		w, err := q.SetWorkspacePolicy(ctx, celldb.SetWorkspacePolicyParams{
			ID:               ws.ID,
			TenantID:         sc.TenantID,
			IdleTimeoutSecs:  ws.IdleTimeoutSecs,
			WarmHoldSecs:     ws.WarmHoldSecs,
			AdmissionPolicy:  ws.AdmissionPolicy,
			MaxSessions:      ws.MaxSessions,
			MinFreeMemoryPct: ws.MinFreeMemoryPct,
		})
		out = w
		return err
	})
	return out, err
}

// setSessionIdleTimeout writes section 2.4's per-session override. A nil value
// clears it, which is "inherit the workspace" rather than "never stop" -- the
// two are different and 0 is what says the second.
func (s *Server) setSessionIdleTimeout(ctx context.Context, sc store.Scope, sid string, secs *int32) (Session, error) {
	var out Session
	err := s.st.InTenant(ctx, sc, func(q *celldb.Queries) error {
		sess, err := q.SetSessionIdleTimeout(ctx, celldb.SetSessionIdleTimeoutParams{
			ID: sid, TenantID: sc.TenantID, IdleTimeoutSecs: secs,
		})
		out = sess
		return err
	})
	return out, err
}

// setIdlePinned records, or clears, that this workspace would be stopping but
// for a background process in one of its sessions. Purely a signal for the
// console -- nothing reads it back to make a decision (see reaper.notePinned).
func (s *Server) setIdlePinned(ctx context.Context, sc scope, id string, since *time.Time) error {
	return s.st.InTenant(ctx, sc.Scope, func(q *celldb.Queries) error {
		return q.SetIdlePinned(ctx, celldb.SetIdlePinnedParams{
			ID: id, TenantID: sc.TenantID, IdlePinnedSince: since,
		})
	})
}

// setWarmHold writes or clears section 2.4's grace-period deadline.
//
// A nil deadline means no hold is running. Persisted rather than held in this
// process, because a control-plane restart must not orphan a task that was
// mid-hold -- see reaper.go.
func (s *Server) setWarmHold(ctx context.Context, sc scope, id string, until *time.Time) error {
	return s.st.InTenant(ctx, sc.Scope, func(q *celldb.Queries) error {
		return q.SetWarmHold(ctx, celldb.SetWarmHoldParams{
			ID: id, TenantID: sc.TenantID, WarmHoldUntil: until,
		})
	})
}

// setWorkspaceSnapshot records the snapshot a replacement task restores from,
// and returns the one it displaced so the caller can delete it.
//
// The ORDER is the design, not an implementation detail. ECS cannot re-attach a
// volume, so this column is the only thing connecting a stopped workspace to its
// filesystem: write it when the snapshot completes, and delete the previous one
// only after this returns. Crashing between the two leaks a snapshot, which
// costs money. Doing it the other way round loses every member's home, which is
// not a cost.
//
// Returns the PREVIOUS id rather than taking it as an argument, so nothing has
// to re-read the row and observe a snapshot some other placement wrote.
func (s *Server) setWorkspaceSnapshot(ctx context.Context, sc scope, id, snapshot string) (previous string, err error) {
	var next *string
	if snapshot != "" {
		next = &snapshot
	}
	err = s.st.InTenant(ctx, sc.Scope, func(q *celldb.Queries) error {
		before, err := q.GetWorkspace(ctx, celldb.GetWorkspaceParams{
			ID: id, TenantID: sc.TenantID,
		})
		if err != nil {
			return err
		}
		previous = derefOr(before.SnapshotID)
		_, err = q.SetWorkspaceSnapshot(ctx, celldb.SetWorkspaceSnapshotParams{
			ID: id, TenantID: sc.TenantID, SnapshotID: next,
		})
		return err
	})
	if err != nil {
		return "", err
	}
	if previous == snapshot {
		// Nothing displaced: the same snapshot written twice, which a retried
		// stop produces. Returning it would ask the caller to delete the
		// snapshot the row now points at.
		return "", nil
	}
	return previous, nil
}

// nextGeneration increments and returns in one statement.
//
// The JSON store read the record, incremented, and wrote back a copy taken
// BEFORE the increment, so the counter never left zero -- and two genuinely
// different placements then derived the same ECS client-token, which ECS
// answers with the original task (section 2.8). UPDATE ... RETURNING cannot
// express that bug.
func (s *Server) nextGeneration(ctx context.Context, sc scope, id string) (int64, error) {
	var gen int64
	err := s.st.InTenant(ctx, sc.Scope, func(q *celldb.Queries) error {
		g, err := q.NextGeneration(ctx, celldb.NextGenerationParams{ID: id, TenantID: sc.TenantID})
		gen = g
		return err
	})
	return gen, err
}

// listWorkspaces reports what the caller may see.
//
// An organization owner sees everything in it; anyone else sees only what they
// own or were added to. Two named queries rather than one with a conditional,
// because the conditional is the part worth being able to read -- and each is
// independently testable (see queries.sql).
func (s *Server) listWorkspaces(ctx context.Context, sc scope) ([]Workspace, error) {
	var out []Workspace
	err := s.st.InTenant(ctx, sc.Scope, func(q *celldb.Queries) error {
		var err error
		if sc.isOwner() {
			out, err = q.ListWorkspaces(ctx, sc.TenantID)
			return err
		}
		out, err = q.ListWorkspacesForUser(ctx, celldb.ListWorkspacesForUserParams{
			TenantID: sc.TenantID, UserID: sc.UserID,
		})
		return err
	})
	return out, err
}

// listWorkspaceSessions reports EVERY session in a workspace, with no
// visibility filtering.
//
// Deliberately not listSessions, which asks "what may this caller see" and
// therefore needs a caller. Background work has none: the idle sweeper acts on
// behalf of the organization rather than a person, and running it through a
// visibility helper puts an empty user id into a uuid comparison -- which is
// how it failed the first time, as a per-sweep error that stopped the whole
// cascade for that workspace while looking like a warning.
//
// Unfiltered is also what it MEANS. A sweeper that only saw one member's
// sessions would stop their idle ones and then conclude the workspace was
// empty, taking a colleague's running session down with the task.
func (s *Server) listWorkspaceSessions(ctx context.Context, sc scope, workspaceID string) ([]Session, error) {
	var out []Session
	err := s.st.InTenant(ctx, sc.Scope, func(q *celldb.Queries) error {
		sessions, err := q.ListSessions(ctx, celldb.ListSessionsParams{
			WorkspaceID: workspaceID, TenantID: sc.TenantID,
		})
		out = sessions
		return err
	})
	return out, err
}

// listOrgSessions reports every session the caller may see anywhere in the
// organization.
//
// It exists for `lem --session <id>`, which takes a unique PREFIX and is not
// told a workspace -- the endpoint it then calls is org-scoped, so requiring a
// workspace only to resolve the id would be asking the user for something the
// API does not need.
func (s *Server) listOrgSessions(ctx context.Context, sc scope) ([]Session, error) {
	var out []Session
	err := s.st.InTenant(ctx, sc.Scope, func(q *celldb.Queries) error {
		var err error
		if sc.isOwner() {
			out, err = q.ListOrgSessions(ctx, sc.TenantID)
			return err
		}
		out, err = q.ListOrgSessionsVisible(ctx, celldb.ListOrgSessionsVisibleParams{
			TenantID: sc.TenantID, UserID: sc.UserID,
		})
		return err
	})
	return out, err
}

func (s *Server) createSessionRecord(ctx context.Context, sc scope, workspaceID string) (Session, error) {
	var out Session
	err := s.st.InTenant(ctx, sc.Scope, func(q *celldb.Queries) error {
		sess, err := q.CreateSession(ctx, celldb.CreateSessionParams{
			ID:          newSessionID(),
			WorkspaceID: workspaceID,
			TenantID:    sc.TenantID,
			UserID:      sc.UserID,
		})
		out = sess
		return err
	})
	return out, err
}

func (s *Server) getSession(ctx context.Context, sc scope, sid string) (Session, error) {
	var out Session
	err := s.st.InTenant(ctx, sc.Scope, func(q *celldb.Queries) error {
		sess, err := q.GetSession(ctx, celldb.GetSessionParams{ID: sid, TenantID: sc.TenantID})
		out = sess
		return err
	})
	return out, err
}

// listSessions reports the sessions in a workspace.
//
// A workspace OWNER sees every session in it, whoever started them; a plain
// member sees only their own. That rule is why session.user_id exists at all.
func (s *Server) listSessions(ctx context.Context, sc scope, workspaceID string) ([]Session, error) {
	var out []Session
	err := s.st.InTenant(ctx, sc.Scope, func(q *celldb.Queries) error {
		owner, err := s.ownsWorkspace(ctx, q, sc, workspaceID)
		if err != nil {
			return err
		}
		if owner {
			out, err = q.ListSessions(ctx, celldb.ListSessionsParams{
				WorkspaceID: workspaceID, TenantID: sc.TenantID,
			})
			return err
		}
		out, err = q.ListSessionsForUser(ctx, celldb.ListSessionsForUserParams{
			WorkspaceID: workspaceID, TenantID: sc.TenantID, UserID: sc.UserID,
		})
		return err
	})
	return out, err
}

// ownsWorkspace reports whether the caller may act on a workspace as its owner.
//
// An organization owner counts, so that administering an organization does not
// mean being added to every workspace in it one at a time.
//
// A caller with no membership row is not an owner, and that is an ANSWER rather
// than an error. It read `return false, err` until Phase 4, which turned a
// plain member listing sessions in a workspace they were never added to into a
// 500 -- the sort of thing that looks like a broken control plane while being
// the access rule working. Whether they should have got that far at all is
// requireWorkspace's business now, not this function's.
func (s *Server) ownsWorkspace(ctx context.Context, q *celldb.Queries, sc scope, workspaceID string) (bool, error) {
	if sc.isOwner() {
		return true, nil
	}
	m, err := q.GetMembership(ctx, celldb.GetMembershipParams{
		WorkspaceID: workspaceID, UserID: sc.UserID, TenantID: sc.TenantID,
	})
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return m.Role == roleOwner, nil
}

func (s *Server) deleteSession(ctx context.Context, sc scope, sid string) error {
	return s.st.InTenant(ctx, sc.Scope, func(q *celldb.Queries) error {
		// Idempotent: a retried DELETE must not become a 404, because the caller
		// already got the outcome it asked for.
		_, err := q.DeleteSession(ctx, celldb.DeleteSessionParams{ID: sid, TenantID: sc.TenantID})
		return err
	})
}

// displayNames resolves user ids to something a person can read, for one
// organization. Reads through org_member rather than over app_user, so the
// tenant predicate is expressible -- app_user has no tenant_id.
func (s *Server) displayNames(ctx context.Context, sc scope) map[string]string {
	out := map[string]string{}
	_ = s.st.InTenant(ctx, sc.Scope, func(q *celldb.Queries) error {
		users, err := q.ListOrgMembers(ctx, sc.TenantID)
		if err != nil {
			return err
		}
		for _, u := range users {
			if u.Email != nil {
				out[u.ID] = *u.Email
			}
		}
		return nil
	})
	return out
}

// setUserEmail records an email read from a validated ID token.
func (s *Server) setUserEmail(ctx context.Context, sc store.Scope, userID, email string) error {
	return s.st.InTenant(ctx, sc, func(q *celldb.Queries) error {
		_, err := q.SetUserEmail(ctx, celldb.SetUserEmailParams{ID: userID, Email: &email})
		return err
	})
}

// rolesByOrg lists every organization the caller belongs to, with their role.
//
// The one read that is deliberately NOT scoped to a single organization -- it
// is the question "which organizations am I in", which cannot be asked from
// inside one of them. org_member's policy admits a row by user_id for exactly
// this; see schema.sql.
func (s *Server) rolesByOrg(ctx context.Context, p auth.Principal) (map[string]string, error) {
	roles := map[string]string{}
	err := s.st.InTenant(ctx, storeScopeFor(p), func(q *celldb.Queries) error {
		rows, err := q.ListOrgsForUser(ctx, p.UserID)
		if err != nil {
			return err
		}
		for _, m := range rows {
			roles[m.TenantID] = m.Role
		}
		return nil
	})
	return roles, err
}

// joinOrg adds the caller to an organization at the role an invite granted.
//
// Scoped to the organization being joined rather than to one the caller is
// already in: org_member's WITH CHECK only permits an insert into the scoped
// organization, which is what stops "add myself to yours" from being one
// statement away. The authorisation is the invite code, checked before this
// runs.
func (s *Server) joinOrg(ctx context.Context, orgID string, p auth.Principal, role string) (bool, error) {
	var joined bool
	err := s.st.InTenant(ctx, store.Scope{TenantID: orgID, UserID: p.UserID},
		func(q *celldb.Queries) error {
			n, err := q.JoinOrg(ctx, celldb.JoinOrgParams{
				TenantID: orgID, UserID: p.UserID, Role: role,
			})
			joined = n > 0
			return err
		})
	return joined, err
}

// timestamp renders a database time the way the API already presents them.
func timestamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// derefOr renders a nullable column for the API, which has always presented an
// absent task reference as an empty string.
func derefOr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// optionalTimestamp renders a nullable time as an empty string, which pairs with
// `omitempty` so an unset deadline is absent from the JSON rather than being
// reported as the zero year.
func optionalTimestamp(t *time.Time) string {
	if t == nil {
		return ""
	}
	return timestamp(*t)
}
