package cliauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// advertising stands in for a control plane answering GET /v1/auth/config.
func advertising(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth/config" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The whole point of the change: the user supplies an address, and everything
// needed to sign in comes back from it.
func TestDiscoverReadsTheConfigurationFromTheControlPlane(t *testing.T) {
	srv := advertising(t, http.StatusOK, `{
		"required": true,
		"issuer": "https://idp.example/oidc",
		"client_id": "cli-abc",
		"audience": "https://api.lemul.local"
	}`)

	cfg, required, err := Discover(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if !required {
		t.Fatal("required = false")
	}
	if cfg.Issuer != "https://idp.example/oidc" || cfg.ClientID != "cli-abc" ||
		cfg.Resource != "https://api.lemul.local" {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestDiscoverReportsAuthenticationOff(t *testing.T) {
	srv := advertising(t, http.StatusOK, `{"required": false}`)

	_, required, err := Discover(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if required {
		t.Error("required = true against a control plane with authentication off")
	}
}

// The operator's mistake, surfaced to the user who has to ask them to fix it.
func TestDiscoverNamesTheMissingFlag(t *testing.T) {
	srv := advertising(t, http.StatusOK, `{
		"required": true,
		"issuer": "https://idp.example/oidc",
		"audience": "https://api.lemul.local"
	}`)

	_, _, err := Discover(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("no error for a control plane requiring auth with no CLI client id")
	}
	if !strings.Contains(err.Error(), "-auth-cli-client-id") {
		t.Errorf("error does not name the flag: %v", err)
	}
}

// A 404 means the question was not answered, which is NOT the same as "no
// authentication needed". Conflating them would have a client conclude it can
// proceed bare and then fail every call with a 401 explaining none of it.
func TestDiscoverDistinguishesSilenceFromNoAuthRequired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, _, err := Discover(context.Background(), srv.URL)
	if !errors.Is(err, ErrNoDiscovery) {
		t.Fatalf("err = %v, want ErrNoDiscovery", err)
	}
}

func TestEnvOverrideAllOrNothing(t *testing.T) {
	t.Run("none set", func(t *testing.T) {
		clearAuthEnv(t)
		_, ok, err := EnvOverride()
		if err != nil || ok {
			t.Fatalf("ok = %v, err = %v; want false, nil", ok, err)
		}
	})

	t.Run("all set", func(t *testing.T) {
		clearAuthEnv(t)
		t.Setenv(envIssuer, "https://idp.example/oidc")
		t.Setenv(envClientID, "cli-abc")
		t.Setenv(envAudience, "https://api.lemul.local")
		cfg, ok, err := EnvOverride()
		if err != nil || !ok {
			t.Fatalf("ok = %v, err = %v; want true, nil", ok, err)
		}
		if cfg.ClientID != "cli-abc" {
			t.Errorf("config = %+v", cfg)
		}
	})

	// A stale half-set shell is the realistic failure. Merging it with discovery
	// would produce a config that fails at token exchange, several steps later,
	// with an error pointing at neither half.
	t.Run("partial is an error naming what is missing", func(t *testing.T) {
		clearAuthEnv(t)
		t.Setenv(envIssuer, "https://idp.example/oidc")
		_, ok, err := EnvOverride()
		if err == nil {
			t.Fatal("no error for a partial environment")
		}
		if ok {
			t.Error("ok = true for a partial environment")
		}
		if !strings.Contains(err.Error(), envClientID) || !strings.Contains(err.Error(), envAudience) {
			t.Errorf("error does not name both missing variables: %v", err)
		}
	})
}

// The override is the deliberate act -- someone pointing at an identity provider
// the control plane does not know about -- so it wins.
func TestResolvePrefersTheEnvironment(t *testing.T) {
	clearAuthEnv(t)
	srv := advertising(t, http.StatusOK, `{
		"required": true, "issuer": "https://discovered.example/oidc",
		"client_id": "discovered", "audience": "https://api.lemul.local"
	}`)
	t.Setenv(envIssuer, "https://override.example/oidc")
	t.Setenv(envClientID, "override")
	t.Setenv(envAudience, "https://api.lemul.local")

	cfg, required, err := Resolve(context.Background(), srv.URL)
	if err != nil || !required {
		t.Fatalf("required = %v, err = %v", required, err)
	}
	if cfg.ClientID != "override" {
		t.Errorf("discovery beat the environment: %+v", cfg)
	}
}

func TestResolveFallsBackToDiscovery(t *testing.T) {
	clearAuthEnv(t)
	srv := advertising(t, http.StatusOK, `{
		"required": true, "issuer": "https://discovered.example/oidc",
		"client_id": "discovered", "audience": "https://api.lemul.local"
	}`)

	cfg, required, err := Resolve(context.Background(), srv.URL)
	if err != nil || !required {
		t.Fatalf("required = %v, err = %v", required, err)
	}
	if cfg.ClientID != "discovered" {
		t.Errorf("config = %+v", cfg)
	}
}

// An empty variable is still a set variable, so the "none set" case only means
// something if they are genuinely absent. t.Setenv is called first purely to
// register the restore; Unsetenv then does the actual removal. Whoever runs the
// suite may well have these exported -- that is the whole shape being replaced.
func clearAuthEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{envIssuer, envClientID, envAudience} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatalf("unset %s: %v", k, err)
		}
	}
}
