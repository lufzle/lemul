// Package cliauth signs ourcli in with the OAuth 2.0 device authorization
// grant (RFC 8628).
//
// Device flow rather than a shared secret, because a static CLI token would be
// one credential for every operator: no per-user attribution, and rotating it
// means finding everyone who has it. Device flow gives each person their own
// token from the same identity provider the console uses, and the control plane
// validates both the same way (internal/auth).
//
// It is also the grant that fits a terminal. There is no redirect URI to listen
// on and no browser to own -- the CLI shows a code, the human opens a browser
// wherever they like, and the CLI polls. That works over SSH, which a loopback
// callback does not.
package cliauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Config identifies the client and what it is asking for.
type Config struct {
	Issuer   string // e.g. http://localhost:3001/oidc
	ClientID string
	Resource string // the API indicator the token must be minted for
}

func (c Config) enabled() bool { return c.Issuer != "" && c.ClientID != "" }

type deviceAuth struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int    `json:"expires_in"`
	TokenType        string `json:"token_type"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// Prompt is what the user has to act on, surfaced so the caller owns the
// presentation rather than this package printing to stdout.
type Prompt struct {
	UserCode        string
	VerificationURI string
	Complete        string
}

var (
	ErrDenied  = errors.New("authorization denied")
	ErrExpired = errors.New("the code expired before it was approved")
)

func (c Config) endpoint(path string) string {
	return strings.TrimRight(c.Issuer, "/") + path
}

// Authorize starts the flow and returns what to show the user plus a function
// that blocks until they finish.
func Authorize(ctx context.Context, cfg Config, show func(Prompt)) (*Token, error) {
	if !cfg.enabled() {
		return nil, errors.New("cliauth: issuer and client id are required")
	}

	form := url.Values{
		"client_id": {cfg.ClientID},
		// offline_access is what yields a refresh token. Without it the CLI
		// would ask the operator to sign in again every hour, which is the
		// fastest way to make people paste a static token instead.
		"scope": {"openid offline_access email"},
	}
	if cfg.Resource != "" {
		form.Set("resource", cfg.Resource)
	}

	var da deviceAuth
	if err := postForm(ctx, cfg.endpoint("/device/auth"), form, &da); err != nil {
		return nil, fmt.Errorf("device authorization: %w", err)
	}

	show(Prompt{
		UserCode:        da.UserCode,
		VerificationURI: da.VerificationURI,
		Complete:        da.VerificationURIComplete,
	})

	// The server's interval is a floor, not a suggestion: polling faster earns
	// slow_down, which only makes the wait longer.
	interval := time.Duration(max(da.Interval, 1)) * time.Second
	deadline := time.Now().Add(time.Duration(max(da.ExpiresIn, 300)) * time.Second)

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
		if time.Now().After(deadline) {
			return nil, ErrExpired
		}

		poll := url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"device_code": {da.DeviceCode},
			"client_id":   {cfg.ClientID},
		}
		// The resource must be repeated here, not only on the authorization
		// request. Without it Logto issues an OPAQUE token instead of a JWT --
		// which looks like a successful sign-in right up to the moment the API
		// rejects it, with nothing in either message connecting the two.
		if cfg.Resource != "" {
			poll.Set("resource", cfg.Resource)
		}
		var tr tokenResponse
		err := postForm(ctx, cfg.endpoint("/token"), poll, &tr)
		// A pending authorization is reported as an HTTP error with an OAuth
		// error code, so the body matters more than the status here.
		switch tr.Error {
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5 * time.Second
			continue
		case "access_denied":
			return nil, ErrDenied
		case "expired_token":
			return nil, ErrExpired
		}
		if err != nil {
			return nil, err
		}
		if tr.AccessToken == "" {
			return nil, errors.New("token endpoint returned no access token")
		}
		return newToken(tr), nil
	}
}

// Refresh exchanges a refresh token for a new access token.
func Refresh(ctx context.Context, cfg Config, refreshToken string) (*Token, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {cfg.ClientID},
	}
	if cfg.Resource != "" {
		form.Set("resource", cfg.Resource)
	}
	var tr tokenResponse
	if err := postForm(ctx, cfg.endpoint("/token"), form, &tr); err != nil {
		return nil, err
	}
	if tr.AccessToken == "" {
		return nil, errors.New("refresh returned no access token")
	}
	t := newToken(tr)
	if t.RefreshToken == "" {
		// Not every server rotates refresh tokens; keep the existing one rather
		// than losing the ability to refresh again.
		t.RefreshToken = refreshToken
	}
	return t, nil
}

func newToken(tr tokenResponse) *Token {
	return &Token{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		// A minute of slack: a token that expires between the check and the
		// request arriving is a 401 the user did nothing to deserve.
		ExpiresAt: time.Now().Add(time.Duration(tr.ExpiresIn)*time.Second - time.Minute),
	}
}

func postForm(ctx context.Context, endpoint string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	// Decoded even on an error status: OAuth reports authorization_pending and
	// slow_down as 400s with a meaningful body, and treating those as transport
	// failures would break the poll loop.
	_ = json.Unmarshal(body, out)
	if resp.StatusCode >= 400 {
		var e struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &e)
		if e.Error != "" {
			return fmt.Errorf("%s: %s", e.Error, e.ErrorDescription)
		}
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}
