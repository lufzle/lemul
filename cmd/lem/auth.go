package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/lufzle/lemul/internal/cliauth"
)

// One place that speaks HTTP to the control plane, so the bearer token cannot
// be forgotten on a call site. The attach WebSocket is deliberately not routed
// through here: it carries a single-use credential from endpoint negotiation
// instead, which is what lets it work from a browser too (internal/auth).
func request(method, url string, body io.Reader) (*http.Response, error) {
	// Every call carries a context, even though nothing cancels this one: a
	// request without one cannot be interrupted at all, so a control plane that
	// accepts the connection and then goes quiet hangs the CLI until the user
	// kills it.
	return requestCtx(context.Background(), method, url, body)
}

// requestCtx is request for a call the CLI intends to ABANDON.
//
// It exists for the workspace event stream, which is a long-lived response body
// deliberately read until something else finishes: the progress a user sees
// during a cold start is a side effect of a request whose useful lifetime is
// decided by another one (see watchWorkspace). Cancelling it is the only way to
// end it, since the server has no reason to.
func requestCtx(ctx context.Context, method, url string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	token, err := cliauth.AccessToken(ctx)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	// Turn the protocol-level answer into the sentence the user needs. A bare
	// "401 Unauthorized" from a CLI leaves them guessing whether the server is
	// broken or they simply have not signed in.
	//
	// One sentence covers every case now: whether this deployment needs a token,
	// and which identity provider mints it, are things `lem login` asks the
	// control plane rather than things the user has to know.
	if resp.StatusCode == http.StatusUnauthorized {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("unauthorized: run `lem login`")
	}
	return resp, nil
}

// runLogin performs the device authorization grant (RFC 8628).
//
// It asks the control plane at -server how to sign in before doing so, so the
// only thing a user supplies is the address they were already going to use.
func runLogin() error {
	ctx := context.Background()
	cfg, required, err := cliauth.Resolve(ctx, *server)
	if err != nil {
		return err
	}
	if !required {
		// Not an error. Signing in to a control plane that wants no token is a
		// no-op, and saying so is more useful than inventing a failure.
		fmt.Fprintf(os.Stderr, "%s does not require authentication — nothing to sign in to.\n", *server)
		return nil
	}

	token, err := cliauth.Authorize(ctx, cfg, func(p cliauth.Prompt) {
		target := p.Complete
		if target == "" {
			target = p.VerificationURI
		}
		// The code goes on its own line and unadorned, because the next thing
		// the user does is read it off the screen and type it somewhere else.
		fmt.Fprintf(os.Stderr, "\nOpen: %s\n", target)
		if p.Complete == "" || p.UserCode != "" {
			fmt.Fprintf(os.Stderr, "Code: %s\n", p.UserCode)
		}
		fmt.Fprintf(os.Stderr, "\nWaiting for approval…\n")
	})
	if err != nil {
		return err
	}
	// Stored with the token: it is what a later refresh needs, and keeping it
	// here is what lets every other command run without asking the control plane
	// anything before it asks for the thing it actually wanted.
	token.Config = cfg
	if err := cliauth.Save(token); err != nil {
		return err
	}
	path, _ := cliauth.TokenPath()
	fmt.Fprintf(os.Stderr, "Signed in. Token cached at %s\n", path)

	// Sign-up happens on the first authenticated request, and that request
	// carries only an access token -- which has no identity claims at all. This
	// is the email arriving, and with it the name of the caller's own
	// organization.
	registerIdentity(token.IDToken)
	return nil
}

func runLogout() error {
	if err := cliauth.Forget(); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Signed out on this machine.")
	// Worth being precise: this drops the local token, it does not revoke the
	// session at the identity provider.
	fmt.Fprintln(os.Stderr, "The identity provider session is untouched.")
	return nil
}

// authStatus is used by `lem whoami`. It reports what this machine holds,
// deliberately without calling the control plane: "am I signed in" should still
// answer when the control plane is unreachable.
func authStatus() string {
	t, err := cliauth.Load()
	if err != nil {
		return "not signed in — run `lem login`"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "issuer:  %s\n", t.Config.Issuer)
	fmt.Fprintf(&b, "expires: %s", t.ExpiresAt.Local().Format("2006-01-02 15:04:05"))
	if t.RefreshToken != "" {
		b.WriteString("  (auto-refreshes)")
	}
	return b.String()
}

// registerIdentity relays the ID token so the control plane learns an email.
//
// Called once, at login. The access token this CLI uses for everything else is
// minted for an API resource and carries a subject and no identity claims --
// which is OAuth working as designed, not a gap -- so the only trustworthy
// source of an address is a signed assertion from the identity provider. The
// control plane checks its subject against the access token's before believing
// it.
//
// Best-effort on purpose. A display name is not worth failing a sign-in over,
// and the interesting failure -- an identity provider whose device flow returns
// no id_token at all -- must leave the user signed in rather than stuck.
func registerIdentity(idToken string) {
	if idToken == "" {
		fmt.Fprintln(os.Stderr,
			"Note: the identity provider returned no ID token, so your email is not recorded. "+
				"Workspaces will show an id rather than an address.")
		return
	}
	body, err := json.Marshal(map[string]string{"id_token": idToken})
	if err != nil {
		return
	}
	u := strings.TrimRight(*server, "/") + "/v1/identity"
	resp, err := request(http.MethodPut, u, bytes.NewReader(body))
	if err != nil {
		return
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		fmt.Fprintf(os.Stderr, "Note: could not record your email: %s\n",
			strings.TrimSpace(string(raw)))
		return
	}
	var out struct {
		Email   string `json:"email"`
		Org     string `json:"org"`
		OrgName string `json:"org_name"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Org == "" {
		return
	}
	fmt.Fprintf(os.Stderr, "You are %s in %s (--org %s).\n", out.Email, out.OrgName, out.Org)
}
