package controlplane

import "net/http"

// authConfigDoc is what a client needs in order to sign in to THIS control
// plane, so that no user ever types an issuer URL, a client id or an audience.
//
// Those three are properties of the deployment, not of the machine a CLI runs
// on. A client that has to be told them cannot be pointed at two control planes
// without two sets of environment variables in one shell, and nothing catches
// the mismatch: the token gets minted by the wrong identity provider and the
// only symptom is a 401 that names none of it. Phase 3 sharpens the point, since
// each tenant authenticates against its own identity provider and only the
// control plane knows which one.
//
// Nothing here is secret. The CLI is a public OAuth client with no secret at
// all -- its client id travels in every device-flow request, and the console's
// equivalent sits in the browser's address bar throughout sign-in. What
// authenticates a user is the token they come back with, which this endpoint
// cannot mint.
type authConfigDoc struct {
	// Required answers "will an unauthenticated call be rejected". Explicit
	// rather than inferred from an empty issuer, so that a client can tell a
	// deployment with authentication switched off from one that did not answer
	// the question -- the two need opposite behaviour, and guessing between
	// them is how a CLI ends up refusing calls the server would have allowed.
	Required bool   `json:"required"`
	Issuer   string `json:"issuer,omitempty"`
	ClientID string `json:"client_id,omitempty"`
	Audience string `json:"audience,omitempty"`
}

// handleAuthConfig is unauthenticated by necessity rather than by choice: it is
// what a client reads BEFORE it has a token, so demanding one would be circular.
func (s *Server) handleAuthConfig(w http.ResponseWriter, r *http.Request) {
	doc := authConfigDoc{Required: s.auth.Enabled()}
	if doc.Required {
		doc.Issuer = s.opt.AuthIssuer
		doc.Audience = s.opt.AuthAudience
		// May be empty: a deployment that only uses the console has no CLI client
		// to advertise. The client turns that into a sentence naming the flag,
		// which beats making it fatal at boot for an operator who does not want
		// the CLI at all.
		doc.ClientID = s.opt.AuthCLIClientID
	}
	writeJSON(w, http.StatusOK, doc)
}
