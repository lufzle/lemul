// Package runner is the tenant's agent in their own VPC.
//
// After decision #6 it is control-only: it places and stops workspace tasks and
// never carries session bytes. That is the whole point -- the runner can crash,
// redeploy or auto-update and every running session is untouched, because each
// workspace task holds its own tunnel (sections 2.2, 2.8).
//
// Its IAM ask is correspondingly small: ecs:RunTask and ecs:StopTask on ONE
// task definition, and no Bedrock permission at all (section 2.1).
package runner

import (
	"context"
	"log"
	"net"
	"strings"
	"time"

	"github.com/lufzle/lemul-cc/internal/agent"
	"github.com/lufzle/lemul-cc/internal/driver"
	"github.com/lufzle/lemul-cc/internal/tunnel"
)

type Options struct {
	ControlPlane string
	TenantID     string
	RunnerID     string
	Token        string
	Driver       driver.Driver
}

// Runner implements agent.Handler.
type Runner struct {
	opt Options
}

func New(o Options) *Runner { return &Runner{opt: o} }

// Run dials out and serves until ctx is cancelled.
func (r *Runner) Run(ctx context.Context) error {
	return agent.Run(ctx, agent.Config{
		URL:   strings.TrimRight(r.opt.ControlPlane, "/") + "/v1/tunnel/runner",
		Token: r.opt.Token,
		Params: map[string]string{
			"tenant":    r.opt.TenantID,
			"runner_id": r.opt.RunnerID,
		},
	}, r)
}

// OnConnect has nothing to announce yet. The event stream stays open because
// the control plane will use it to assign the reconciler role once there is
// more than one replica (section 2.8) -- no leader election in the customer's
// account, since the coordinator already exists.
func (r *Runner) OnConnect(events net.Conn) error { return nil }

func (r *Runner) OnDisconnect(err error) {
	// Nothing to clean up: the runner is a stateless command executor, and the
	// workspace tasks it placed hold their own tunnels.
	log.Printf("tunnel down: %v", err)
}

func (r *Runner) OnStream(stream net.Conn) {
	defer func() { _ = stream.Close() }()

	env, err := tunnel.ReadMsg(stream)
	if err != nil {
		log.Printf("stream: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	switch env.Type {
	case tunnel.MsgStartWorkspace:
		var req tunnel.StartWorkspace
		if err := env.Decode(&req); err != nil {
			replyError(stream, err.Error())
			return
		}
		spec := driver.Spec{
			TenantID:     req.TenantID,
			WorkspaceID:  req.WorkspaceID,
			Generation:   req.Generation,
			Credential:   req.Credential,
			ControlPlane: req.ControlPlane,
			Image:        req.Image,
			CPU:          req.CPU,
			MemoryMB:     req.MemoryMB,
			Env:          req.Env,
		}
		ref, err := r.opt.Driver.Start(ctx, spec)
		if err != nil {
			log.Printf("start workspace %s: %v", req.WorkspaceID, err)
			replyError(stream, err.Error())
			return
		}
		log.Printf("workspace %s gen %d placed: ref=%s (idempotency key %s)",
			req.WorkspaceID, req.Generation, ref, spec.IdempotencyKey())
		_ = tunnel.WriteMsg(stream, tunnel.MsgOK, tunnel.Ref{Ref: ref})

	case tunnel.MsgStopWorkspace:
		var req tunnel.StopWorkspace
		if err := env.Decode(&req); err != nil {
			replyError(stream, err.Error())
			return
		}
		if err := r.opt.Driver.Stop(ctx, req.Ref); err != nil {
			replyError(stream, err.Error())
			return
		}
		log.Printf("workspace %s stopped: ref=%s", req.WorkspaceID, req.Ref)
		_ = tunnel.WriteMsg(stream, tunnel.MsgOK, nil)

	default:
		replyError(stream, "unknown message type "+env.Type)
	}
}

func replyError(stream net.Conn, msg string) {
	_ = tunnel.WriteMsg(stream, tunnel.MsgError, tunnel.Error{Message: msg})
}
