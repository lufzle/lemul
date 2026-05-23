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
	ID       string
	TenantID string
	// WorkspaceName, not an id. The supervisor is given the NAME as its
	// -workspace argv and announces that, while the schema keys workspaces on a
	// uuid -- one thing with two identifiers, which has cost real debugging
	// time here before. Spelt out in the field name so that assigning a uuid to
	// it looks wrong at the call site. Empty for runner tunnels.
	WorkspaceName string
	RemoteAddr    string
	ConnectedAt   time.Time

	sess *yamux.Session
}

func NewTunnel(id, tenantID, workspaceName, remoteAddr string, sess *yamux.Session) *Tunnel {
	return &Tunnel{
		ID:            id,
		TenantID:      tenantID,
		WorkspaceName: workspaceName,
		RemoteAddr:    remoteAddr,
		ConnectedAt:   time.Now(),
		sess:          sess,
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
// (tenant, workspace name).
//
// The workspace key is a pair rather than a name because a workspace name is
// unique only WITHIN an organization -- `UNIQUE (tenant_id, name)` in the cell
// schema. Keying on the name alone was correct while there was one tenant and
// silently wrong the moment there were two: the second organization's `api`
// workspace would have joined the first one's tunnel set, and PickWorkspace
// returns the newest, so a user would have been handed a terminal inside
// somebody else's sandbox.
type Registry struct {
	mu         sync.Mutex
	runners    map[string][]*Tunnel
	workspaces map[wsKey][]*Tunnel
	waiters    map[wsKey][]chan *Tunnel
	rr         atomic.Uint64
}

// wsKey is what makes a workspace name unambiguous. A struct rather than a
// joined string so that a name containing the separator cannot be made to
// collide with another organization's.
type wsKey struct {
	tenantID string
	name     string
}

func New() *Registry {
	return &Registry{
		runners:    make(map[string][]*Tunnel),
		workspaces: make(map[wsKey][]*Tunnel),
		waiters:    make(map[wsKey][]chan *Tunnel),
	}
}

func (t *Tunnel) key() wsKey { return wsKey{tenantID: t.TenantID, name: t.WorkspaceName} }

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
	k := t.key()
	r.mu.Lock()
	r.workspaces[k] = append(r.workspaces[k], t)
	waiters := r.waiters[k]
	delete(r.waiters, k)
	r.mu.Unlock()

	for _, ch := range waiters {
		ch <- t
		close(ch)
	}
}

// CloseAll closes every tunnel this registry holds.
//
// For shutdown. A service going away should drop its tunnels rather than let
// the peers discover it by keepalive: the interval is 15 s and the agent
// retries every 2 s, so closing turns a quarter-minute of unreachable sessions
// into an immediate reconnect.
//
// It matters most for the relay, which is the service whose restart must be a
// blip rather than an outage -- a workspace task whose data tunnel is still
// "up" to a process that has stopped routing is unreachable while looking fine
// from both ends.
func (r *Registry) CloseAll() {
	r.mu.Lock()
	var all []*Tunnel
	for _, ts := range r.workspaces {
		all = append(all, ts...)
	}
	for _, ts := range r.runners {
		all = append(all, ts...)
	}
	r.mu.Unlock()
	for _, t := range all {
		_ = t.Close()
	}
}

func (r *Registry) RemoveWorkspace(t *Tunnel) {
	k := t.key()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.workspaces[k] = remove(r.workspaces[k], t)
	if len(r.workspaces[k]) == 0 {
		delete(r.workspaces, k)
	}
}

// PickWorkspace returns the tunnel for a workspace task.
//
// A workspace has 0..1 task, so the slice normally holds one element. It is a
// slice anyway because a replacement task can register before the old one's
// tunnel has been noticed as dead -- during vertical migration later, and
// during any relay redeploy today. Taking the newest avoids handing a client to
// a task that is on its way out.
func (r *Registry) PickWorkspace(tenantID, name string) (*Tunnel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ts := r.workspaces[wsKey{tenantID, name}]
	if len(ts) == 0 {
		return nil, ErrNoTunnel
	}
	return ts[len(ts)-1], nil
}

// WaitWorkspace blocks until a workspace tunnel registers, or ctx expires.
// Session creation uses it to cover the Fargate cold start (20-60 s).
func (r *Registry) WaitWorkspace(ctx context.Context, tenantID, name string) (*Tunnel, error) {
	k := wsKey{tenantID, name}
	r.mu.Lock()
	if ts := r.workspaces[k]; len(ts) > 0 {
		t := ts[len(ts)-1]
		r.mu.Unlock()
		return t, nil
	}
	ch := make(chan *Tunnel, 1)
	r.waiters[k] = append(r.waiters[k], ch)
	r.mu.Unlock()

	select {
	case t := <-ch:
		return t, nil
	case <-ctx.Done():
		r.mu.Lock()
		r.waiters[k] = removeChan(r.waiters[k], ch)
		if len(r.waiters[k]) == 0 {
			delete(r.waiters, k)
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
