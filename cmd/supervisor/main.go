// Command supervisor is the thin entry point for the workspace-task agent.
// The behaviour lives in internal/supervisor.
//
// Usage:
//
//	supervisor -control-plane ws://host:9000 -tenant t1 -workspace w1 -token ...
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/lufzle/lemul/internal/bedrock"
	"github.com/lufzle/lemul/internal/logging"
	"github.com/lufzle/lemul/internal/supervisor"
)

func main() {
	// The one-shot write probe the supervisor forks at the session's uid to find
	// out whether the workspace is really writable (internal/supervisor
	// sessionuid.go). Handled before flag parsing because it is not a mode of the
	// supervisor so much as a different, tiny program that happens to share the
	// binary -- which is what lets it run without a shell in the image.
	if len(os.Args) == 3 && os.Args[1] == "-writable-check" {
		if err := supervisor.RunWritableCheck(os.Args[2]); err != nil {
			// Plain stderr, not the logger: this branch is a different tiny
			// program sharing the binary, its output is read by the parent
			// supervisor, and structured framing would only be in the way.
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	var (
		controlPlane = flag.String("control-plane", "ws://localhost:9000", "Management API base URL (control tunnel)")
		relay        = flag.String("relay", "", "session relay base URL (data tunnel); empty reuses -control-plane")
		generation   = flag.Uint64("generation", 0,
			"this task's workspace generation, announced on the data tunnel")
		tenantID    = flag.String("tenant", "t1", "tenant id")
		workspaceID = flag.String("workspace", "w1", "workspace id")
		token       = flag.String("token", "", "workspace-scoped tunnel credential")
		configDir   = flag.String("config-dir", os.Getenv("CLAUDE_CONFIG_DIR"),
			"Claude Code config dir holding conversations; empty resolves it as Claude Code does")
		termName   = flag.String("term", "xterm-256color", "TERM for children")
		ringBytes  = flag.Int("ring", 256<<10, "per-session replay ring size in bytes")
		nudgeDelay = flag.Duration("nudge-delay", 75*time.Millisecond, "gap between the two resize ioctls on attach")
		// Defaulted from the environment for the reason the gateway flags below
		// are: the control plane passes it at placement, because IT is what
		// measures a report's staleness and the two have to agree about the
		// cadence being measured (mgmtapi.Options.HeadroomInterval).
		headroomEvry = flag.Duration("headroom-interval", envDuration("LEMUL_HEADROOM_INTERVAL", 30*time.Second),
			"resource headroom and session-activity reporting interval")
		// The explorer's boundary, not a convenience: this process runs as root,
		// so nothing under it enforces containment (internal/supervisor/fsjail.go).
		// Same variable image/entrypoint.sh uses, so the container and a
		// hand-run supervisor agree on what "the workspace" means.
		workspaceRoot = flag.String("workspace-root", os.Getenv("LEMUL_PROJECT_DIR"),
			"directory the workspace explorer may list; empty uses /workspace")
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
		// The uid boundary that keeps -gateway-key out of a session's reach: the
		// supervisor's environ is mode 0400 owned by root, so a session at a
		// different uid cannot read it (internal/supervisor/sessionuid.go). Zero
		// disables it, and it is ignored when this process is not root -- which
		// is what keeps the local driver and the e2e suite working unchanged.
		sessionUID = flag.Uint("session-uid", envUint("LEMUL_SESSION_UID"),
			"run sessions at this uid instead of the supervisor's; 0 disables the boundary")
		sessionGID = flag.Uint("session-gid", envUint("LEMUL_SESSION_GID"),
			"gid for sessions; 0 uses -session-uid")
	)
	// What a session runs, seeded from the environment and overridable by hand.
	//
	// This process is the ONE place that knows it. Both create paths -- an
	// attach with Create, and the resume verb -- fork whatever is here, because
	// neither the relay nor the Management API names a command at fork time any
	// more (see tunnel.Attach for why the relay must not).
	//
	// The environment carries a JSON array and the flag takes a space-separated
	// string, and the difference is not an inconsistency. A placed task is
	// configured by mgmtapi.workspaceEnv, where an argv element may contain
	// spaces; a person typing -cmd has a shell doing the splitting for them and
	// wants the short form.
	sessionCmd := argvFromJSON(os.Getenv("LEMUL_SESSION_CMD"), []string{"claude"})
	flag.Var(&sessionCmd, "cmd", "default command for a new session (space separated)")

	flag.Parse()
	logging.Setup("supervisor")

	var pins []bedrock.Pin
	if *pinSpec != "" {
		var err error
		if pins, err = bedrock.ParsePins(*pinSpec); err != nil {
			logging.Fatal("parsing model pins", "pins", *pinSpec, "error", err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	s := supervisor.New(supervisor.Options{
		ControlPlane:     *controlPlane,
		Relay:            *relay,
		Generation:       *generation,
		TenantID:         *tenantID,
		WorkspaceID:      *workspaceID,
		Token:            *token,
		DefaultCmd:       sessionCmd,
		ConfigDir:        *configDir,
		Term:             *termName,
		RingBytes:        *ringBytes,
		NudgeDelay:       *nudgeDelay,
		HeadroomInterval: *headroomEvry,
		GatewayURL:       *gatewayURL,
		GatewayKey:       *gatewayKey,
		UserID:           *userID,
		SessionUID:       uint32(*sessionUID),
		SessionGID:       uint32(*sessionGID),
		BedrockPreflight: *bedrockPre,
		Region:           *region,
		Pins:             pins,
		WorkspaceRoot:    *workspaceRoot,
	})

	if err := s.Run(ctx); err != nil && ctx.Err() == nil {
		logging.Fatal("supervisor stopped", "error", err)
	}
}

// argv is a command line that can be seeded from a JSON array and overridden
// with a space-separated flag. It is a flag.Value rather than a flag default so
// that passing -cmd REPLACES the environment instead of being ignored by it,
// which is how every other environment-defaulted flag here behaves.
type argv []string

func (a *argv) String() string { return strings.Join(*a, " ") }

func (a *argv) Set(v string) error {
	*a = strings.Fields(v)
	return nil
}

// argvFromJSON decodes the environment's form, falling back rather than failing:
// a malformed value should not stop a workspace starting, and the fallback is
// what the deployment would have run anyway.
func argvFromJSON(v string, def []string) argv {
	if v == "" {
		return def
	}
	var out []string
	if err := json.Unmarshal([]byte(v), &out); err != nil || len(out) == 0 {
		return def
	}
	return out
}

// envUint reads a flag default from the environment, which is how a task is
// configured under both drivers. An unparseable value yields 0 -- the safe
// direction for every other setting, but not for this one, so the supervisor
// warns at startup when it is root, holding a credential, and unbounded.
func envUint(name string) uint {
	v, err := strconv.ParseUint(os.Getenv(name), 10, 32)
	if err != nil {
		return 0
	}
	return uint(v)
}

// envDuration reads a flag default from the environment, which is how a task is
// configured in both drivers. An unparseable value falls back rather than
// failing: a bad duration should not stop a workspace starting, and the default
// is safe.
func envDuration(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return def
	}
	return d
}
