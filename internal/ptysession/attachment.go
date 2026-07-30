package ptysession

import "sync"

// CloseReason tells the supervisor how to close the stream carrying an
// attachment, which is how the client decides whether to reconnect.
type CloseReason int

const (
	ReasonNone CloseReason = iota
	// ReasonSessionEnded -- the child exited. Clean close; do not reconnect.
	ReasonSessionEnded
	// ReasonDetached -- the client went away, or was detached deliberately.
	ReasonDetached
	// ReasonOverflow -- this attacher could not keep up. Abnormal close, so the
	// client reconnects and gets a clean repaint.
	ReasonOverflow
)

func (r CloseReason) String() string {
	switch r {
	case ReasonSessionEnded:
		return "session ended"
	case ReasonDetached:
		return "detached"
	case ReasonOverflow:
		return "attacher too slow"
	default:
		return "closed"
	}
}

// Attachment is one client's view of a session.
//
// Output is queued rather than written directly because the PTY reader must
// never block: a stalled client would otherwise stall Claude Code's stdout and
// with it the agent itself. When a client falls too far behind we drop the
// attachment instead of dropping bytes -- dropping bytes corrupts a terminal
// silently, whereas a disconnect is recoverable, and reattaching costs only a
// repaint.
type Attachment struct {
	sess     *Session
	readOnly bool

	mu     sync.Mutex
	cond   *sync.Cond
	q      [][]byte
	queued int
	limit  int
	closed bool
	reason CloseReason
}

func newAttachment(s *Session, readOnly bool, limit int) *Attachment {
	if limit <= 0 {
		limit = defaultQueueBytes
	}
	a := &Attachment{sess: s, readOnly: readOnly, limit: limit}
	a.cond = sync.NewCond(&a.mu)
	return a
}

// push enqueues output. Callers hold the session lock, so this must not block.
func (a *Attachment) push(p []byte) {
	if len(p) == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return
	}
	if a.queued+len(p) > a.limit {
		a.closed = true
		a.reason = ReasonOverflow
		a.q = nil
		a.queued = 0
		a.cond.Broadcast()
		return
	}
	a.q = append(a.q, append([]byte(nil), p...))
	a.queued += len(p)
	a.cond.Broadcast()
}

// Next blocks until output is available or the attachment closes. ok is false
// once closed, and reason says why.
func (a *Attachment) Next() (p []byte, reason CloseReason, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for len(a.q) == 0 && !a.closed {
		a.cond.Wait()
	}
	if len(a.q) > 0 {
		p = a.q[0]
		a.q = a.q[1:]
		a.queued -= len(p)
		return p, ReasonNone, true
	}
	return nil, a.reason, false
}

// Write forwards client input to the child.
//
// A viewer's input is discarded here rather than rejected: both attachers share
// one PTY stdin, so silently dropping is the only behaviour that keeps a viewer
// from becoming a co-driver without also tearing down its view (section 2.5).
func (a *Attachment) Write(p []byte) (int, error) {
	if a.readOnly {
		return len(p), nil
	}
	return a.sess.write(p)
}

// Resize is a no-op for viewers: a viewer must not resize the shared PTY.
func (a *Attachment) Resize(rows, cols uint16) error {
	if a.readOnly {
		return nil
	}
	return a.sess.Resize(rows, cols)
}

func (a *Attachment) ReadOnly() bool { return a.readOnly }

func (a *Attachment) close(reason CloseReason) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return
	}
	a.closed = true
	a.reason = reason
	a.cond.Broadcast()
}
