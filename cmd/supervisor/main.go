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
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/lufzle/lemul-cc/internal/supervisor"
)

func main() {
	var (
		controlPlane = flag.String("control-plane", "ws://localhost:9000", "control plane base URL")
		tenantID     = flag.String("tenant", "t1", "tenant id")
		workspaceID  = flag.String("workspace", "w1", "workspace id")
		token        = flag.String("token", "", "workspace-scoped tunnel credential")
		defaultCmd   = flag.String("cmd", "claude", "default command for a new session (space separated)")
		termName     = flag.String("term", "xterm-256color", "TERM for children")
		ringBytes    = flag.Int("ring", 256<<10, "per-session replay ring size in bytes")
		nudgeDelay   = flag.Duration("nudge-delay", 75*time.Millisecond, "gap between the two resize ioctls on attach")
		headroomEvry = flag.Duration("headroom-interval", 30*time.Second, "resource headroom reporting interval")
	)
	flag.Parse()
	log.SetPrefix("supervisor: ")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	s := supervisor.New(supervisor.Options{
		ControlPlane:     *controlPlane,
		TenantID:         *tenantID,
		WorkspaceID:      *workspaceID,
		Token:            *token,
		DefaultCmd:       strings.Fields(*defaultCmd),
		Term:             *termName,
		RingBytes:        *ringBytes,
		NudgeDelay:       *nudgeDelay,
		HeadroomInterval: *headroomEvry,
	})

	if err := s.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}
