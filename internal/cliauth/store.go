package cliauth

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// Token is what ourcli caches between invocations.
type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func (t *Token) valid() bool {
	return t != nil && t.AccessToken != "" && time.Now().Before(t.ExpiresAt)
}

var ErrNotSignedIn = errors.New("not signed in: run `ourcli login`")

// TokenPath is where the cached token lives. Under the user's config dir rather
// than the working directory, so it is not picked up by a stray `git add` and
// not shared between users on one machine.
func TokenPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "lemul", "token.json"), nil
}

func Load() (*Token, error) {
	path, err := TokenPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotSignedIn
	}
	if err != nil {
		return nil, err
	}
	var t Token
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, ErrNotSignedIn
	}
	return &t, nil
}

// Save writes the token 0600. It is a bearer credential: anything that can read
// the file can act as the user until it expires.
func Save(t *Token) error {
	path, err := TokenPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func Forget() error {
	path, err := TokenPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// AccessToken returns a usable token, refreshing it if it has expired.
//
// Returns "" with no error when authentication is not configured, which is what
// keeps ourcli working against a control plane that has none -- the same opt-in
// posture as the server side.
func AccessToken(ctx context.Context, cfg Config) (string, error) {
	if !cfg.enabled() {
		return "", nil
	}
	t, err := Load()
	if err != nil {
		return "", err
	}
	if t.valid() {
		return t.AccessToken, nil
	}
	if t.RefreshToken == "" {
		return "", ErrNotSignedIn
	}
	refreshed, err := Refresh(ctx, cfg, t.RefreshToken)
	if err != nil {
		// A refresh token that no longer works means the session is over --
		// revoked, expired, or the tenant was reset. Say so in the words that
		// tell the user what to do.
		return "", ErrNotSignedIn
	}
	if err := Save(refreshed); err != nil {
		return "", err
	}
	return refreshed.AccessToken, nil
}

// ConfigFromEnv reads the same variables auth-stack/seed.ts generates.
func ConfigFromEnv() Config {
	return Config{
		Issuer:   os.Getenv("LEMUL_AUTH_ISSUER"),
		ClientID: os.Getenv("LEMUL_CLI_CLIENT_ID"),
		Resource: os.Getenv("LEMUL_AUTH_AUDIENCE"),
	}
}
