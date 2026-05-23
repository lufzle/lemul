package ptysession

import (
	"bytes"
	"sort"
	"strconv"
)

// ModeTracker watches a PTY's output stream for the terminal-mode negotiation
// an application performs once at startup, and can replay the current state as
// a short "prelude".
//
// This exists because of a measured property of Claude Code:
// a SIGWINCH nudge makes it repaint the entire visible viewport -- cursor-home
// plus ESC[2K per line -- but it re-emits NO mode sequences. It sets them
// exactly once at startup:
//
//	ESC[?2004h    bracketed paste
//	ESC[>1u       kitty keyboard protocol push
//	ESC[>4;2m     modifyOtherKeys level 2
//
// So a client reattaching into a fresh terminal gets a perfect-looking screen
// with broken Shift+Enter and broken paste -- working display, broken input,
// exactly the failure section 9 predicts. Replaying the prelude ahead of the
// replay ring fixes it without a screen model: this is a scanner over a handful
// of sequence shapes, not a VT emulator.
//
// ModeTracker is not safe for concurrent use; callers hold the session lock.
type ModeTracker struct {
	state    int
	partial  []byte       // bytes of an escape sequence still being consumed
	dec      map[int]bool // DEC private mode -> currently set
	kitty    []byte       // last CSI > Ps u / = Ps u / < Ps u
	modOther []byte       // last CSI > 4 ; Ps m
}

const (
	stGround = iota
	stEsc
	stCSI
)

// maxTrackedModes bounds the map so a hostile or corrupt stream cannot grow it
// without limit.
const maxTrackedModes = 64

// maxPartial discards an escape sequence that never terminates, rather than
// buffering the rest of the session into it.
const maxPartial = 64

// transientModes are DEC private modes that must NOT be replayed even though
// they are "currently set" at the moment of a snapshot.
//
// 2026 is synchronized output (BSU/ESU): it brackets a single frame, so a
// snapshot taken mid-frame would leave a reattaching client with its display
// frozen waiting for an end-marker that already went past.
var transientModes = map[int]bool{2026: true}

func NewModeTracker() *ModeTracker {
	return &ModeTracker{dec: make(map[int]bool)}
}

// AtBoundary reports whether the stream is between escape sequences. The replay
// ring uses this to avoid starting a replay in the middle of one, which would
// make the client's terminal swallow the bytes that follow.
func (m *ModeTracker) AtBoundary() bool { return m.state == stGround }

// Feed consumes a chunk of PTY output.
func (m *ModeTracker) Feed(p []byte) {
	for _, b := range p {
		switch m.state {
		case stGround:
			if b == 0x1b {
				m.state = stEsc
				m.partial = m.partial[:0]
				m.partial = append(m.partial, b)
			}
		case stEsc:
			m.partial = append(m.partial, b)
			if b == '[' {
				m.state = stCSI
			} else {
				// Not a CSI (could be ESC ] OSC, ESC P DCS, or a two-byte
				// sequence). We only track CSI modes, so resync at ground --
				// OSC/DCS payloads are data we do not interpret.
				m.state = stGround
				m.partial = m.partial[:0]
			}
		case stCSI:
			m.partial = append(m.partial, b)
			if b >= 0x40 && b <= 0x7e { // final byte
				m.commit(b)
				m.state = stGround
				m.partial = m.partial[:0]
			} else if len(m.partial) > maxPartial {
				m.state = stGround
				m.partial = m.partial[:0]
			}
		}
	}
}

// commit interprets a complete CSI sequence held in m.partial.
func (m *ModeTracker) commit(final byte) {
	seq := m.partial
	if len(seq) < 3 {
		return
	}
	params := seq[2 : len(seq)-1] // strip ESC [ and the final byte
	if len(params) == 0 {
		return
	}
	switch params[0] {
	case '?':
		if final != 'h' && final != 'l' {
			return
		}
		set := final == 'h'
		for _, f := range bytes.Split(params[1:], []byte{';'}) {
			n, err := strconv.Atoi(string(f))
			if err != nil {
				continue
			}
			if _, known := m.dec[n]; !known && len(m.dec) >= maxTrackedModes {
				continue
			}
			m.dec[n] = set
		}
	case '>', '=', '<':
		switch final {
		case 'u':
			// Kitty keyboard protocol push/pop/set. Keeping only the latest is
			// an approximation of the stack, which is enough: applications push
			// once at startup and pop at exit.
			m.kitty = append([]byte(nil), seq...)
		case 'm':
			// xterm modifyOtherKeys, e.g. CSI > 4 ; 2 m.
			if params[0] == '>' {
				m.modOther = append([]byte(nil), seq...)
			}
		}
	}
}

// Prelude renders the tracked state as a byte sequence to send to a newly
// attached client, ahead of any replayed output.
//
// Order matters: DEC modes first (they include the alternate-screen switch,
// which must precede anything drawn), then the keyboard protocols.
func (m *ModeTracker) Prelude() []byte {
	nums := make([]int, 0, len(m.dec))
	for n := range m.dec {
		if !transientModes[n] {
			nums = append(nums, n)
		}
	}
	sort.Ints(nums)

	var buf bytes.Buffer
	for _, n := range nums {
		buf.WriteString("\x1b[?")
		buf.WriteString(strconv.Itoa(n))
		if m.dec[n] {
			buf.WriteByte('h')
		} else {
			buf.WriteByte('l')
		}
	}
	buf.Write(m.modOther)
	buf.Write(m.kitty)
	return buf.Bytes()
}
