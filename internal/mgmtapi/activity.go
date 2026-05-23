package mgmtapi

import (
	"sync"
	"time"

	"github.com/lufzle/lemul/internal/tunnel"
)

// activityStore keeps the latest report a workspace task pushed up its control
// tunnel: resource headroom, plus what each session has been doing.
//
// Modelled on preflightStore, and keyed the same way, for the same reason -- a
// workspace name is unique only within an organization, so keying on the name
// alone would let one organization's report decide another organization's
// sessions.
//
// It holds a receivedAt, and that single field is what both fail-safe rules
// read: admission falls back to a count when a report is stale, and the reaper
// does nothing at all. Reports are in memory rather than in the database
// because they are worthless once old -- persisting them would only make a
// stale one survive the restart that would otherwise have discarded it.
type activityStore struct {
	mu      sync.Mutex
	reports map[wsRef]activityReport
}

type activityReport struct {
	Headroom   tunnel.Headroom
	ReceivedAt time.Time
}

func newActivityStore() *activityStore {
	return &activityStore{reports: make(map[wsRef]activityReport)}
}

func (a *activityStore) put(tenantID string, h tunnel.Headroom, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reports[wsRef{tenantID, h.WorkspaceID}] = activityReport{Headroom: h, ReceivedAt: now}
}

func (a *activityStore) get(k wsRef) (activityReport, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.reports[k]
	return r, ok
}

// forget drops a workspace's report when its task goes away, so a replacement
// is never judged by its predecessor's numbers -- the same reasoning that makes
// preflightStore.forget exist.
func (a *activityStore) forget(k wsRef) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.reports, k)
}

// all returns a snapshot for the sweeper to walk without holding the lock while
// it makes network calls.
func (a *activityStore) all() map[wsRef]activityReport {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[wsRef]activityReport, len(a.reports))
	for k, v := range a.reports {
		out[k] = v
	}
	return out
}

// fresh reports whether a report is recent enough to act on.
//
// Three reporting intervals: one missed tick is a hiccup, three is a supervisor
// that has stopped talking. The consequence of getting this wrong is asymmetric,
// which is why it is generous -- too strict merely delays a stop, while too
// loose reaps a session on the strength of a measurement taken before it began.
func (r activityReport) fresh(now time.Time, interval time.Duration) bool {
	return now.Sub(r.ReceivedAt) <= 3*interval
}
