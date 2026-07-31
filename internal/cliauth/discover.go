package cliauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// The environment variables auth-stack/seed.ts generates. They are a development
// override now rather than the normal way in -- see Resolve.
const (
	envIssuer   = "LEMUL_AUTH_ISSUER"
	envClientID = "LEMUL_CLI_CLIENT_ID"
	envAudience = "LEMUL_AUTH_AUDIENCE"
)

// ErrNoDiscovery means the control plane did not answer the question at all,
// which is different from answering "authentication is off". Treating the two
// alike would let a client conclude no sign-in is needed from a server that
// merely predates this endpoint, and then fail every subsequent call with a 401
// that explains none of it.
var ErrNoDiscovery = errors.New("this control plane does not advertise how to sign in")

// Discover asks a control plane how to authenticate to it.
//
// The client already knows one thing legitimately -- the address it was told to
// talk to -- so everything else is derived from that rather than configured.
func Discover(ctx context.Context, serverURL string) (Config, bool, error) {
	endpoint := strings.TrimRight(serverURL, "/") + "/v1/auth/config"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Config{}, false, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Config{}, false, fmt.Errorf("asking %s how to sign in: %w", serverURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return Config{}, false, ErrNoDiscovery
	}
	if resp.StatusCode != http.StatusOK {
		return Config{}, false, fmt.Errorf("asking %s how to sign in: %s", serverURL, resp.Status)
	}

	var doc struct {
		Required bool   `json:"required"`
		Issuer   string `json:"issuer"`
		ClientID string `json:"client_id"`
		Audience string `json:"audience"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return Config{}, false, fmt.Errorf("asking %s how to sign in: %w", serverURL, err)
	}
	if !doc.Required {
		return Config{}, false, nil
	}
	// Required but unusable. Naming the flag matters: this is the operator's
	// mistake, not the user's, and the user is the one reading the message.
	if doc.ClientID == "" {
		return Config{}, true, fmt.Errorf(
			"%s requires authentication but advertises no CLI client id;\n"+
				"whoever runs it needs to pass -auth-cli-client-id", serverURL)
	}
	return Config{Issuer: doc.Issuer, ClientID: doc.ClientID, Resource: doc.Audience}, true, nil
}

// EnvOverride returns the configuration held in the environment, if any.
//
// All three variables or none. A partial set is a mistake worth naming rather
// than quietly merging with what the control plane advertises: the hybrid fails
// at token exchange, several steps later, with an error that points at neither
// half.
func EnvOverride() (Config, bool, error) {
	issuer, clientID, audience := os.Getenv(envIssuer), os.Getenv(envClientID), os.Getenv(envAudience)
	switch {
	case issuer == "" && clientID == "" && audience == "":
		return Config{}, false, nil
	case issuer != "" && clientID != "" && audience != "":
		return Config{Issuer: issuer, ClientID: clientID, Resource: audience}, true, nil
	}
	var missing []string
	for name, v := range map[string]string{envIssuer: issuer, envClientID: clientID, envAudience: audience} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	sortStrings(missing)
	return Config{}, false, fmt.Errorf(
		"%s is set but %s is not.\n"+
			"Set all three or none -- with none, the control plane is asked directly",
		presentNames(issuer, clientID, audience), strings.Join(missing, " and "))
}

// Resolve decides which configuration to sign in with: the environment when it
// holds a complete one, otherwise whatever the control plane advertises.
//
// The environment wins because it is the deliberate act -- someone testing
// against an identity provider the control plane does not know about. It is not
// the normal path, and it is not what a user should ever have to do.
func Resolve(ctx context.Context, serverURL string) (Config, bool, error) {
	cfg, ok, err := EnvOverride()
	if err != nil {
		return Config{}, false, err
	}
	if ok {
		return cfg, true, nil
	}
	return Discover(ctx, serverURL)
}

func presentNames(issuer, clientID, audience string) string {
	var present []string
	for name, v := range map[string]string{envIssuer: issuer, envClientID: clientID, envAudience: audience} {
		if v != "" {
			present = append(present, name)
		}
	}
	sortStrings(present)
	return strings.Join(present, " and ")
}

// A three-element sort, kept local rather than importing sort for one call in a
// message-formatting path. Map iteration order is random, and an error message
// that reorders itself between runs looks like two different errors.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
