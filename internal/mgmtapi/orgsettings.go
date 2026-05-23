package mgmtapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/lufzle/lemul/internal/store"
	"github.com/lufzle/lemul/internal/store/celldb"
)

// Organization settings.
//
// One setting so far, and the shape matters more than the count: these are
// per-organization policy that an owner sets and every other handler reads, so
// they live in the cell next to the rows they govern rather than in the
// directory, which exists only to route a principal to a cell.

// orgSettings is the resolved policy for one organization.
//
// A struct rather than the generated row, because ABSENCE IS AN ANSWER: an
// organization that has never changed a setting has no row, and every reader
// wants the defaults rather than a not-found error. Keeping the defaults in one
// constructor means a new setting cannot acquire a different default in two
// places.
type orgSettings struct {
	// MembersCanCreateWorkspaces is false by default, which is the closed
	// answer: only organization owners create workspaces. A workspace is a task
	// somebody pays for, so the permissive setting is one an owner opts into.
	MembersCanCreateWorkspaces bool `json:"members_can_create_workspaces"`
}

func defaultOrgSettings() orgSettings {
	return orgSettings{MembersCanCreateWorkspaces: false}
}

// readOrgSettings resolves an organization's settings, defaults included.
func (s *Server) readOrgSettings(ctx context.Context, sc scope) (orgSettings, error) {
	out := defaultOrgSettings()
	err := s.st.InTenant(ctx, sc.Scope, func(q *celldb.Queries) error {
		row, err := q.GetOrgSettings(ctx, sc.TenantID)
		if err != nil {
			return err
		}
		out.MembersCanCreateWorkspaces = row.MembersCanCreateWorkspaces
		return nil
	})
	// No row is the common case, not a failure: it means nobody has changed
	// anything, so the defaults stand.
	if errors.Is(err, store.ErrNotFound) {
		return defaultOrgSettings(), nil
	}
	if err != nil {
		return orgSettings{}, err
	}
	return out, nil
}

// mayCreateWorkspace enforces the setting, and writes the refusal itself so
// every creation path gets the same sentence.
//
// An organization owner is never gated: the setting governs MEMBERS, and an
// owner who could not create a workspace could not turn the setting on either
// without a chicken-and-egg problem.
func (s *Server) mayCreateWorkspace(w http.ResponseWriter, r *http.Request, sc scope) bool {
	if sc.isOwner() {
		return true
	}
	set, err := s.readOrgSettings(r.Context(), sc)
	if err != nil {
		s.log.Error("reading organization settings", "org", sc.Org.Slug, "error", err)
		http.Error(w, "could not read the organization settings",
			http.StatusInternalServerError)
		return false
	}
	if set.MembersCanCreateWorkspaces {
		return true
	}
	// 403 rather than 404: the caller has already proven they are in this
	// organization, so there is no existence to protect here -- only a
	// permission to explain. Naming who can change it is the difference between
	// a dead end and a next step.
	http.Error(w,
		"only an owner of "+sc.Org.Slug+" may create workspaces here; "+
			"an owner can allow this under the organization's settings",
		http.StatusForbidden)
	return false
}

// handleGetOrgSettings serves the settings to any member.
//
// Readable by everyone rather than owners only, because a member has to be able
// to find out why creating a workspace was refused -- and the console needs it
// to decide whether to show the button at all.
func (s *Server) handleGetOrgSettings(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	set, err := s.readOrgSettings(r.Context(), sc)
	if err != nil {
		s.log.Error("reading organization settings", "org", sc.Org.Slug, "error", err)
		http.Error(w, "could not read the organization settings",
			http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, set)
}

// updateOrgSettingsRequest uses a pointer per field so that absent means
// "leave it alone" rather than "set it to false".
//
// The same reasoning as createWorkspaceRequest's optionalString: a plain bool
// cannot tell a missing key from an explicit false, and a PATCH that silently
// turns a setting off because a client did not mention it is the kind of bug
// that is only ever found by its consequences.
type updateOrgSettingsRequest struct {
	MembersCanCreateWorkspaces *bool `json:"members_can_create_workspaces"`
}

// handleUpdateOrgSettings changes them. Owner only.
func (s *Server) handleUpdateOrgSettings(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	if !sc.isOwner() {
		http.Error(w, "this requires the owner role in "+sc.Org.Slug,
			http.StatusForbidden)
		return
	}

	var req updateOrgSettingsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	var out orgSettings
	err := s.st.InTenant(ctx, sc.Scope, func(q *celldb.Queries) error {
		// Read-then-upsert inside ONE transaction, because a PATCH that omits a
		// field has to preserve whatever is stored, and the upsert writes every
		// column. Doing the read outside would make two concurrent PATCHes of
		// different fields lose one of them.
		cur := defaultOrgSettings()
		row, err := q.GetOrgSettings(ctx, sc.TenantID)
		switch {
		case err == nil:
			cur.MembersCanCreateWorkspaces = row.MembersCanCreateWorkspaces
		case !errors.Is(err, store.ErrNotFound):
			return err
		}
		if req.MembersCanCreateWorkspaces != nil {
			cur.MembersCanCreateWorkspaces = *req.MembersCanCreateWorkspaces
		}
		saved, err := q.UpsertOrgSettings(ctx, celldb.UpsertOrgSettingsParams{
			TenantID:                   sc.TenantID,
			MembersCanCreateWorkspaces: cur.MembersCanCreateWorkspaces,
		})
		if err != nil {
			return err
		}
		out = orgSettings{MembersCanCreateWorkspaces: saved.MembersCanCreateWorkspaces}
		return nil
	})
	if err != nil {
		s.log.Error("updating organization settings", "org", sc.Org.Slug, "error", err)
		http.Error(w, "could not update the organization settings",
			http.StatusInternalServerError)
		return
	}
	s.log.Info("organization settings updated", "org", sc.Org.Slug, "by", sc.UserID,
		"members_can_create_workspaces", out.MembersCanCreateWorkspaces)
	writeJSON(w, http.StatusOK, out)
}
