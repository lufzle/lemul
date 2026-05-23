package ptysession

import (
	"bytes"
	"strings"
	"testing"
)

func TestRingEvictsOldestFirst(t *testing.T) {
	r := newRing(10)
	r.push([]byte("aaaa"), true)
	r.push([]byte("bbbb"), true)
	r.push([]byte("cccc"), true)

	got := string(r.snapshot())
	if strings.Contains(got, "aaaa") {
		t.Errorf("oldest chunk survived eviction: %q", got)
	}
	if !strings.Contains(got, "cccc") {
		t.Errorf("newest chunk was evicted: %q", got)
	}
	if r.bytes > r.limit {
		t.Errorf("ring holds %d bytes, limit %d", r.bytes, r.limit)
	}
}

// Replaying from the middle of an escape sequence makes the client's terminal
// swallow the bytes that follow it, so a replay may only start on a boundary.
func TestRingSnapshotStartsOnAnEscapeBoundary(t *testing.T) {
	r := newRing(1 << 20)
	r.push([]byte("\x1b[38;2;255"), true) // chunk split mid-CSI
	r.push([]byte("m tail"), false)       // continuation: not a safe start

	if got := string(r.snapshot()); !strings.HasPrefix(got, "\x1b[") {
		t.Errorf("snapshot did not start at the clean chunk: %q", got)
	}

	// With the clean head evicted, nothing left is safe to replay. Returning
	// nothing is correct: the repaint that follows fills the viewport anyway.
	r2 := newRing(8)
	r2.push([]byte("\x1b[38;2;255"), true)
	r2.push([]byte("m tail!!"), false)
	if got := r2.snapshot(); got != nil {
		t.Errorf("snapshot replayed from mid-sequence: %q", got)
	}
}

func TestRingSnapshotPreservesOrderAndBytes(t *testing.T) {
	r := newRing(1 << 20)
	want := []byte{}
	for _, s := range [][]byte{{0x00, 0x80, 0xff}, []byte("middle"), {0x1b, '[', 'K'}} {
		r.push(s, true)
		want = append(want, s...)
	}
	if got := r.snapshot(); !bytes.Equal(got, want) {
		t.Errorf("snapshot corrupted bytes\n want: % x\n got:  % x", want, got)
	}
}

// push must copy: the caller reuses its read buffer on the next PTY read.
func TestRingCopiesPushedBytes(t *testing.T) {
	r := newRing(1 << 20)
	buf := []byte("original")
	r.push(buf, true)
	copy(buf, "OVERWRIT")
	if got := string(r.snapshot()); got != "original" {
		t.Errorf("ring aliased the caller's buffer: %q", got)
	}
}
