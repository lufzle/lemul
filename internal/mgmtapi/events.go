package mgmtapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/lufzle/lemul/internal/store"
)

// Section 2.6's workspace event stream, which was in the API surface from the
// start and had never been built.
//
// Its absence is what made a cold start look like a hang. `lem` posts a session,
// the control plane blocks inside ensureWorkspace waiting for a Fargate task to
// dial in, and the user watches a blank terminal for 25 s with no way to tell a
// slow start from a wedged one. Nothing was wrong with either component -- there
// was simply no channel on which "the machine is booting" could be said.
//
// So this is the THIRD consumer of the one mechanism increment 4 adds, not a
// second one: the row's `status` and `last_error` are the content, and this
// endpoint is a way of reading them without polling by hand. The provisioning
// row, the console table and the CLI's progress all watch the same two fields.
//
// It polls the record rather than subscribing to an in-process broadcast, and
// that is a correctness choice rather than a shortcut. A workspace's control
// tunnel is held by exactly ONE control-plane process (reaper.go), so the
// replica that placed a task is very often not the replica a console is talking
// to -- an in-memory fan-out would deliver nothing at all to the other one, and
// would do it silently. The database is the only thing both replicas can see.

// eventsPollInterval is how often the record is re-read for a watching client.
//
// Twice a second: fast enough that a status change looks immediate to somebody
// staring at a terminal, and cheap enough that it is one indexed primary-key
// read per watcher. Nothing here is on the placement path, so a slower value
// would only make the UI lag behind a decision already taken.
const eventsPollInterval = 500 * time.Millisecond

// eventsHeartbeat keeps an idle stream from being reaped by something in the
// middle. A workspace can legitimately sit in one state for the whole 20-60 s
// of a cold start, which is long enough for an ALB idle timeout to close a
// connection that is working perfectly.
const eventsHeartbeat = 20 * time.Second

// eventsMaxLifetime bounds one stream. A watcher is welcome to reconnect; a
// handler that never returns is a goroutine and a database poll this process
// holds for as long as a browser tab is open and forgotten.
const eventsMaxLifetime = 30 * time.Minute

// workspaceEvent is what changes about a workspace while somebody watches it.
//
// Deliberately NOT workspaceDoc. Two fields of that document -- the session and
// running counts -- cost a tunnel round trip to the task, and paying that twice
// a second per watcher to report numbers nobody is waiting on would turn a
// progress indicator into load. A client merges these fields onto the row it
// already has.
type workspaceEvent struct {
	Status string `json:"status"`
	// Connected is whether a task is holding a tunnel ON THIS REPLICA. It can
	// disagree with Status, and that disagreement is the useful part: a
	// workspace recorded active whose task died reads active+disconnected.
	Connected bool   `json:"connected"`
	TaskRef   string `json:"task_ref,omitempty"`
	LastError string `json:"last_error,omitempty"`
	At        string `json:"at"`
}

// handleWorkspaceEvents streams a workspace's status transitions as
// Server-Sent Events.
//
// The first event is a SNAPSHOT, sent before anything changes. Without it a
// client that connects to a workspace which is already active waits forever for
// a transition that has already happened -- and "already up" is the common case
// this endpoint has to answer quickly, not the exceptional one.
func (s *Server) handleWorkspaceEvents(w http.ResponseWriter, r *http.Request) {
	a, ok := s.requireWorkspace(w, r)
	if !ok {
		return
	}
	// Without a flusher every event sits in a buffer until the handler returns,
	// which for a stream is never. Refuse rather than serve a stream that will
	// deliver its first byte at the end of the cold start it was meant to
	// narrate.
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming is not supported by this server", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	// Named explicitly because an intermediary that buffers this response
	// defeats the whole endpoint, and nginx in particular does so by default.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ctx, cancel := context.WithTimeout(r.Context(), eventsMaxLifetime)
	defer cancel()

	sc, ws := a.scope, a.WS
	last := s.workspaceEventFor(sc, ws)
	if !s.writeEvent(w, flusher, last) {
		return
	}

	poll := time.NewTicker(eventsPollInterval)
	defer poll.Stop()
	beat := time.NewTicker(eventsHeartbeat)
	defer beat.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-beat.C:
			// An SSE comment: it keeps the connection warm and is ignored by
			// every parser, so it cannot be mistaken for a state change.
			if _, err := w.Write([]byte(": keep-alive\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case <-poll.C:
			rec, err := s.getWorkspaceByName(ctx, sc, ws.Name)
			if errors.Is(err, store.ErrNotFound) {
				// Deleted while being watched. Ending the stream is the honest
				// answer; inventing a "deleted" event would be a fourth status
				// value that every comparison would have to learn.
				return
			}
			if err != nil {
				// A transient read failure is not a state change. Saying
				// nothing is right: the next tick re-reads, and a stream that
				// reported the database being briefly unavailable as a
				// workspace transition would be worse than a gap.
				continue
			}
			next := s.workspaceEventFor(sc, rec)
			if next.sameAs(last) {
				continue
			}
			last = next
			if !s.writeEvent(w, flusher, next) {
				return
			}
		}
	}
}

// workspaceEventFor renders the record plus the one thing the record cannot
// say: whether a task is holding a tunnel right now.
func (s *Server) workspaceEventFor(sc scope, ws Workspace) workspaceEvent {
	ev := workspaceEvent{
		Status:    ws.Status,
		TaskRef:   derefOr(ws.TaskRef),
		LastError: derefOr(ws.LastError),
		At:        timestamp(time.Now()),
	}
	// By NAME. The registry is keyed on the name the supervisor was started
	// with, the database on the uuid -- two identifiers for one thing, and the
	// trap the split introduces (state.go).
	if _, err := s.reg.PickWorkspace(sc.TenantID, ws.Name); err == nil {
		ev.Connected = true
	}
	return ev
}

// sameAs compares everything except the timestamp, which is what makes "has
// anything changed" a question about the workspace rather than about the clock.
func (e workspaceEvent) sameAs(o workspaceEvent) bool {
	return e.Status == o.Status && e.Connected == o.Connected &&
		e.TaskRef == o.TaskRef && e.LastError == o.LastError
}

// writeEvent emits one SSE frame and reports whether the client is still there.
func (s *Server) writeEvent(w http.ResponseWriter, f http.Flusher, ev workspaceEvent) bool {
	body, err := json.Marshal(ev)
	if err != nil {
		return false
	}
	if _, err := w.Write([]byte("event: workspace\ndata: " + string(body) + "\n\n")); err != nil {
		return false
	}
	f.Flush()
	return true
}
