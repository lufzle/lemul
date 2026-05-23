package mgmtapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/lufzle/lemul/internal/store"
	"github.com/lufzle/lemul/internal/store/celldb"
)

// Membership as a surface: who is in an organization, who is in a workspace,
// and the verbs that change the second.
//
// The queries have existed since Phase 2 with no callers. What was missing was
// somewhere to hang the permission check, which is what wsScope now provides.

type memberDoc struct {
	// UserID is what the membership verbs take. It is a uuid rather than an
	// email because an email is not stable -- and because a user may have none
	// until they have relayed an ID token.
	UserID string `json:"user_id"`
	Email  string `json:"email,omitempty"`
	Role   string `json:"role"`
	// Me marks the caller's own row, so a client can grey out "remove yourself"
	// without having to learn its own user id from somewhere else. Nothing else
	// in the API tells a client who it is.
	Me bool `json:"me,omitempty"`
}

type memberListResponse struct {
	Members []memberDoc `json:"members"`
}

// handleListOrgMembers answers "who is in this organization", which is what an
// owner needs before they can add anyone to a workspace: the membership verbs
// take a user id, and this is the only place to get one.
//
// Any member may read it. Knowing who your colleagues are is not privileged
// inside an organization you are already in, and withholding it would make
// adding somebody a guessing game.
func (s *Server) handleListOrgMembers(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	var rows []celldb.ListOrgMembersRow
	err := s.st.InTenant(ctx, sc.Scope, func(q *celldb.Queries) error {
		var err error
		rows, err = q.ListOrgMembers(ctx, sc.TenantID)
		return err
	})
	if err != nil {
		s.log.Error("listing organization members", "org", sc.Org.Slug, "error", err)
		http.Error(w, "could not list members", http.StatusInternalServerError)
		return
	}
	docs := make([]memberDoc, 0, len(rows))
	for _, m := range rows {
		d := memberDoc{UserID: m.ID, Role: m.Role, Me: m.ID == sc.UserID}
		if m.Email != nil {
			d.Email = *m.Email
		}
		docs = append(docs, d)
	}
	writeJSON(w, http.StatusOK, memberListResponse{Members: docs})
}

// handleListWorkspaceMembers reports who has been added to one workspace.
// Any member of the workspace may read it; a non-member never gets here.
func (s *Server) handleListWorkspaceMembers(w http.ResponseWriter, r *http.Request) {
	a, ok := s.requireWorkspace(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	var rows []celldb.ListMembersRow
	err := s.st.InTenant(ctx, a.Scope, func(q *celldb.Queries) error {
		var err error
		rows, err = q.ListMembers(ctx, celldb.ListMembersParams{
			WorkspaceID: a.WS.ID, TenantID: a.TenantID,
		})
		return err
	})
	if err != nil {
		s.log.Error("listing workspace members", "workspace", a.WS.Name, "error", err)
		http.Error(w, "could not list members", http.StatusInternalServerError)
		return
	}
	docs := make([]memberDoc, 0, len(rows))
	for _, m := range rows {
		d := memberDoc{UserID: m.UserID, Role: m.Role, Me: m.UserID == a.UserID}
		if m.Email != nil {
			d.Email = *m.Email
		}
		docs = append(docs, d)
	}
	writeJSON(w, http.StatusOK, memberListResponse{Members: docs})
}

type putMemberRequest struct {
	// Role the member gets. Empty means "user"; silently granting ownership
	// would be the wrong default to pick on somebody's behalf.
	Role string `json:"role"`
}

// handlePutWorkspaceMember adds a member, or changes their role. Owner only.
//
// It verifies the target is in the ORGANIZATION first, and that check is
// load-bearing rather than tidy: workspace_member's WITH CHECK is scoped to the
// tenant and nothing else, so without it any uuid at all -- including a user
// belonging to another organization entirely -- could be written in, and the
// row would then satisfy every policy that reads it.
func (s *Server) handlePutWorkspaceMember(w http.ResponseWriter, r *http.Request) {
	a, ok := s.requireWorkspaceOwner(w, r)
	if !ok {
		return
	}
	userID := r.PathValue("user")

	var req putMemberRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil &&
		!errors.Is(err, io.EOF) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	role := req.Role
	if role == "" {
		role = roleUser
	}
	if role != roleOwner && role != roleUser {
		http.Error(w, "role must be owner or user", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	err := s.st.InTenant(ctx, a.Scope, func(q *celldb.Queries) error {
		if _, err := q.GetOrgMembership(ctx, celldb.GetOrgMembershipParams{
			TenantID: a.TenantID, UserID: userID,
		}); err != nil {
			return err
		}
		_, err := q.AddMember(ctx, celldb.AddMemberParams{
			WorkspaceID: a.WS.ID, UserID: userID, TenantID: a.TenantID, Role: role,
		})
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "that user is not a member of this organization; invite them to it first",
			http.StatusNotFound)
		return
	} else if err != nil {
		s.log.Error("adding a workspace member",
			"workspace", a.WS.Name, "user", userID, "error", err)
		http.Error(w, "could not add the member", http.StatusInternalServerError)
		return
	}
	s.log.Info("workspace member added",
		"org", a.Org.Slug, "workspace", a.WS.Name, "user", userID, "role", role, "by", a.UserID)
	writeJSON(w, http.StatusOK, memberDoc{UserID: userID, Role: role, Me: userID == a.UserID})
}

// handleDeleteWorkspaceMember removes one. Owner only.
//
// Refuses to remove the workspace's own owner_user_id, which would leave the
// record owned by somebody who can no longer reach it -- and, if they were the
// last owner, leave nobody who can undo it.
func (s *Server) handleDeleteWorkspaceMember(w http.ResponseWriter, r *http.Request) {
	a, ok := s.requireWorkspaceOwner(w, r)
	if !ok {
		return
	}
	userID := r.PathValue("user")
	if userID == a.WS.OwnerUserID {
		http.Error(w, "this is the workspace's owner, who cannot be removed from it",
			http.StatusConflict)
		return
	}

	ctx := r.Context()
	err := s.st.InTenant(ctx, a.Scope, func(q *celldb.Queries) error {
		// Idempotent: a retried removal must not become a 404, because the
		// caller already got the outcome it asked for.
		_, err := q.RemoveMember(ctx, celldb.RemoveMemberParams{
			WorkspaceID: a.WS.ID, UserID: userID, TenantID: a.TenantID,
		})
		return err
	})
	if err != nil {
		s.log.Error("removing a workspace member",
			"workspace", a.WS.Name, "user", userID, "error", err)
		http.Error(w, "could not remove the member", http.StatusInternalServerError)
		return
	}
	s.log.Info("workspace member removed",
		"org", a.Org.Slug, "workspace", a.WS.Name, "user", userID, "by", a.UserID)
	w.WriteHeader(http.StatusNoContent)
}
