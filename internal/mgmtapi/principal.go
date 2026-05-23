package mgmtapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/lufzle/lemul/internal/auth"
	"github.com/lufzle/lemul/internal/directory"
	"github.com/lufzle/lemul/internal/store"
	"github.com/lufzle/lemul/internal/store/celldb"
)

// The two-step request path, and where identity stops being a token.
//
// A validated access token gives an issuer and a subject and nothing else. Two
// lookups turn that into something a query can be scoped to:
//
//	directory   (issuer, subject) -> home organization -> cell
//	cell        (issuer, subject) -> app_user.id
//
// This has to exist even with one cell. Without it every handler would be
// written against a fixed tenant and rewritten the day there are two -- which
// is precisely the position Phase 2 left behind.

// errNoOrg is what a caller sees for an organization that does not exist AND
// for one they are not a member of.
//
// Deliberately one error. Distinguishing them would answer "does this
// organization exist" for anyone who can sign in, and organization slugs are
// printed in URLs and pasted into chat -- so a 403 on a real one is a
// membership oracle. 404 for both.
var errNoOrg = errors.New("no such organization")

// scope is a resolved (organization, user, role) triple: everything a
// tenant-scoped query and an authorisation decision need.
//
// It replaces Server.scope(), which returned a fixed tenant and system user.
// The methods in state.go take one rather than reaching for a field, so a
// handler cannot run a query without having first said which organization it is
// acting in.
type scope struct {
	store.Scope
	Org directory.Org
	// Role is the caller's role in Org: "owner" or "user".
	Role string
	// Principal is who is acting, carried along so handlers do not have to pull
	// it out of the context a second time.
	Principal auth.Principal
}

// isOwner reports whether the caller may administer the organization itself --
// issue invites, manage members. Distinct from owning a workspace inside it.
func (sc scope) isOwner() bool { return sc.Role == roleOwner }

// Organization roles. Mirrored from the CHECK constraint in schema.sql; the
// database is the authority.
const (
	roleOwner = "owner"
	roleUser  = "user"
)

// Workspace access scopes -- WHO MAY USE a workspace, which is a different
// question from who administers it (that is workspace_member.role).
//
// Also mirrored from a CHECK constraint, and deliberately not the same
// vocabulary as the roles above: reusing "owner" for both would make the two
// independent axes look like one, which is the confusion section 2.5 already
// had to unpick once.
const (
	accessOwner   = "owner"   // the owner alone, plus organization owners
	accessMembers = "members" // explicit workspace_member rows
	accessOrg     = "org"     // anybody in the organization
)

// validAccessScopes is the set a client may ask for, and the reason there is a
// set at all: the database CHECK would refuse a bad value, but as a 500 rather
// than as a sentence naming what was wrong.
var validAccessScopes = map[string]bool{
	accessOwner: true, accessMembers: true, accessOrg: true,
}

// protect authenticates a request and resolves its principal.
//
// The two steps are one wrapper because they are never useful apart: a subject
// with no user row behind it cannot be authorised against anything, and every
// protected handler needs the resolved half.
func (s *Server) protect(h http.HandlerFunc) http.HandlerFunc {
	return s.auth.Wrap(func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.PrincipalFrom(r.Context())
		if !ok || p.Subject == "" {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		full, err := s.resolvePrincipal(r.Context(), p)
		if err != nil {
			s.log.Error("resolving the caller", "subject", p.Subject, "error", err)
			http.Error(w, "could not resolve the caller", http.StatusInternalServerError)
			return
		}
		h(w, r.WithContext(auth.WithPrincipal(r.Context(), full)))
	})
}

// resolvePrincipal turns a token identity into a user and a home organization,
// signing them up if this is the first time we have seen them.
//
// Sign-up on first request rather than at an explicit endpoint: there is no
// moment a client could call one that is not simply "before the first real
// request", and making every client remember to do it first would mean every
// client eventually forgetting.
func (s *Server) resolvePrincipal(ctx context.Context, p auth.Principal) (auth.Principal, error) {
	home, err := s.dir.LookupHome(ctx, p.Issuer, p.Subject)
	switch {
	case err == nil:
	case errors.Is(err, directory.ErrNotFound):
		if home, err = s.signUp(ctx, p); err != nil {
			return p, err
		}
	default:
		return p, fmt.Errorf("directory lookup: %w", err)
	}

	userID, err := s.ensureUser(ctx, home.ID, p.Issuer, p.Subject)
	if err != nil {
		return p, err
	}
	p.UserID = userID
	p.HomeOrgID = home.ID
	return p, nil
}

// signUp creates a principal's own organization and records where it lives.
//
// The order is: create the organization, then claim the binding. Claiming
// second is what makes a concurrent first request safe -- BindPrincipal is
// ON CONFLICT DO NOTHING, so exactly one caller wins, and the loser discards
// the organization it optimistically created rather than leaving a person with
// two homes.
func (s *Server) signUp(ctx context.Context, p auth.Principal) (directory.Home, error) {
	// The empty name asks CreateOrg for its slug as the placeholder. It must NOT
	// be applied through NameOrg, which would spend the one-shot permission to
	// replace it and leave the organization called `forty-crimson-windmill` for
	// good.
	org, err := s.dir.CreateOrg(ctx, newUUID(), s.opt.CellID, "", true)
	if err != nil {
		return directory.Home{}, err
	}

	won, err := s.dir.Bind(ctx, p.Issuer, p.Subject, org.ID)
	if err != nil {
		return directory.Home{}, err
	}
	if !won {
		// Lost the race. Drop ours and adopt the winner's -- and do NOT recurse
		// into signUp, because a second lookup that also missed would mean the
		// binding vanished between two statements, which is not a case retrying
		// fixes.
		if err := s.dir.DeleteOrg(ctx, org.ID); err != nil {
			// Not fatal: an orphaned organization nobody is bound to is
			// invisible, where failing the request would deny a user their
			// first sign-in over a cleanup.
			s.log.Warn("could not remove the organization that lost a signup race",
				"org", org.ID, "error", err)
		}
		return s.dir.LookupHome(ctx, p.Issuer, p.Subject)
	}

	s.log.Info("organization created", "org", org.Slug, "org_id", org.ID, "personal", true)
	return directory.Home{Org: org}, nil
}

// ensureUser finds or creates the caller's row in the cell, and their ownership
// of their own organization.
//
// Idempotent on purpose. Two concurrent first requests both reach here with the
// same home organization, and app_user's global UNIQUE (idp_issuer,
// idp_subject) is what decides between them -- so the loser re-reads rather
// than failing. Postgres aborts a transaction on any statement error, so the
// retry has to be a fresh transaction, not a second attempt inside one.
func (s *Server) ensureUser(ctx context.Context, orgID, issuer, subject string) (string, error) {
	sc := store.Scope{TenantID: orgID}
	for attempt := range 2 {
		var userID string
		err := s.st.InTenant(ctx, sc, func(q *celldb.Queries) error {
			u, err := q.GetUserBySubject(ctx, celldb.GetUserBySubjectParams{
				IdpIssuer: issuer, IdpSubject: subject,
			})
			switch {
			case err == nil:
				userID = u.ID
			case errors.Is(err, store.ErrNotFound):
				created, err := q.CreateUser(ctx, celldb.CreateUserParams{
					ID: newUUID(), HomeTenantID: orgID,
					IdpIssuer: issuer, IdpSubject: subject,
				})
				if err != nil {
					return err
				}
				userID = created.ID
			default:
				return err
			}
			// Every user owns their own organization. Written here rather than
			// in signUp so that a binding created by one request and a user row
			// created by another still converge on a complete state.
			_, err = q.AddOrgMember(ctx, celldb.AddOrgMemberParams{
				TenantID: orgID, UserID: userID, Role: roleOwner,
			})
			return err
		})
		if err == nil {
			return userID, nil
		}
		if attempt == 0 && directory.IsUniqueViolation(err) {
			continue
		}
		return "", fmt.Errorf("resolving the user in the cell: %w", err)
	}
	return "", errors.New("could not resolve the user in the cell")
}

// orgScope resolves the {org} path segment into something queries can run
// under, refusing a caller who is not a member.
//
// This is the authorisation choke point for everything organization-scoped.
// Membership is read from the database on every request rather than cached in
// the principal, because removing someone from an organization has to take
// effect on their next request, not on their next sign-in.
func (s *Server) orgScope(r *http.Request) (scope, error) {
	ctx := r.Context()
	p, ok := auth.PrincipalFrom(ctx)
	if !ok || !p.Resolved() {
		return scope{}, errors.New("unresolved principal")
	}
	org, err := s.dir.OrgBySlug(ctx, r.PathValue("org"))
	if errors.Is(err, directory.ErrNotFound) {
		return scope{}, errNoOrg
	} else if err != nil {
		return scope{}, err
	}

	sc := scope{
		Scope:     store.Scope{TenantID: org.ID, UserID: p.UserID},
		Org:       org,
		Principal: p,
	}
	err = s.st.InTenant(ctx, sc.Scope, func(q *celldb.Queries) error {
		m, err := q.GetOrgMembership(ctx, celldb.GetOrgMembershipParams{
			TenantID: org.ID, UserID: p.UserID,
		})
		if err != nil {
			return err
		}
		sc.Role = m.Role
		return nil
	})
	if errors.Is(err, store.ErrNotFound) {
		return scope{}, errNoOrg
	} else if err != nil {
		return scope{}, err
	}
	return sc, nil
}

// requireOrg is orgScope with the error already written. Handlers that need a
// scope and nothing else start with it.
func (s *Server) requireOrg(w http.ResponseWriter, r *http.Request) (scope, bool) {
	sc, err := s.orgScope(r)
	switch {
	case errors.Is(err, errNoOrg):
		http.Error(w, errNoOrg.Error(), http.StatusNotFound)
		return scope{}, false
	case err != nil:
		s.log.Error("resolving the organization", "org", r.PathValue("org"), "error", err)
		http.Error(w, "could not resolve the organization", http.StatusInternalServerError)
		return scope{}, false
	}
	return sc, true
}

// requireOwner is requireOrg for the acts only an organization owner may
// perform. 403 rather than 404 here: the caller has already proven membership,
// so the organization's existence is not what is being protected.
func (s *Server) requireOwner(w http.ResponseWriter, r *http.Request) (scope, bool) {
	sc, ok := s.requireOrg(w, r)
	if !ok {
		return scope{}, false
	}
	if !sc.isOwner() {
		http.Error(w, "this requires the owner role in "+sc.Org.Slug, http.StatusForbidden)
		return scope{}, false
	}
	return sc, true
}
