package ptysession

import (
	"strings"
	"testing"
)

// The prelude exists because Claude Code emits its mode negotiation exactly
// once, at startup, and the SIGWINCH repaint re-emits none of it. These are the
// three sequences measured against CC 2.1.220.
func TestPreludeCapturesClaudeCodeStartupModes(t *testing.T) {
	m := NewModeTracker()
	m.Feed([]byte("\x1b[?2004h\x1b[>4;2m\x1b[>1u\x1b[?25lhello world"))

	got := string(m.Prelude())
	for _, want := range []string{"\x1b[?2004h", "\x1b[>4;2m", "\x1b[>1u", "\x1b[?25l"} {
		if !strings.Contains(got, want) {
			t.Errorf("prelude missing %q\n got: %q", want, got)
		}
	}
	// Payload text must never leak into the prelude -- it is modes, not content.
	if strings.Contains(got, "hello") {
		t.Errorf("prelude contains stream content: %q", got)
	}
}

// A mode set then reset must replay as reset, not as both. Otherwise a
// reattaching client is put back into a mode the application has left.
func TestPreludeKeepsLatestValue(t *testing.T) {
	m := NewModeTracker()
	m.Feed([]byte("\x1b[?1049h"))
	m.Feed([]byte("\x1b[?1049l"))

	got := string(m.Prelude())
	if strings.Contains(got, "\x1b[?1049h") {
		t.Errorf("prelude replays a stale set: %q", got)
	}
	if !strings.Contains(got, "\x1b[?1049l") {
		t.Errorf("prelude lost the reset: %q", got)
	}
}

// Chunk boundaries fall wherever the PTY read lands, so a sequence split across
// two Feed calls must still be recognised. This is the whole reason the tracker
// is a state machine rather than a regexp over each chunk.
func TestModeTrackerHandlesSplitSequences(t *testing.T) {
	for _, split := range []int{1, 2, 3, 4, 5, 6, 7} {
		seq := "\x1b[?2004h"
		m := NewModeTracker()
		m.Feed([]byte(seq[:split]))
		if m.AtBoundary() {
			t.Errorf("split at %d: reported boundary mid-sequence", split)
		}
		m.Feed([]byte(seq[split:]))
		if !m.AtBoundary() {
			t.Errorf("split at %d: not at boundary after a complete sequence", split)
		}
		if got := string(m.Prelude()); got != "\x1b[?2004h" {
			t.Errorf("split at %d: got prelude %q", split, got)
		}
	}
}

// Synchronized output (2026) brackets a single frame. Replaying a stale "begin"
// leaves the reattaching client waiting for an end marker that already went
// past -- a frozen display, which is worse than no prelude at all.
func TestPreludeExcludesSynchronizedOutput(t *testing.T) {
	m := NewModeTracker()
	m.Feed([]byte("\x1b[?2026h\x1b[?2004h"))
	if got := string(m.Prelude()); strings.Contains(got, "2026") {
		t.Errorf("prelude replays synchronized-output mode: %q", got)
	}
}

func TestModeTrackerMultiParamDECSET(t *testing.T) {
	m := NewModeTracker()
	m.Feed([]byte("\x1b[?1000;1002;1006h"))
	got := string(m.Prelude())
	for _, want := range []string{"\x1b[?1000h", "\x1b[?1002h", "\x1b[?1006h"} {
		if !strings.Contains(got, want) {
			t.Errorf("prelude missing %q from a multi-parameter DECSET\n got: %q", want, got)
		}
	}
}

// An unterminated escape must not swallow the rest of the session into the
// partial buffer, and must not wedge the tracker off-boundary forever.
func TestModeTrackerRecoversFromUnterminatedSequence(t *testing.T) {
	m := NewModeTracker()
	m.Feed([]byte("\x1b[" + strings.Repeat("1", maxPartial+10)))
	if !m.AtBoundary() {
		t.Fatal("tracker stuck off-boundary after an over-long sequence")
	}
	m.Feed([]byte("\x1b[?2004h"))
	if got := string(m.Prelude()); !strings.Contains(got, "2004") {
		t.Errorf("tracker did not resync: %q", got)
	}
}

// OSC payloads can contain bytes that look like CSI finals. Treating them as
// modes would put junk in the prelude.
func TestModeTrackerIgnoresOSC(t *testing.T) {
	m := NewModeTracker()
	m.Feed([]byte("\x1b]52;c;SGVsbG8=\x07\x1b[?2004h"))
	got := string(m.Prelude())
	if got != "\x1b[?2004h" {
		t.Errorf("OSC leaked into the prelude: %q", got)
	}
}

func TestModeTrackerBoundsTrackedModes(t *testing.T) {
	m := NewModeTracker()
	for i := range maxTrackedModes * 3 {
		m.Feed([]byte("\x1b[?" + itoa(1000+i) + "h"))
	}
	if len(m.dec) > maxTrackedModes {
		t.Errorf("tracked %d modes, cap is %d", len(m.dec), maxTrackedModes)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
