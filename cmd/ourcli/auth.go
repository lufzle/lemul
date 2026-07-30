package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/lufzle/lemul-cc/internal/cliauth"
)

// One place that speaks HTTP to the control plane, so the bearer token cannot
// be forgotten on a call site. The attach WebSocket is deliberately not routed
// through here: it carries a single-use credential from endpoint negotiation
// instead, which is what lets it work from a browser too (internal/auth).
func request(method, url string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	token, err := cliauth.AccessToken(context.Background(), cliauth.ConfigFromEnv())
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
	if resp.StatusCode == http.StatusUnauthorized {
		_ = resp.Body.Close()
		if cliauth.ConfigFromEnv().ClientID == "" {
			return nil, fmt.Errorf(
				"the control plane requires authentication, but this client has none configured.\n" +
					"Set LEMUL_AUTH_ISSUER, LEMUL_CLI_CLIENT_ID and LEMUL_AUTH_AUDIENCE " +
					"(auth-stack/seed.ts generates them), then run `ourcli login`")
		}
		return nil, fmt.Errorf("unauthorized: run `ourcli login`")
	}
	return resp, nil
}

// runLogin performs the device authorization grant (RFC 8628).
func runLogin() error {
	cfg := cliauth.ConfigFromEnv()
	if cfg.ClientID == "" {
		return fmt.Errorf(
			"authentication is not configured.\n" +
				"Generate it with: bun auth-stack/seed.ts > auth-stack/.env.generated\n" +
				"then export LEMUL_AUTH_ISSUER, LEMUL_CLI_CLIENT_ID and LEMUL_AUTH_AUDIENCE")
	}

	token, err := cliauth.Authorize(context.Background(), cfg, func(p cliauth.Prompt) {
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
	if err := cliauth.Save(token); err != nil {
		return err
	}
	path, _ := cliauth.TokenPath()
	fmt.Fprintf(os.Stderr, "Signed in. Token cached at %s\n", path)
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

// authStatus is used by `ourcli whoami`.
func authStatus() string {
	cfg := cliauth.ConfigFromEnv()
	if cfg.ClientID == "" {
		return "authentication is not configured (the control plane may not require it)"
	}
	t, err := cliauth.Load()
	if err != nil {
		return "not signed in — run `ourcli login`"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "issuer:  %s\n", cfg.Issuer)
	fmt.Fprintf(&b, "expires: %s", t.ExpiresAt.Local().Format("2006-01-02 15:04:05"))
	if t.RefreshToken != "" {
		b.WriteString("  (auto-refreshes)")
	}
	return b.String()
}
