// Package registry tracks the live outbound tunnels the control plane holds.
//
// Tunnels are modelled as a SET per key, never as a single tunnel, for the
// reason given in section 2.8: the data-structure choice is free today and
// expensive to retrofit, and it is what turns `desiredCount: 1 -> 2` into a
// Terraform variable rather than a project. We deploy one runner replica and
// design for N.
//
// The tunnel is also the health check. There are no liveness probes and no
// service discovery: yamux keepalive notices a dead peer, the tunnel leaves the
// set, and dispatch picks a survivor.
package registry

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"
)

var ErrNoTunnel = errors.New("no live tunnel")

// Tunnel is one agent's multiplexed connection.
type Tunnel struct {
	ID          string
	TenantID    string
	WorkspaceID string // empty for runner tunnels
	RemoteAddr  string
	ConnectedAt time.Time

	sess *yamux.Session
}

func NewTunnel(id, tenantID, workspaceID, remoteAddr string, sess *yamux.Session) *Tunnel {
	return &Tunnel{
		ID:          id,
		TenantID:    tenantID,
		WorkspaceID: workspaceID,
		RemoteAddr:  remoteAddr,
		ConnectedAt: time.Now(),
		sess:        sess,
	}
}

// Open starts a new stream to the agent. The control plane always opens;
// the agent opens exactly one stream, its event stream, at registration.
func (t *Tunnel) Open() (net.Conn, error) { return t.sess.Open() }

// Accept receives the agent's event stream.
func (t *Tunnel) Accept() (net.Conn, error) { return t.sess.Accept() }

func (t *Tunnel) Closed() <-chan struct{} { return t.sess.CloseChan() }
func (t *Tunnel) Close() error            { return t.sess.Close() }

// Registry holds runner tunnels keyed by tenant and workspace tunnels keyed by
// workspace.
type Registry struct {
	mu         sync.Mutex
	runners    map[string][]*Tunnel
	workspaces map[string][]*Tunnel
	waiters    map[string][]chan *Tunnel
	rr         atomic.Uint64
}

func New() *Registry {
	return &Registry{
		runners:    make(map[string][]*Tunnel),
		workspaces: make(map[string][]*Tunnel),
		waiters:    make(map[string][]chan *Tunnel),
	}
}

func (r *Registry) AddRunner(t *Tunnel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runners[t.TenantID] = append(r.runners[t.TenantID], t)
}

func (r *Registry) RemoveRunner(t *Tunnel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runners[t.TenantID] = remove(r.runners[t.TenantID], t)
	if len(r.runners[t.TenantID]) == 0 {
		delete(r.runners, t.TenantID)
	}
}

// PickRunner returns any live runner tunnel for the tenant. Round-robin rather
// than "the first one" so that with two replicas both paths get exercised
// instead of one sitting cold until the other dies.
func (r *Registry) PickRunner(tenantID string) (*Tunnel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ts := r.runners[tenantID]
	if len(ts) == 0 {
		return nil, ErrNoTunnel
	}
	i := r.rr.Add(1) % uint64(len(ts))
	return ts[i], nil
}

func (r *Registry) AddWorkspace(t *Tunnel) {
	r.mu.Lock()
	r.workspaces[t.WorkspaceID] = append(r.workspaces[t.WorkspaceID], t)
	waiters := r.waiters[t.WorkspaceID]
	delete(r.waiters, t.WorkspaceID)
	r.mu.Unlock()

	for _, ch := range waiters {
		ch <- t
		close(ch)
	}
}

func (r *Registry) RemoveWorkspace(t *Tunnel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.workspaces[t.WorkspaceID] = remove(r.workspaces[t.WorkspaceID], t)
	if len(r.workspaces[t.WorkspaceID]) == 0 {
		delete(r.workspaces, t.WorkspaceID)
	}
}

// PickWorkspace returns the tunnel for a workspace task.
//
// A workspace has 0..1 task, so the slice normally holds one element. It is a
// slice anyway because a replacement task can register before the old one's
// tunnel has been noticed as dead -- during vertical migration later, and
// during any relay redeploy today. Taking the newest avoids handing a client to
// a task that is on its way out.
func (r *Registry) PickWorkspace(workspaceID string) (*Tunnel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ts := r.workspaces[workspaceID]
	if len(ts) == 0 {
		return nil, ErrNoTunnel
	}
	return ts[len(ts)-1], nil
}

// WaitWorkspace blocks until a workspace tunnel registers, or ctx expires.
// Session creation uses it to cover the Fargate cold start (20-60 s).
func (r *Registry) WaitWorkspace(ctx context.Context, workspaceID string) (*Tunnel, error) {
	r.mu.Lock()
	if ts := r.workspaces[workspaceID]; len(ts) > 0 {
		t := ts[len(ts)-1]
		r.mu.Unlock()
		return t, nil
	}
	ch := make(chan *Tunnel, 1)
	r.waiters[workspaceID] = append(r.waiters[workspaceID], ch)
	r.mu.Unlock()

	select {
	case t := <-ch:
		return t, nil
	case <-ctx.Done():
		r.mu.Lock()
		r.waiters[workspaceID] = removeChan(r.waiters[workspaceID], ch)
		if len(r.waiters[workspaceID]) == 0 {
			delete(r.waiters, workspaceID)
		}
		r.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (r *Registry) RunnerCount(tenantID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.runners[tenantID])
}

func remove(ts []*Tunnel, t *Tunnel) []*Tunnel {
	out := ts[:0]
	for _, x := range ts {
		if x != t {
			out = append(out, x)
		}
	}
	return out
}

func removeChan(cs []chan *Tunnel, c chan *Tunnel) []chan *Tunnel {
	out := cs[:0]
	for _, x := range cs {
		if x != c {
			out = append(out, x)
		}
	}
	return out
}
