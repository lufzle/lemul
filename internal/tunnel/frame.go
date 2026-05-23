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
// tag is explicit. Getting this wrong is the most likely way the mux breaks
// fidelity: a control message misread as PTY data would inject JSON into the
// terminal, and PTY data misread as control would drop output silently.
const (
	FrameData    byte = 0x00 // raw PTY bytes, passed through untouched
	FrameControl byte = 0x01 // JSON Envelope
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

// Envelope is the control-frame payload: a discriminated union over the
// message types below. Body is left raw so a reader can dispatch on Type
// before committing to a concrete struct.
type Envelope struct {
	Type string          `json:"type"`
	Body json.RawMessage `json:"body,omitempty"`
}

// WriteMsg marshals v into an Envelope of the given type and writes it as a
// control frame.
func WriteMsg(w io.Writer, typ string, v any) error {
	env := Envelope{Type: typ}
	if v != nil {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		env.Body = b
	}
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return WriteFrame(w, FrameControl, b)
}

// ReadMsg reads the next frame and requires it to be a control frame.
// A data frame arriving where a control message is expected is a protocol
// error, not something to skip -- see the comment on the frame types.
func ReadMsg(r io.Reader) (Envelope, error) {
	typ, payload, err := ReadFrame(r)
	if err != nil {
		return Envelope{}, err
	}
	if typ != FrameControl {
		return Envelope{}, fmt.Errorf("expected control frame, got type %#x", typ)
	}
	var env Envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return Envelope{}, fmt.Errorf("bad envelope: %w", err)
	}
	return env, nil
}

// Decode unmarshals an envelope body into v.
func (e Envelope) Decode(v any) error {
	if len(e.Body) == 0 {
		return nil
	}
	return json.Unmarshal(e.Body, v)
}
