package ptysession

// ring is a bounded replay buffer of recent PTY output.
//
// Its role is smaller than it first appears. The winch-probe measurement showed
// that Claude Code's SIGWINCH repaint reconstructs the entire visible viewport
// from a blank screen, so the ring is NOT what makes a reattached screen
// correct -- the nudge is. The ring only restores scrollback above the
// viewport, which is a nicety rather than a correctness requirement.
//
// It stores whole chunks rather than a flat byte buffer so that eviction can
// respect escape-sequence boundaries: replaying from the middle of a CSI
// sequence would make the client's terminal swallow the bytes that follow it.
type ring struct {
	chunks []chunk
	bytes  int
	limit  int
}

type chunk struct {
	b []byte
	// cleanStart records that the stream was between escape sequences when this
	// chunk began, so a replay may start here safely.
	cleanStart bool
}

func newRing(limit int) *ring {
	if limit <= 0 {
		limit = defaultRingBytes
	}
	return &ring{limit: limit}
}

func (r *ring) push(b []byte, cleanStart bool) {
	c := chunk{b: append([]byte(nil), b...), cleanStart: cleanStart}
	r.chunks = append(r.chunks, c)
	r.bytes += len(c.b)
	for r.bytes > r.limit && len(r.chunks) > 0 {
		r.bytes -= len(r.chunks[0].b)
		r.chunks = r.chunks[1:]
	}
}

// snapshot returns the replayable contents, starting at the oldest chunk that
// begins on an escape-sequence boundary. If no retained chunk does, it returns
// nothing rather than risk corrupting the client's terminal -- the repaint that
// follows will fill the viewport regardless.
func (r *ring) snapshot() []byte {
	start := -1
	for i, c := range r.chunks {
		if c.cleanStart {
			start = i
			break
		}
	}
	if start < 0 {
		return nil
	}
	n := 0
	for _, c := range r.chunks[start:] {
		n += len(c.b)
	}
	out := make([]byte, 0, n)
	for _, c := range r.chunks[start:] {
		out = append(out, c.b...)
	}
	return out
}
