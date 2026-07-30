// Package driver abstracts where a workspace task actually runs.
//
// Two implementations exist from day one, deliberately (section 8, and the
// first risk in section 13):
//
//	ecs    -- Fargate in the customer's VPC. The product.
//	local  -- our own machine. Development, demos, and reproducing failures.
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

	// ControlPlane is the ws:// URL the supervisor dials out to.
	ControlPlane string

	Image    string
	CPU      int
	MemoryMB int
	Env      map[string]string
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
