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
	token, err := cliauth.AccessToken(context.Background())
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
	// and which identity provider mints it, are things `ourcli login` asks the
	// control plane rather than things the user has to know.
	if resp.StatusCode == http.StatusUnauthorized {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("unauthorized: run `ourcli login`")
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

// authStatus is used by `ourcli whoami`. It reports what this machine holds,
// deliberately without calling the control plane: "am I signed in" should still
// answer when the control plane is unreachable.
func authStatus() string {
	t, err := cliauth.Load()
	if err != nil {
		return "not signed in — run `ourcli login`"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "issuer:  %s\n", t.Config.Issuer)
	fmt.Fprintf(&b, "expires: %s", t.ExpiresAt.Local().Format("2006-01-02 15:04:05"))
	if t.RefreshToken != "" {
		b.WriteString("  (auto-refreshes)")
	}
	return b.String()
}
