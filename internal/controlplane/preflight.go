package controlplane

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/lufzle/lemul-cc/internal/tunnel"
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
	reports map[string]tunnel.PreflightReport
	waiters map[string][]chan tunnel.PreflightReport
}

func newPreflightStore() *preflightStore {
	return &preflightStore{
		reports: make(map[string]tunnel.PreflightReport),
		waiters: make(map[string][]chan tunnel.PreflightReport),
	}
}

func (p *preflightStore) put(rep tunnel.PreflightReport) {
	p.mu.Lock()
	p.reports[rep.WorkspaceID] = rep
	waiters := p.waiters[rep.WorkspaceID]
	delete(p.waiters, rep.WorkspaceID)
	p.mu.Unlock()

	for _, ch := range waiters {
		ch <- rep
		close(ch)
	}
}

func (p *preflightStore) get(wid string) (tunnel.PreflightReport, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rep, ok := p.reports[wid]
	return rep, ok
}

// forget drops a workspace's verdict, so a replacement task is not judged by the
// previous one's result. Model access is often granted after a failure, and a
// stale "no" would keep a fixed account looking broken.
func (p *preflightStore) forget(wid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.reports, wid)
}

func (p *preflightStore) wait(ctx context.Context, wid string) (tunnel.PreflightReport, bool) {
	p.mu.Lock()
	if rep, ok := p.reports[wid]; ok {
		p.mu.Unlock()
		return rep, true
	}
	ch := make(chan tunnel.PreflightReport, 1)
	p.waiters[wid] = append(p.waiters[wid], ch)
	p.mu.Unlock()

	select {
	case rep := <-ch:
		return rep, true
	case <-ctx.Done():
		p.mu.Lock()
		p.waiters[wid] = removeReportChan(p.waiters[wid], ch)
		if len(p.waiters[wid]) == 0 {
			delete(p.waiters, wid)
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
func (s *Server) checkPreflight(ctx context.Context, wid string) error {
	cctx, cancel := context.WithTimeout(ctx, preflightWait)
	defer cancel()

	rep, ok := s.preflights.wait(cctx, wid)
	if !ok {
		log.Printf("workspace %s: no bedrock preflight report after %s; admitting the session anyway",
			wid, preflightWait)
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
	wid := r.PathValue("wid")
	rep, ok := s.preflights.get(wid)
	resp := preflightResponse{WorkspaceID: wid, Available: ok}
	if ok {
		resp.Report = &rep
	}
	writeJSON(w, http.StatusOK, resp)
}
