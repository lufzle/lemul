// Command controlplane is the thin entry point for the control plane.
// The behaviour lives in internal/controlplane.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/lufzle/lemul-cc/internal/controlplane"
	"github.com/lufzle/lemul-cc/internal/store"
)

func main() {
	var (
		addr         = flag.String("addr", ":9000", "listen address")
		agentToken   = flag.String("agent-token", "dev-token", "shared secret agents present at the upgrade")
		publicURL    = flag.String("public-url", "", "ws:// base URL agents dial back to (defaults to ws://localhost<addr>)")
		statePath    = flag.String("state", "", "path to the JSON state file (empty = memory only)")
		tenantID     = flag.String("tenant", "t1", "the single tenant in Phase 1")
		sessionCmd   = flag.String("session-cmd", "claude", "command a new session runs (space separated)")
		startWait    = flag.Duration("start-timeout", 90*time.Second, "how long to wait for a workspace task to dial in")
		image        = flag.String("image", "", "image passed to the driver (ignored by the local driver)")
		gatewayURL   = flag.String("gateway-url", "", "gateway workspace tasks broker model traffic to")
		gatewayKey   = flag.String("gateway-key", "", "gateway credential (never reaches a session)")
		otelEndpoint = flag.String("otel-endpoint", "", "OTLP base URL for workspace telemetry (empty disables it)")
		otelHeaders  = flag.String("otel-headers", "", "OTLP headers, e.g. Authorization=Basic xxx")
		// protobuf, not json: OpenObserve's OTLP JSON parser rejects part of
		// Claude Code's payload with `invalid type: map, expected f64`, and the
		// failure is silent without CLAUDE_CODE_OTEL_DIAG_STDERR.
		otelProtocol = flag.String("otel-protocol", "http/protobuf", "OTLP protocol")
		otelTraces   = flag.Bool("otel-traces", false, "also export Claude Code's beta traces")
		bedrockPre   = flag.Bool("bedrock-preflight", false, "workspace tasks check their pinned Bedrock models at start")
		region       = flag.String("region", "", "AWS region for workspace tasks")
		pins         = flag.String("pins", "", "comma-separated role=modelID pins; empty uses the defaults")
		// Bearer-token validation for the management API. Both default to the
		// environment so the console, ourcli and this process can be pointed at
		// one identity provider from a single generated env file
		// (auth-stack/seed.ts). Empty issuer leaves authentication off.
		authIssuer = flag.String("auth-issuer", os.Getenv("LEMUL_AUTH_ISSUER"),
			"OIDC issuer for management API tokens (empty disables authentication)")
		authAudience = flag.String("auth-audience", os.Getenv("LEMUL_AUTH_AUDIENCE"),
			"API resource indicator tokens must be minted for")
		// Advertised at GET /v1/auth/config so that `ourcli login` can discover
		// how to sign in from the address it was already given, instead of the
		// user exporting deployment facts into their shell.
		authCLIClientID = flag.String("auth-cli-client-id", os.Getenv("LEMUL_CLI_CLIENT_ID"),
			"public device-flow client id advertised to ourcli")
	)
	flag.Parse()
	log.SetPrefix("controlplane: ")

	st, err := store.NewMemory(*statePath)
	if err != nil {
		log.Fatalf("state: %v", err)
	}

	pub := *publicURL
	if pub == "" {
		pub = "ws://localhost" + *addr
	}

	s, err := controlplane.New(controlplane.Options{
		Store:        st,
		AgentToken:   *agentToken,
		TenantID:     *tenantID,
		SessionCmd:   strings.Fields(*sessionCmd),
		StartTimeout: *startWait,
		PublicURL:    pub,
		Image:        *image,

		GatewayURL:       *gatewayURL,
		GatewayKey:       *gatewayKey,
		OTelEndpoint:     *otelEndpoint,
		OTelHeaders:      *otelHeaders,
		OTelProtocol:     *otelProtocol,
		OTelTraces:       *otelTraces,
		BedrockPreflight: *bedrockPre,
		Region:           *region,
		Pins:             *pins,
		AuthIssuer:       *authIssuer,
		AuthAudience:     *authAudience,
		AuthCLIClientID:  *authCLIClientID,
	})
	if err != nil {
		log.Fatalf("control plane: %v", err)
	}
	if *authIssuer != "" {
		log.Printf("management API requires a bearer token from %s (audience %s)", *authIssuer, *authAudience)
		// Said at boot rather than left for the first operator to hit, because
		// the symptom is `ourcli login` failing on a control plane that looks
		// correctly configured from every other angle.
		if *authCLIClientID == "" {
			log.Printf("WARNING: no -auth-cli-client-id, so `ourcli login` cannot discover how to sign in")
		}
	} else {
		log.Printf("management API is UNAUTHENTICATED (no -auth-issuer configured)")
	}

	srv := &http.Server{
		Addr:    *addr,
		Handler: s.Handler(),
		// No write timeout: these are long-lived WebSockets, and yamux
		// keepalive is what detects a dead peer (section 2.2).
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("listening on %s (tenant %s)", *addr, *tenantID)
	log.Fatal(srv.ListenAndServe())
}
