package mgmtapi

import (
	"context"
	"time"
)

// Placement as a background operation (section 2.4, increment 4).
//
// Until now the only thing that placed a task was a caller waiting for one:
// handleCreateSession blocked inside ensureWorkspace until the tunnel came up,
// which is 20-60 s of a Fargate cold start with nothing on the wire. Creating a
// workspace placed nothing at all, so the first person to want a session paid
// for the machine to boot.
//
// Placement now happens on CREATE, and nobody waits for it. That moves the
// whole cost of a cold start to the moment somebody asked for a machine, which
// is the moment they expect to wait -- but it also removes the request that
// used to carry a failure back. So the record grows two things a caller can
// read afterwards: `status`, which it already had, and `last_error`, which
// carries a failed placement instead of losing it to a log line in a service
// the customer cannot see.
//
// ORDERING IS THE SAFETY ARGUMENT. This is deliberately after increment 3:
// place-on-create with nothing to stop an idle workspace means a workspace
// created on a Friday afternoon bills until Monday. The auto-stop cascade is
// what makes an unattended placement bounded, and it exists now (reaper.go).

// placementTimeout bounds one background attempt.
//
// Derived from StartTimeout rather than a constant of its own: it is the same
// question -- how long may a task take to dial in -- and two numbers for one
// question drift, with the symptom being a workspace recorded as failed by one
// deadline while the other is still waiting for it.
func (s *Server) placementTimeout() time.Duration { return s.opt.StartTimeout }

// startPlacement marks a workspace as starting and places its task without a
// caller waiting for the result.
//
// The status write is SYNCHRONOUS, and that is what lets the handler answer 202
// with a record that is already true. Doing it inside the goroutine would leave
// a window where create said `starting` and the row still said `stopped` -- and
// that row is what the console polls and what `lem` reads a moment later.
//
// It returns the record as the caller should report it, rather than mutating
// through a pointer: every other handler here maps a record it holds by value,
// and a placement that could not even be recorded must not be reported as one
// that was.
func (s *Server) startPlacement(ctx context.Context, sc scope, ws Workspace, why string) Workspace {
	if err := s.setWorkspaceStatus(ctx, sc, ws.ID, WorkspaceStarting, derefOr(ws.TaskRef)); err != nil {
		// Logged rather than fatal: the placement below is still worth
		// attempting, and it writes the status again on its own way through.
		// What is lost is only that this response reports the older state.
		s.log.Error("recording a workspace as starting",
			"workspace", ws.Name, "error", err)
	} else {
		ws.Status = WorkspaceStarting
		ws.LastError = nil
	}

	// Tracked so a shutdown -- and a test -- can wait for what is in flight.
	// Without it the process can exit between dispatching a task and recording
	// its reference, which is an orphaned task nothing is left to account for.
	s.placing.Add(1)
	// contextcheck is right that this drops the caller's context, and dropping
	// it is the entire feature: see the comment below.
	go func() { //nolint:contextcheck
		defer s.placing.Done()
		// The REQUEST's context is deliberately not used: it is cancelled the
		// moment the 202 is written, so a placement inheriting it would be
		// killed by its own success. s.bg outlives every request and is
		// cancelled only when the server is.
		ctx, cancel := context.WithTimeout(s.bg, s.placementTimeout())
		defer cancel()
		s.log.Info("placing a workspace task", "org", sc.Org.Slug,
			"workspace", ws.Name, "why", why)
		if _, err := s.ensureWorkspace(ctx, sc, ws); err != nil {
			// Not returned anywhere and not lost either: ensureWorkspace has
			// already written it to last_error, which is the only place a
			// caller can still see it from.
			s.log.Error("placing a workspace task failed", "org", sc.Org.Slug,
				"workspace", ws.Name, "why", why, "error", err)
			return
		}
		s.log.Info("workspace task placed", "org", sc.Org.Slug, "workspace", ws.Name)
	}()
	return ws
}

// notePlacementFailure records why a placement failed, and what the record
// should now claim about the task.
//
// The status is chosen by ONE question: may a task exist? Nothing else is a
// safe basis, because the next placement decision reads this row and the
// expensive mistake is placing a second task beside one that is still running
// (section 2.8).
//
//   - Dispatched and never dialed in -- a task may well be out there, slow
//     rather than dead, so the row keeps `starting` and its reference. That is
//     also what earns it the reconnect grace on the next attempt instead of an
//     immediate replacement (ensureWorkspace).
//   - Failed before dispatch, with a previous task on the record -- nothing new
//     was placed, so the row goes on describing the OLD task rather than
//     forgetting the reference that is the only handle for stopping it.
//   - Failed before dispatch with no previous task -- nothing is running
//     anywhere, and `stopped` is the only honest thing to say.
func (s *Server) notePlacementFailure(ctx context.Context, sc scope, ws Workspace, dispatched string, cause error) {
	status, ref := WorkspaceStopped, ""
	switch {
	case dispatched != "":
		status, ref = WorkspaceStarting, dispatched
	case derefOr(ws.TaskRef) != "":
		status, ref = ws.Status, derefOr(ws.TaskRef)
	}

	// A FRESH deadline on a context stripped of its cancellation, because the
	// most common way to get here is that ctx itself expired -- waiting out
	// StartTimeout for a task that never dialed in. Recording the failure on
	// the context that caused it would drop exactly the message a user most
	// needs, and leave the row saying `starting` forever.
	write, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := s.failWorkspacePlacement(write, sc, ws.ID, status, ref, cause.Error()); err != nil {
		s.log.Error("recording a failed placement",
			"workspace", ws.Name, "cause", cause, "error", err)
	}
}

// WaitForPlacements blocks until every background placement in flight has
// finished.
//
// Its caller today is the test suite, which needs to be deterministic about a
// goroutine it did not start. It is exported rather than kept unexported
// because it is also what a GRACEFUL shutdown would call, and this process has
// none -- cmd/controlplane runs ListenAndServe to exit. Worth having the hook
// on the side that knows what is in flight, so adding the shutdown later is a
// call rather than a redesign.
func (s *Server) WaitForPlacements() { s.placing.Wait() }
