package mgmtapi

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lufzle/lemul/internal/auth"
	"github.com/lufzle/lemul/internal/directory"
)

// Organizations, as clients see them.
//
// The internal name is `tenant`, because that is what the isolation mechanism is
// built on -- RLS scopes on tenant_id and the policies would read as if they
// enforced something else if the column were renamed. Everything facing a user
// says organization.

type orgDoc struct {
	// Slug is the addressable identifier: the {org} segment, and what `lem
	// --org` takes. Three generated words rather than anything derived from the
	// display name, which is not unique and changes.
	Slug string `json:"slug"`
	Name string `json:"name"`
	// Role is the CALLER's role here, not a property of the organization. It is
	// on this document because the only reason to list organizations is to act
	// in one, and the client needs to know what it may do before it tries.
	Role string `json:"role"`
	// Personal marks the organization created for this user at sign-up. Clients
	// use it as the default to fall back to; nothing on the server does.
	Personal bool   `json:"personal"`
	Plan     string `json:"plan,omitempty"`
}

type orgListResponse struct {
	Orgs []orgDoc `json:"orgs"`
}

// handleListOrgs answers "which organizations am I in", which is what the
// console's switcher and the CLI's --org default are built on.
//
// It is the one endpoint that is not scoped to a single organization, because
// it is the question you ask before you can name one. Everything it returns is
// about the caller.
func (s *Server) handleListOrgs(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok || !p.Resolved() {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}

	roles, err := s.rolesByOrg(r.Context(), p)
	if err != nil {
		s.log.Error("listing organizations", "user", p.UserID, "error", err)
		http.Error(w, "could not list organizations", http.StatusInternalServerError)
		return
	}
	ids := make([]string, 0, len(roles))
	for id := range roles {
		ids = append(ids, id)
	}
	// Names live in the directory and memberships in the cell -- two databases
	// by design, so this is two queries rather than a join that cannot be
	// written.
	orgs, err := s.dir.Orgs(r.Context(), ids)
	if err != nil {
		s.log.Error("resolving organization names", "user", p.UserID, "error", err)
		http.Error(w, "could not list organizations", http.StatusInternalServerError)
		return
	}

	docs := make([]orgDoc, 0, len(orgs))
	for _, o := range orgs {
		docs = append(docs, orgDoc{
			Slug: o.Slug, Name: o.Name, Role: roles[o.ID],
			Personal: o.Personal, Plan: o.Plan,
		})
	}
	writeJSON(w, http.StatusOK, orgListResponse{Orgs: docs})
}

type createInviteRequest struct {
	// Role the redeemer gets. Empty means "user"; an invite that silently
	// granted ownership would be the wrong default to pick for someone.
	Role string `json:"role"`
	// MaxUses of zero means unlimited, which is the shape a JSON body can
	// express without every caller having to send a null.
	MaxUses int32 `json:"max_uses"`
	// ExpiresInHours of zero uses the default below rather than "never". A code
	// that never expires is a credential nobody remembers issuing.
	ExpiresInHours int `json:"expires_in_hours"`
}

type inviteResponse struct {
	Code      string `json:"code"`
	Org       string `json:"org"`
	Role      string `json:"role"`
	ExpiresAt string `json:"expires_at"`
}

// defaultInviteTTL bounds an invite that did not name its own lifetime. A week
// is long enough to reach somebody and short enough that a code pasted into a
// chat log stops working before the log is forgotten.
const defaultInviteTTL = 7 * 24 * time.Hour

// handleCreateInvite issues a code that adds its redeemer to this organization.
//
// Owner only. Adding people is the one act that changes who can see an
// organization's workspaces, so it is the clearest case for the role to mean
// something.
func (s *Server) handleCreateInvite(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.requireOwner(w, r)
	if !ok {
		return
	}

	var req createInviteRequest
	// An empty body is a valid request for a default invite, so a decode failure
	// on no bytes is not an error worth reporting.
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

	ttl := defaultInviteTTL
	if req.ExpiresInHours > 0 {
		ttl = time.Duration(req.ExpiresInHours) * time.Hour
	}
	expires := time.Now().Add(ttl).UTC()
	var maxUses *int32
	if req.MaxUses > 0 {
		maxUses = &req.MaxUses
	}

	code, err := newInviteCode()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	iv, err := s.dir.CreateInvite(r.Context(), code, sc.Org.ID, role, &expires, maxUses)
	if err != nil {
		s.log.Error("creating an invite", "org", sc.Org.Slug, "error", err)
		http.Error(w, "could not create the invite", http.StatusInternalServerError)
		return
	}
	// The code itself is never logged. It is a bearer credential for membership
	// of an organization, and a log line is exactly the place one gets copied
	// out of.
	s.log.Info("invite created", "org", sc.Org.Slug, "role", role, "by", sc.UserID)

	writeJSON(w, http.StatusCreated, inviteResponse{
		Code: iv.Code, Org: sc.Org.Slug, Role: iv.Role,
		ExpiresAt: timestamp(expires),
	})
}

type redeemResponse struct {
	Org  string `json:"org"`
	Name string `json:"name"`
	Role string `json:"role"`
	// Joined is false when the caller was already a member. Not an error: a
	// second click on the same link should land somewhere sensible rather than
	// on a failure page.
	Joined bool `json:"joined"`
}

// handleRedeemInvite adds the caller to the organization an invite names.
//
// Not nested under {org}: the caller is not a member yet, so requireOrg would
// refuse them before the code could be considered. The code IS the
// authorisation, and redeeming it is one statement that both checks and
// consumes a use -- so two callers cannot both spend the last one.
//
// Because every user already has their own organization, this only ever ADDS a
// membership. There is nothing to migrate and no ordering to get wrong: a code
// works the same whether it is redeemed before or after the first `lem`.
func (s *Server) handleRedeemInvite(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok || !p.Resolved() {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}

	iv, err := s.dir.RedeemInvite(r.Context(), r.PathValue("code"))
	if errors.Is(err, directory.ErrNotFound) {
		// Expired, exhausted and never-existed are one answer on purpose.
		// Telling them apart would turn a code into an oracle for which codes
		// exist, and a caller can do nothing different with the distinction.
		http.Error(w, "this invite code does not work", http.StatusNotFound)
		return
	} else if err != nil {
		s.log.Error("redeeming an invite", "error", err)
		http.Error(w, "could not redeem the invite", http.StatusInternalServerError)
		return
	}

	org, err := s.dir.OrgByID(r.Context(), iv.OrgID)
	if err != nil {
		http.Error(w, "could not resolve the organization", http.StatusInternalServerError)
		return
	}
	// The invariant that lets one directory row answer "which database holds
	// this user": every organization they belong to is in the same cell as
	// their home one. Enforced here because redemption is the only way to gain
	// a membership outside it.
	if org.CellID != s.opt.CellID {
		s.log.Warn("refused an invite across cells",
			"org", org.Slug, "org_cell", org.CellID, "cell", s.opt.CellID)
		http.Error(w, "this invite is for an organization in another region", http.StatusConflict)
		return
	}

	joined, err := s.joinOrg(r.Context(), org.ID, p, iv.Role)
	if err != nil {
		s.log.Error("joining an organization", "org", org.Slug, "user", p.UserID, "error", err)
		http.Error(w, "could not join the organization", http.StatusInternalServerError)
		return
	}
	s.log.Info("invite redeemed",
		"org", org.Slug, "user", p.UserID, "role", iv.Role, "joined", joined)

	writeJSON(w, http.StatusOK, redeemResponse{
		Org: org.Slug, Name: org.Name, Role: iv.Role, Joined: joined,
	})
}

// inviteCodeBytes is the entropy behind a code.
//
// 20 bytes, base32 without padding, is 32 characters -- readable enough to
// dictate and far past guessable. A code is a bearer credential for membership
// of an organization, and redemption is deliberately unauthenticated as to
// WHICH organization, so guessability is the whole of its security.
const inviteCodeBytes = 20

func newInviteCode() (string, error) {
	var b [inviteCodeBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	// Lowercase base32: no case to get wrong when someone reads it aloud or
	// retypes it, and no characters a URL would escape.
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])), nil
}
