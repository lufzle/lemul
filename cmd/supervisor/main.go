// Command supervisor is the thin entry point for the workspace-task agent.
// The behaviour lives in internal/supervisor.
//
// Usage:
//
//	supervisor -control-plane ws://host:9000 -tenant t1 -workspace w1 -token ...
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/lufzle/lemul-cc/internal/bedrock"
	"github.com/lufzle/lemul-cc/internal/supervisor"
)

func main() {
	var (
		controlPlane = flag.String("control-plane", "ws://localhost:9000", "control plane base URL")
		tenantID     = flag.String("tenant", "t1", "tenant id")
		workspaceID  = flag.String("workspace", "w1", "workspace id")
		token        = flag.String("token", "", "workspace-scoped tunnel credential")
		defaultCmd   = flag.String("cmd", "claude", "default command for a new session (space separated)")
		configDir    = flag.String("config-dir", os.Getenv("CLAUDE_CONFIG_DIR"),
			"Claude Code config dir holding conversations; empty resolves it as Claude Code does")
		termName = flag.String("term", "xterm-256color", "TERM for children")
		ringBytes    = flag.Int("ring", 256<<10, "per-session replay ring size in bytes")
		nudgeDelay   = flag.Duration("nudge-delay", 75*time.Millisecond, "gap between the two resize ioctls on attach")
		headroomEvry = flag.Duration("headroom-interval", 30*time.Second, "resource headroom reporting interval")
		// Defaulted from the environment because that is how a task is configured
		// in both drivers: the local driver passes env to the child, and ECS
		// passes it through the task definition. Flags stay for running the
		// supervisor by hand.
		gatewayURL = flag.String("gateway-url", os.Getenv("LEMUL_GATEWAY_URL"),
			"broker model traffic to this gateway (decision #12; the supported inference path)")
		gatewayKey = flag.String("gateway-key", os.Getenv("LEMUL_GATEWAY_KEY"),
			"gateway credential; held by the supervisor and stripped from every session")
		userID     = flag.String("user", os.Getenv("LEMUL_USER_ID"), "user id for gateway cost attribution")
		bedrockPre = flag.Bool("bedrock-preflight", os.Getenv("LEMUL_BEDROCK_PREFLIGHT") != "",
			"check pinned Bedrock models at task start")
		region  = flag.String("region", os.Getenv("AWS_REGION"), "AWS region for the Bedrock check")
		pinSpec = flag.String("pins", os.Getenv("LEMUL_PINS"),
			"comma-separated role=modelID pins; empty uses the defaults")
	)
	flag.Parse()
	log.SetPrefix("supervisor: ")

	var pins []bedrock.Pin
	if *pinSpec != "" {
		var err error
		if pins, err = bedrock.ParsePins(*pinSpec); err != nil {
			log.Fatal(err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	s := supervisor.New(supervisor.Options{
		ControlPlane:     *controlPlane,
		TenantID:         *tenantID,
		WorkspaceID:      *workspaceID,
		Token:            *token,
		DefaultCmd:       strings.Fields(*defaultCmd),
		ConfigDir:        *configDir,
		Term:             *termName,
		RingBytes:        *ringBytes,
		NudgeDelay:       *nudgeDelay,
		HeadroomInterval: *headroomEvry,
		GatewayURL:       *gatewayURL,
		GatewayKey:       *gatewayKey,
		UserID:           *userID,
		BedrockPreflight: *bedrockPre,
		Region:           *region,
		Pins:             pins,
	})

	if err := s.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}
