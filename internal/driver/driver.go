// Package driver abstracts where a workspace task actually runs.
//
// The interface existed from day one, deliberately (section 8, and the first
// risk in section 13). Three implementations now sit behind it:
//
//	ecs    -- Fargate in the customer's VPC. The product.
//	docker -- the real sandbox image on our own machine. Same entrypoint,
//	          managed settings and supervisor binary as ecs; only the placement
//	          substrate differs, which is what makes it a rehearsal rather than
//	          an approximation.
//	local  -- a bare process. Development, demos, and the e2e suite.
//
// The reason for the interface is not portability, it is observability. Once
// the data plane lives in customer accounts we cannot attach a debugger to a
// wedged sandbox, so our debugging environment has to share a code path with
// the product. Retrofitting the interface later does not achieve that, because
// by then the two paths have already diverged.
package driver

import "context"

// Spec is everything needed to place one workspace task.
type Spec struct {
	TenantID    string
	WorkspaceID string

	// Generation makes placement idempotent. The ECS driver turns
	// (WorkspaceID, Generation) into a RunTask --client-token, so a dispatch
	// retried after a timeout cannot produce two tasks for one workspace --
	// which would mean two filesystems and a split-brain snapshot (2.8).
	Generation uint64

	// Credential is workspace-scoped and short-TTL. The supervisor presents it
	// when it dials out; the relay binds it to the task identity on first
	// connect.
	Credential string

	// ControlPlane is the ws:// URL of the Management API, for the control
	// tunnel.
	ControlPlane string
	// Relay is the ws:// URL of the session relay, for the data tunnel. Empty
	// means the supervisor uses ControlPlane for both.
	Relay string

	Image    string
	CPU      int
	MemoryMB int
	Env      map[string]string

	// SnapshotID is the workspace's disk, carried across from its previous
	// task. Empty means a fresh volume, which is what a workspace that has
	// never been stopped with anything on it gets.
	//
	// It is a SPEC field rather than driver configuration because it changes
	// per placement: ECS gives a task either a new volume or one created from a
	// snapshot, and never an existing volume, so restoring is something each
	// RunTask says rather than something the driver knows.
	SnapshotID string
}

// IdempotencyKey is the token derived from the workspace and its generation.
// ECS caps client-token at 64 characters (2.8).
func (s Spec) IdempotencyKey() string {
	k := s.WorkspaceID + "-" + itoa(s.Generation)
	if len(k) > 64 {
		k = k[len(k)-64:]
	}
	return k
}

func itoa(u uint64) string {
	if u == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for u > 0 {
		i--
		b[i] = byte('0' + u%10)
		u /= 10
	}
	return string(b[i:])
}

// Driver places and removes workspace tasks.
//
// Start must be idempotent on Spec.IdempotencyKey: calling it twice with the
// same key returns the same reference rather than placing a second task.
type Driver interface {
	Name() string
	Start(ctx context.Context, s Spec) (ref string, err error)
	Stop(ctx context.Context, ref string) error
}

// Snapshotter is a driver whose workspaces have a disk that does not survive
// their task, and which can therefore preserve one.
//
// SEPARATE FROM Driver on purpose. Only the ecs driver needs it: `local` has no
// volume at all, and `docker` uses a named volume that simply persists, so
// neither has anything a snapshot would mean. Folding these into Driver would
// give both of them two methods whose only honest implementation returns an
// error the control plane cannot act on -- and the control plane would then
// have to distinguish "this driver does not snapshot" from "the snapshot
// failed", which are opposite instructions about whether it is safe to stop a
// task.
//
// So the rule the Management API follows is: a driver that does not implement
// this has nothing to lose by stopping. That is true of both drivers that do
// not, and it is checked where it is assumed rather than assumed globally.
type Snapshotter interface {
	// Preserve disposes of the workspace disk attached to ref, after Stop.
	// With keep true it first snapshots it and returns an identifier a later
	// Start can restore from via Spec.SnapshotID; with keep false it returns
	// the empty string. EITHER WAY IT RELEASES THE DISK.
	//
	// The two halves are one call because they are one decision about one
	// volume, and splitting them is what went wrong. This was `Snapshot`, and
	// releasing lived inside it -- so DELETE, which passes keep=false precisely
	// to avoid billing for a disk the customer asked us to destroy, skipped the
	// call entirely and orphaned the volume it was trying not to pay for. The
	// same for a task that failed to start. Measured on Fargate 2026-08-03:
	// four tasks, four orphaned 30 GiB volumes, DeleteVolume called twice.
	//
	// "Do not preserve" must never be able to mean "do not release", and with
	// one method carrying both there is no call site that can express it.
	//
	// It must be safe to call on a task that is already gone, because that is a
	// race the auto-stop cascade genuinely reaches: it returns the empty string
	// and no error, meaning "there was nothing to preserve". A driver that
	// errored there would turn a benign race into a workspace stuck refusing to
	// stop.
	Preserve(ctx context.Context, ref string, keep bool) (snapshotID string, err error)

	// DropSnapshot deletes one, and is how the previous snapshot is collected
	// after a new one has been recorded. Deleting one that is already gone is
	// success, for the reason Stop treats a missing task as success.
	DropSnapshot(ctx context.Context, snapshotID string) error
}
