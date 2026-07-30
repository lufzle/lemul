// Command runner is the thin entry point for the tenant's control-plane agent.
// The behaviour lives in internal/runner.
//
// Usage:
//
//	runner -control-plane ws://host:9000 -tenant t1 -driver local
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/lufzle/lemul-cc/internal/driver"
	"github.com/lufzle/lemul-cc/internal/driver/local"
	"github.com/lufzle/lemul-cc/internal/runner"
)

func main() {
	var (
		controlPlane = flag.String("control-plane", "ws://localhost:9000", "control plane base URL")
		tenantID     = flag.String("tenant", "t1", "tenant id")
		runnerID     = flag.String("id", "", "runner id (defaults to hostname)")
		token        = flag.String("token", "dev-token", "tenant runner token")
		driverName   = flag.String("driver", "local", "workspace runtime driver: local|ecs")
		supervisorAt = flag.String("supervisor", "./bin/supervisor", "path to the supervisor binary (local driver)")
	)
	flag.Parse()
	log.SetPrefix("runner: ")

	if *runnerID == "" {
		h, _ := os.Hostname()
		if h == "" {
			h = "runner"
		}
		*runnerID = h
	}

	drv, err := newDriver(*driverName, *supervisorAt)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("driver=%s tenant=%s id=%s", drv.Name(), *tenantID, *runnerID)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	r := runner.New(runner.Options{
		ControlPlane: *controlPlane,
		TenantID:     *tenantID,
		RunnerID:     *runnerID,
		Token:        *token,
		Driver:       drv,
	})
	if err := r.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}

func newDriver(name, supervisorPath string) (driver.Driver, error) {
	switch name {
	case "local":
		return local.New(supervisorPath), nil
	default:
		// The ecs driver lands with the Terraform module; the interface exists
		// from day one so the two paths cannot diverge (sections 8 and 13).
		return nil, fmt.Errorf("unknown driver: %s", name)
	}
}
