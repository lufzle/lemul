// Command controlplane is the thin entry point for the control plane.
// The behaviour lives in internal/controlplane.
package main

import (
	"flag"
	"log"
	"net/http"
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
		otelProtocol = flag.String("otel-protocol", "http/json", "OTLP protocol")
		otelTraces   = flag.Bool("otel-traces", false, "also export Claude Code's beta traces")
		bedrockPre   = flag.Bool("bedrock-preflight", false, "workspace tasks check their pinned Bedrock models at start")
		region       = flag.String("region", "", "AWS region for workspace tasks")
		pins         = flag.String("pins", "", "comma-separated role=modelID pins; empty uses the defaults")
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

	s := controlplane.New(controlplane.Options{
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
	})

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
