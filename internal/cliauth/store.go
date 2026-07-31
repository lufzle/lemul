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
	// Config is the deployment this token was minted for, stored with it so that
	// a refresh needs neither environment variables nor a second round trip to
	// the control plane. Discovery then happens exactly once, at login, instead
	// of on every command.
	//
	// A token file written before this field existed simply cannot refresh: its
	// ClientID is empty, AccessToken reports "not signed in", and one
	// `ourcli login` fixes it.
	Config Config `json:"config"`
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

// AccessToken returns a usable token from this machine's cache, refreshing it
// if it has expired.
//
// Having no token at all is NOT an error here: it returns "" and lets the
// request go out bare. A control plane with authentication off accepts it, and
// one that requires it answers 401, which the caller turns into `run ourcli
// login`. Deciding locally instead would mean guessing at the server's posture
// -- and guessing wrong means the CLI refusing calls the server would have
// allowed.
func AccessToken(ctx context.Context) (string, error) {
	t, err := Load()
	if errors.Is(err, ErrNotSignedIn) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if t.valid() {
		return t.AccessToken, nil
	}
	// Expired, and beyond saving: no refresh token, or a token file written
	// before the configuration was stored alongside it. Same answer as having no
	// token -- go out bare and let the control plane rule on it. Failing here
	// instead would break a CLI whose control plane wants no token at all,
	// purely because of a stale file it was never going to send.
	if t.RefreshToken == "" || !t.Config.enabled() {
		return "", nil
	}
	refreshed, err := Refresh(ctx, t.Config, t.RefreshToken)
	if err != nil {
		// A refresh token that no longer works means the session is over --
		// revoked, expired, or the tenant was reset. Still not a local verdict:
		// the 401 that follows says `run ourcli login`, which is both the right
		// advice and the server's own answer rather than our guess at it.
		return "", nil
	}
	refreshed.Config = t.Config
	if err := Save(refreshed); err != nil {
		return "", err
	}
	return refreshed.AccessToken, nil
}
