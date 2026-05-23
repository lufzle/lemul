// Package statekey finds the signing key the two control-plane services share.
//
// The Management API mints workspace, attach and runner credentials with it;
// the relay verifies attach and workspace credentials against it. They share
// exactly this one thing (section 12.9), so "where does the key come from" is a
// property of the pair rather than of either binary -- which is why it lives
// here rather than twice in two package mains, once each.
//
// TWO POLICIES, AND THE ASYMMETRY IS THE POINT:
//
//	LoadOrCreate  the Management API OWNS the key, so finding none means making
//	              one.
//	Load          the relay only ever verifies what the other service signed, so
//	              a key of its own is a key nothing else shares. Every correctly
//	              minted credential would then be rejected, reported as
//	              "unauthorized" -- which reads as a bad token and sends whoever
//	              is debugging it to look at the client. Refusing to start names
//	              the real problem instead.
//
// Both read the same filename from the same directory, which is what makes
// pointing both services at one -state-dir work at all. That agreement used to
// be two constants in two files with nothing comparing them.
package statekey

import (
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/lufzle/lemul/internal/creds"
)

// Filename is what both services look for inside -state-dir.
const Filename = "signing.key"

// Path is where a key lives under a state directory.
func Path(stateDir string) string { return filepath.Join(stateDir, Filename) }

// Load reads the key and REFUSES to invent one. This is the relay's policy.
//
// An empty state directory is an error rather than an ephemeral key, because a
// relay holding a key of its own rejects every credential the Management API
// mints -- and does it with a message pointing at the credential.
func Load(explicit, stateDir string) ([]byte, error) {
	if explicit != "" {
		return checkExplicit(explicit)
	}
	if stateDir == "" {
		return nil, errors.New("the relay needs the control plane's signing key: pass " +
			"-signing-key, set LEMUL_SIGNING_KEY, or point -state-dir at the same " +
			"directory the control plane uses. It cannot generate one, because a key " +
			"nothing else shares rejects every credential it is handed")
	}
	path := Path(stateDir)
	key, err := os.ReadFile(path) //nolint:gosec // the operator names this directory
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("%s does not exist. Start the control plane with the same "+
			"-state-dir first; it owns the key and this service only verifies with it", path)
	case err != nil:
		return nil, err
	}
	return checkFile(path, key)
}

// LoadOrCreate finds the key, making one when there is none. This is the
// Management API's policy.
//
// Three sources, in the order that makes the durable case the default:
//
//	explicit      -signing-key / LEMUL_SIGNING_KEY -- production, from a secret store
//	beside state  <-state-dir>/signing.key, created once -- local development
//	generated     only when there is no state dir either, so nothing was durable anyway
//
// The key must outlive the process. Credentials are DERIVED from it rather than
// stored (internal/creds), which is what lets a restarted control plane
// recognise a workspace task its predecessor placed -- but only if it derives
// them the same way. A key generated afresh at every boot reproduces the exact
// failure that design removes: every restart orphans every live task.
func LoadOrCreate(explicit, stateDir string) ([]byte, error) {
	if explicit != "" {
		return checkExplicit(explicit)
	}

	if stateDir == "" {
		// Memory-only: the store does not survive a restart either, so a fresh
		// key loses nothing that was not already lost. Still said out loud,
		// because "credentials do not survive a restart" is worth knowing before
		// it is diagnosed from a workspace task exiting.
		key, err := generate()
		if err != nil {
			return nil, err
		}
		slog.Warn("no -signing-key and no -state-dir: generated an ephemeral key, " +
			"so a restart will orphan any running workspace task")
		return key, nil
	}

	path := Path(stateDir)
	switch key, err := os.ReadFile(path); { //nolint:gosec // the operator names this directory
	case err == nil:
		return checkFile(path, key)
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}

	key, err := generate()
	if err != nil {
		return nil, err
	}
	// 0700 and 0600: this key mints credentials for every workspace in the
	// deployment.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, err
	}
	slog.Info("created a signing key", "path", path)
	return key, nil
}

// checkExplicit and checkFile refuse a key too short to be worth having.
//
// creds.NewSigner refuses the same length, but only after the process has
// decided this IS its key -- so the failure would name neither the flag nor the
// file, and the two cases have different fixes.
func checkExplicit(v string) ([]byte, error) {
	if len(v) < creds.MinKeyBytes {
		return nil, fmt.Errorf("-signing-key must be at least %d bytes, got %d",
			creds.MinKeyBytes, len(v))
	}
	return []byte(v), nil
}

func checkFile(path string, key []byte) ([]byte, error) {
	if len(key) < creds.MinKeyBytes {
		return nil, fmt.Errorf("%s holds %d bytes, need at least %d",
			path, len(key), creds.MinKeyBytes)
	}
	return key, nil
}

func generate() ([]byte, error) {
	key := make([]byte, creds.MinKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return key, nil
}
