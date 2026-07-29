package tunnel

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// Frame types carried inside a yamux stream.
//
// On the client<->relay hop, WebSocket's own frame types separate PTY bytes
// from control messages. Inside the tunnel there are no frame types, so the
// tag is explicit. Getting this wrong is the most likely way step 2 breaks
// fidelity: a control message misread as PTY data would inject JSON into the
// terminal, and PTY data misread as control would drop output silently.
const (
	FrameData    byte = 0x00 // raw PTY bytes, passed through untouched
	FrameControl byte = 0x01 // JSON control message (proto.Control)
)

// maxFrame bounds a single frame so a corrupt length header cannot make the
// reader allocate arbitrarily. PTY reads are 32 KiB, so this is generous.
const maxFrame = 1 << 20

// WriteFrame emits one framed message: [type:1][len:4 BE][payload].
func WriteFrame(w io.Writer, typ byte, payload []byte) error {
	if len(payload) > maxFrame {
		return fmt.Errorf("frame too large: %d", len(payload))
	}
	var hdr [5]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := w.Write(payload)
	return err
}

// WriteControlFrame marshals v and writes it as a control frame.
func WriteControlFrame(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return WriteFrame(w, FrameControl, b)
}

// ReadFrame reads one framed message. The returned slice is freshly allocated,
// so callers may retain it.
func ReadFrame(r io.Reader) (typ byte, payload []byte, err error) {
	var hdr [5]byte
	if _, err = io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxFrame {
		return 0, nil, fmt.Errorf("frame too large: %d", n)
	}
	if n == 0 {
		return hdr[0], nil, nil
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return hdr[0], payload, nil
}

// Hello is the first control frame on a new session stream. The terminal size
// must arrive before the runner forks the child -- a PTY defaults to 0x0, and a
// resize sent afterwards is too late (the TUI has already drawn its first
// frame). See CC_REMOTE_ANALYSIS.md 4.1.
type Hello struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
	Cmd  string `json:"cmd,omitempty"`
	Term string `json:"term,omitempty"`
}
