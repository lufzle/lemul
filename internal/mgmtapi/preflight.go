package mgmtapi

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/lufzle/lemul/internal/tunnel"
)

// preflightStore keeps the latest Bedrock verdict per workspace, and lets a
// caller wait for one that has not arrived yet.
//
// The wait exists because the check runs asynchronously after a task registers
// its tunnel, so the first session-create of a cold workspace can otherwise race
// it. Waiting a few seconds is far better than admitting a session into a
// workspace we already suspect is broken.
type preflightStore struct {
	mu      sync.Mutex
	reports map[wsRef]tunnel.PreflightReport
	waiters map[wsRef][]chan tunnel.PreflightReport
}

// wsRef identifies a workspace outside the database, where the name is the
// identifier. The organization is part of the key for the same reason it is
// part of the tunnel registry's: `UNIQUE (tenant_id, name)` means a name alone
// is ambiguous, and here the consequence would be one organization's Bedrock
// verdict silently gating another organization's sessions.
type wsRef struct {
	tenantID string
	name     string
}

func newPreflightStore() *preflightStore {
	return &preflightStore{
		reports: make(map[wsRef]tunnel.PreflightReport),
		waiters: make(map[wsRef][]chan tunnel.PreflightReport),
	}
}

func (p *preflightStore) put(tenantID string, rep tunnel.PreflightReport) {
	k := wsRef{tenantID, rep.WorkspaceID}
	p.mu.Lock()
	p.reports[k] = rep
	waiters := p.waiters[k]
	delete(p.waiters, k)
	p.mu.Unlock()

	for _, ch := range waiters {
		ch <- rep
		close(ch)
	}
}

func (p *preflightStore) get(k wsRef) (tunnel.PreflightReport, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rep, ok := p.reports[k]
	return rep, ok
}

// forget drops a workspace's verdict, so a replacement task is not judged by the
// previous one's result. Model access is often granted after a failure, and a
// stale "no" would keep a fixed account looking broken.
func (p *preflightStore) forget(k wsRef) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.reports, k)
}

func (p *preflightStore) wait(ctx context.Context, k wsRef) (tunnel.PreflightReport, bool) {
	p.mu.Lock()
	if rep, ok := p.reports[k]; ok {
		p.mu.Unlock()
		return rep, true
	}
	ch := make(chan tunnel.PreflightReport, 1)
	p.waiters[k] = append(p.waiters[k], ch)
	p.mu.Unlock()

	select {
	case rep := <-ch:
		return rep, true
	case <-ctx.Done():
		p.mu.Lock()
		p.waiters[k] = removeReportChan(p.waiters[k], ch)
		if len(p.waiters[k]) == 0 {
			delete(p.waiters, k)
		}
		p.mu.Unlock()
		return tunnel.PreflightReport{}, false
	}
}

func removeReportChan(cs []chan tunnel.PreflightReport, c chan tunnel.PreflightReport) []chan tunnel.PreflightReport {
	out := cs[:0]
	for _, x := range cs {
		if x != c {
			out = append(out, x)
		}
	}
	return out
}

// preflightWait bounds how long session creation waits for a verdict. The check
// itself is three one-token invocations, so a healthy task answers in seconds.
const preflightWait = 20 * time.Second

// checkPreflight refuses a session when the workspace's Bedrock is known to be
// misconfigured.
//
// The bias is deliberate: block on EVIDENCE of breakage, never on the absence of
// evidence. A report that never arrives -- an older supervisor, a bug in our own
// event path -- must not take the product down, so that case fails open with a
// loud log. A report that says a required model is unusable fails closed, with
// the advice attached, because the alternative is Claude Code starting and then
// dying opaquely inside the PTY on the user's first prompt.
func (s *Server) checkPreflight(ctx context.Context, sc scope, wid string) error {
	cctx, cancel := context.WithTimeout(ctx, preflightWait)
	defer cancel()

	rep, ok := s.preflights.wait(cctx, wsRef{sc.TenantID, wid})
	if !ok {
		s.log.Warn("no bedrock preflight report; admitting the session anyway",
			"workspace", wid, "waited", preflightWait)
		return nil
	}
	if rep.Skipped || !rep.Blocking {
		return nil
	}
	if m, found := rep.FirstBlocker(); found {
		return fmt.Errorf("bedrock is not usable in this workspace: %s (%s) is not invocable. %s",
			m.ModelID, m.Role, m.Advice)
	}
	return fmt.Errorf("bedrock is not usable in this workspace")
}

type preflightResponse struct {
	WorkspaceID string                  `json:"workspace_id"`
	Available   bool                    `json:"available"`
	Report      *tunnel.PreflightReport `json:"report,omitempty"`
}

// handlePreflight exposes the verdict for the admin console. It never triggers a
// check: it reports what the workspace last said, and says plainly when it has
// not said anything.
func (s *Server) handlePreflight(w http.ResponseWriter, r *http.Request) {
	a, ok := s.requireWorkspace(w, r)
	if !ok {
		return
	}
	sc, wid := a.scope, a.WS.Name
	rep, have := s.preflights.get(wsRef{sc.TenantID, wid})
	resp := preflightResponse{WorkspaceID: wid, Available: have}
	if have {
		resp.Report = &rep
	}
	writeJSON(w, http.StatusOK, resp)
}
