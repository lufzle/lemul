package mgmtapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/lufzle/lemul/internal/store"
	"github.com/lufzle/lemul/internal/store/celldb"
)

// Authorisation inside an organization.
//
// Phase 3 made the organization structural -- it is a path segment, it scopes
// the transaction, and every policy compares against it -- so a member of one
// organization cannot reach another. Within one, there was nothing: every
// handler taking a {wid} checked organization membership and stopped there.
//
// This file is the missing half. It mirrors principal.go's
// orgScope/requireOrg/requireOwner trio one level down, and the reasoning
// carries over unchanged:
//
//   - A caller with no standing in a workspace gets 404, not 403. Whether a
//     workspace exists is not a stranger's business, and its name travels in
//     URLs and chat messages exactly as an organization slug does. The body
//     must not echo the name back either, which is why errNoWorkspace carries
//     none.
//   - Membership is read per request rather than cached on the principal, so
//     removing somebody takes effect on their next request instead of their
//     next sign-in.
//
// Sessions get their own resolution on top, because the two questions differ:
// a session's KEYBOARD belongs to the person who started it, while
// administering one -- listing, stopping, deleting -- follows the workspace
// (section 2.5).

var errNoWorkspace = errors.New("no such workspace")

// wsScope is a resolved (organization, user, workspace, role) tuple: everything
// an authorisation decision about one workspace needs.
type wsScope struct {
	scope
	WS Workspace
	// Role is the caller's role in WS: "owner" or "user". An organization owner
	// is reported "owner" here without holding a workspace_member row.
	Role string
}

// isOwner reports whether the caller may administer this workspace: rename it,
// delete it, manage its members, and see every session in it.
func (a wsScope) isOwner() bool { return a.Role == roleOwner }

// sessScope is a session together with the caller's standing in its workspace.
type sessScope struct {
	wsScope
	Sess Session
}

// isMine reports whether the caller started this session. It is the only thing
// that grants a control attach: a session is user-specific (section 2.3), and
// authorising attach at the workspace level would drop one person into
// another's live conversation.
func (a sessScope) isMine() bool { return a.Sess.UserID == a.UserID }

// isVisible reports whether the caller may know this session exists at all.
//
// It matches what listSessions returns -- your own, plus everything in a
// workspace you own -- so a session a caller cannot see in a listing answers
// 404 everywhere else too, rather than 403 revealing that it is there.
func (a sessScope) isVisible() bool { return a.isMine() || a.isOwner() }

// workspaceScope resolves the name a client used and the caller's standing in
// it.
func (s *Server) workspaceScope(ctx context.Context, sc scope, name string) (wsScope, error) {
	ws, err := s.getWorkspaceByName(ctx, sc, name)
	if errors.Is(err, store.ErrNotFound) {
		return wsScope{}, errNoWorkspace
	} else if err != nil {
		return wsScope{}, err
	}
	return s.accessTo(ctx, sc, ws)
}

// workspaceScopeByID is the same from a workspace uuid, which is what a session
// record carries. Both exist rather than one, because id and name are two
// identifiers for one thing and converting between them at a call site is the
// trap section 2.8 records -- state.go owns that conversion, and these own the
// authorisation.
func (s *Server) workspaceScopeByID(ctx context.Context, sc scope, id string) (wsScope, error) {
	var ws Workspace
	err := s.st.InTenant(ctx, sc.Scope, func(q *celldb.Queries) error {
		w, err := q.GetWorkspace(ctx, celldb.GetWorkspaceParams{ID: id, TenantID: sc.TenantID})
		ws = w
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return wsScope{}, errNoWorkspace
	} else if err != nil {
		return wsScope{}, err
	}
	return s.accessTo(ctx, sc, ws)
}

// accessTo decides what the caller is to a workspace it has already resolved.
//
// An organization owner counts as a workspace owner throughout their
// organization, so administering an organization does not mean being added to
// every workspace in it one at a time (section 2.5). Downward only: owning a
// workspace grants nothing at the organization level.
func (s *Server) accessTo(ctx context.Context, sc scope, ws Workspace) (wsScope, error) {
	out := wsScope{scope: sc, WS: ws}
	if sc.isOwner() {
		out.Role = roleOwner
		return out, nil
	}
	err := s.st.InTenant(ctx, sc.Scope, func(q *celldb.Queries) error {
		m, err := q.GetMembership(ctx, celldb.GetMembershipParams{
			WorkspaceID: ws.ID, UserID: sc.UserID, TenantID: sc.TenantID,
		})
		if err != nil {
			return err
		}
		out.Role = m.Role
		return nil
	})
	// An explicit membership row wins, because it can say "owner" where the
	// access scope can only ever say "may use this".
	if err == nil {
		return out, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return wsScope{}, err
	}
	// No membership row. A workspace open to the whole organization still admits
	// this caller, as a plain user -- and the check costs nothing, because being
	// in the organization was established before any row here was read (the
	// {org} path segment scopes the transaction, section 2.6). That is what
	// makes 'org' cheap now where section 2.5 rejected it as needing a user list.
	if ws.AccessScope == accessOrg {
		out.Role = roleUser
		return out, nil
	}
	// Otherwise no membership row is not an error: it is the answer, and it is
	// the same answer as "there is no such workspace".
	return wsScope{}, errNoWorkspace
}

// sessionAccess resolves a session and the caller's standing in its workspace.
func (s *Server) sessionAccess(ctx context.Context, sc scope, sess Session) (sessScope, error) {
	ws, err := s.workspaceScopeByID(ctx, sc, sess.WorkspaceID)
	if err != nil {
		return sessScope{}, err
	}
	return sessScope{wsScope: ws, Sess: sess}, nil
}

// requireWorkspace is the gate every handler taking a {wid} starts with. It
// resolves the organization, the workspace and the caller's role in one step,
// so that reaching the body of a handler already means being allowed in.
func (s *Server) requireWorkspace(w http.ResponseWriter, r *http.Request) (wsScope, bool) {
	sc, ok := s.requireOrg(w, r)
	if !ok {
		return wsScope{}, false
	}
	ws, err := s.workspaceScope(r.Context(), sc, r.PathValue("wid"))
	return s.writeWorkspaceError(w, r, ws, err)
}

// requireWorkspaceOwner is requireWorkspace for the acts only a workspace owner
// may perform: rename, delete, and membership.
//
// 403 rather than 404 here, and the distinction is deliberate: the caller has
// already proven they can see the workspace, so its existence is not what is
// being protected any more.
func (s *Server) requireWorkspaceOwner(w http.ResponseWriter, r *http.Request) (wsScope, bool) {
	ws, ok := s.requireWorkspace(w, r)
	if !ok {
		return wsScope{}, false
	}
	if !ws.isOwner() {
		http.Error(w, "this requires the owner role on workspace "+ws.WS.Name,
			http.StatusForbidden)
		return wsScope{}, false
	}
	return ws, true
}

// requireSession resolves a {sid} and refuses a caller who may not know it
// exists. Handlers that then need more than visibility -- attach needs the
// session's own user -- ask on top of it.
func (s *Server) requireSession(w http.ResponseWriter, r *http.Request) (sessScope, bool) {
	sc, ok := s.requireOrg(w, r)
	if !ok {
		return sessScope{}, false
	}
	sess, err := s.getSession(r.Context(), sc, r.PathValue("sid"))
	if err != nil {
		// A session in another organization does not resolve here at all,
		// because the transaction is scoped before the read. So "not yours" and
		// "does not exist" are already the same answer.
		http.Error(w, "no such session", http.StatusNotFound)
		return sessScope{}, false
	}
	a, err := s.sessionAccess(r.Context(), sc, sess)
	switch {
	case errors.Is(err, errNoWorkspace):
		http.Error(w, "no such session", http.StatusNotFound)
		return sessScope{}, false
	case err != nil:
		s.log.Error("resolving the session's workspace",
			"session", sess.ID, "org", sc.Org.Slug, "error", err)
		http.Error(w, "could not resolve the session", http.StatusInternalServerError)
		return sessScope{}, false
	}
	if !a.isVisible() {
		// Same answer as a session that is not there. A plain member sees only
		// their own sessions in a listing, so anything else must be absent here
		// too or the listing becomes the weaker of two statements.
		http.Error(w, "no such session", http.StatusNotFound)
		return sessScope{}, false
	}
	return a, true
}

// writeWorkspaceError turns a resolution failure into a response, so the two
// require* helpers cannot disagree about which code a missing workspace gets.
func (s *Server) writeWorkspaceError(w http.ResponseWriter, r *http.Request, ws wsScope, err error) (wsScope, bool) {
	switch {
	case errors.Is(err, errNoWorkspace):
		// Carries no name: a 404 that echoed it back would confirm the name a
		// caller guessed, which is the whole thing being withheld.
		http.Error(w, errNoWorkspace.Error(), http.StatusNotFound)
		return wsScope{}, false
	case err != nil:
		s.log.Error("resolving the workspace",
			"workspace", r.PathValue("wid"), "org", ws.Org.Slug, "error", err)
		http.Error(w, "could not resolve the workspace", http.StatusInternalServerError)
		return wsScope{}, false
	}
	return ws, true
}
