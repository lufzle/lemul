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
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/lufzle/lemul/internal/agent"
	"github.com/lufzle/lemul/internal/driver"
	"github.com/lufzle/lemul/internal/tunnel"
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
	slog.Warn("tunnel down", "error", err)
}

func (r *Runner) OnStream(stream net.Conn) {
	defer func() { _ = stream.Close() }()

	env, err := tunnel.ReadMsg(stream)
	if err != nil {
		slog.Error("reading command stream", "error", err)
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
			Relay:        req.Relay,
			Image:        req.Image,
			CPU:          req.CPU,
			MemoryMB:     req.MemoryMB,
			Env:          req.Env,
			SnapshotID:   req.SnapshotID,
		}
		ref, err := r.opt.Driver.Start(ctx, spec)
		if err != nil {
			slog.Error("placing workspace failed",
				"workspace", req.WorkspaceID, "generation", req.Generation, "error", err)
			replyError(stream, err.Error())
			return
		}
		slog.Info("workspace placed",
			"workspace", req.WorkspaceID, "generation", req.Generation,
			"ref", ref, "idempotency_key", spec.IdempotencyKey())
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
		// Dispose of the disk AFTER stopping it. The volume survives the task
		// because the driver asked ECS not to delete it (deleteOnTermination
		// false), which is the only reason there is anything here to snapshot --
		// and the only reason something has to release it.
		//
		// CALLED WHETHER OR NOT WE ARE PRESERVING, and req.Snapshot is an
		// argument rather than a guard. It used to be the guard, so a DELETE --
		// which sets it false precisely to avoid billing for a disk the customer
		// asked us to destroy -- skipped the call and orphaned the volume it was
		// trying not to pay for.
		//
		// Failure is logged and NOT returned. The task is already stopped, so
		// answering with an error would leave the control plane believing a
		// workspace is still running -- and it would then not stop it again,
		// which is worse than an unpreserved disk it can at least be told about.
		var snapshot string
		if snap, ok := r.opt.Driver.(driver.Snapshotter); ok {
			var err error
			if snapshot, err = snap.Preserve(ctx, req.Ref, req.Snapshot); err != nil {
				slog.Error("could not preserve the workspace disk",
					"workspace", req.WorkspaceID, "ref", req.Ref, "error", err)
			}
		}
		slog.Info("workspace stopped",
			"workspace", req.WorkspaceID, "ref", req.Ref, "snapshot", snapshot)
		_ = tunnel.WriteMsg(stream, tunnel.MsgOK, tunnel.Stopped{SnapshotID: snapshot})

	case tunnel.MsgDropSnapshot:
		var req tunnel.DropSnapshot
		if err := env.Decode(&req); err != nil {
			replyError(stream, err.Error())
			return
		}
		snap, ok := r.opt.Driver.(driver.Snapshotter)
		if !ok {
			// Nothing to collect on a driver that never made one. Success, not
			// an error: the control plane records snapshots for whatever driver
			// it is talking to, and a runner swapped for a local one must not
			// turn that bookkeeping into a failure.
			_ = tunnel.WriteMsg(stream, tunnel.MsgOK, nil)
			return
		}
		if err := snap.DropSnapshot(ctx, req.SnapshotID); err != nil {
			replyError(stream, err.Error())
			return
		}
		slog.Info("snapshot dropped", "snapshot", req.SnapshotID)
		_ = tunnel.WriteMsg(stream, tunnel.MsgOK, nil)

	default:
		replyError(stream, "unknown message type "+env.Type)
	}
}

func replyError(stream net.Conn, msg string) {
	_ = tunnel.WriteMsg(stream, tunnel.MsgError, tunnel.Error{Message: msg})
}
