package mgmtapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/lufzle/lemul/internal/auth"
)

// Turning an identity provider's subject into something a person can read.
//
// An access token minted for an API resource carries `sub` and no identity
// claims at all -- no email, no name. That is OAuth working as designed rather
// than a gap: identity belongs to the ID token and to userinfo, and an API is
// meant to know its caller as a stable opaque subject. Verified here: our token
// arrives with an empty scope and Logto's userinfo rejects it outright, so the
// control plane cannot look the caller up even on its own behalf.
//
// The alternatives were both worse. Logto's JWT customizer would have carried
// the email in the token, but custom claims are a Cloud feature and the
// self-hosted build accepts the configuration, stores it, serves it back, and
// silently never runs it. Resolving arbitrary subjects needs Management API
// credentials, and Logto OSS offers exactly one scope on that API -- `all` --
// so a "read-only lookup" credential does not exist; it would be a credential
// that can rewrite the tenant, held by a console with no authorisation model.
//
// So the client relays the ID TOKEN it already holds, and we verify it. That is
// the change from Phase 1, which took a self-asserted label: a label was
// display-only because anyone could send any string, and an email anyone can
// claim is not an identity. A signed assertion from the identity provider is --
// provided its subject is checked against the access token's, because otherwise
// a valid ID token for ANYONE would let a caller write somebody else's address
// onto their own record.
//
// It also names the organization. Sign-up cannot: it happens on a request
// carrying only an access token, so the organization created there is named
// after its own slug until the first ID token arrives.

type identityRequest struct {
	// IDToken is the caller's OIDC ID token, as issued to the console or the
	// CLI. Both are accepted -- see auth.Options.IDTokenAudiences.
	IDToken string `json:"id_token"`
}

type identityResponse struct {
	Email string `json:"email,omitempty"`
	// Org and OrgName report the caller's own organization, so a client that
	// has just signed in learns where it landed without a second call.
	Org     string `json:"org,omitempty"`
	OrgName string `json:"org_name,omitempty"`
}

// handleIdentity records the caller's email from a validated ID token.
//
// PUT rather than POST because it is idempotent: signing in again with the same
// address writes the same row and renames nothing that has already been named.
func (s *Server) handleIdentity(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok || !p.Resolved() {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}

	var req identityRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.IDToken == "" {
		http.Error(w, "id_token is required", http.StatusBadRequest)
		return
	}

	claims, err := s.auth.VerifyIDToken(r.Context(), req.IDToken)
	if err != nil {
		s.log.Warn("rejected an id token", "subject", p.Subject, "error", err)
		http.Error(w, "invalid id token", http.StatusBadRequest)
		return
	}
	// THE WHOLE SECURITY PROPERTY. Without it a caller could present anyone
	// else's ID token -- they are handed to clients, not kept secret from them
	// -- and have that person's email recorded against their own user row,
	// which is the identity every owner column in the console displays.
	if claims.Subject != p.Subject {
		s.log.Warn("id token subject does not match the caller",
			"caller", p.Subject, "token", claims.Subject)
		http.Error(w, "this id token is not yours", http.StatusForbidden)
		return
	}

	email := strings.TrimSpace(claims.Email)
	if email == "" {
		// The provider issued a token with no email claim -- the scope was not
		// granted, or the account has none. Not something the caller can fix by
		// retrying, and not a reason to fail their sign-in.
		s.log.Info("id token carried no email", "subject", p.Subject)
		writeJSON(w, http.StatusOK, identityResponse{})
		return
	}
	if len(email) > 320 { // an email address cannot exceed 320 octets
		http.Error(w, "email is implausibly long", http.StatusBadRequest)
		return
	}

	if err := s.setUserEmail(r.Context(), storeScopeFor(p), p.UserID, email); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Name their own organization after them, once. `dario@sinumo.com` becomes
	// "Dario's Org". NameOrg is a no-op once it has a real name, so a later
	// rename survives every subsequent sign-in.
	org, err := s.dir.OrgByID(r.Context(), p.HomeOrgID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	name := orgNameFor(email)
	if renamed, err := s.dir.NameOrg(r.Context(), org.ID, name); err != nil {
		// Logged, not fatal: the email is recorded either way, and a display
		// name is not worth failing a sign-in over.
		s.log.Warn("naming the organization", "org", org.Slug, "error", err)
	} else if renamed {
		org.Name = name
		s.log.Info("organization named", "org", org.Slug, "name", name)
	}

	writeJSON(w, http.StatusOK, identityResponse{
		Email: email, Org: org.Slug, OrgName: org.Name,
	})
}

// orgNameFor builds the display name of a personal organization from an email.
//
//	dario@sinumo.com       -> Dario's Org
//	dario.farzati@acme.io  -> Dario's Org
//
// Display only, and deliberately not unique -- two people called dario at
// different domains get the same string, which is fine because nothing
// addresses an organization by name. The slug is what has to be unique.
func orgNameFor(email string) string {
	local, _, ok := strings.Cut(email, "@")
	if !ok || local == "" {
		local = email
	}
	// The first component of first.last or first_last, which is what reads as a
	// person's name in "Dario's Org".
	parts := strings.FieldsFunc(local, func(r rune) bool {
		return r == '.' || r == '_' || r == '-' || r == '+'
	})
	if len(parts) == 0 {
		// An address that is nothing but separators. Unlikely, but returning
		// "'s Org" would be worse than saying nothing useful.
		return "My Org"
	}
	first := parts[0]
	return strings.ToUpper(first[:1]) + first[1:] + "'s Org"
}
