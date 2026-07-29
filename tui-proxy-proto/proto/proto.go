// Package proto defines the wire contract between the local client and the
// supervisor that owns the PTY.
//
// Framing relies on WebSocket's own frame types rather than a length-prefixed
// header:
//
//	BinaryMessage -> raw PTY bytes, passed through untouched
//	TextMessage   -> JSON control message (see Control)
//
// Binary frames are mandatory for terminal data. Terminal output is not
// guaranteed to be valid UTF-8 at arbitrary chunk boundaries -- a multi-byte
// rune or an escape sequence will straddle a frame -- and TextMessage forces
// UTF-8 validation, which corrupts it. See CC_REMOTE_ANALYSIS.md section 4.1.
package proto

import (
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Control message types (sent as TextMessage).
const (
	TypeResize = "resize"
)

// Control is an out-of-band control message. Terminal size cannot travel in
// the byte stream, so resize needs its own channel: the supervisor turns it
// into ioctl(TIOCSWINSZ), which delivers SIGWINCH to the child.
type Control struct {
	Type string `json:"type"`
	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
}

// Conn wraps a websocket.Conn with a write mutex. gorilla/websocket permits
// only one concurrent writer, and both sides of this prototype have two
// writing goroutines (byte pump + resize handler).
type Conn struct {
	ws *websocket.Conn
	mu sync.Mutex
}

func NewConn(ws *websocket.Conn) *Conn {
	// Explicit even though Go's net package enables TCP_NODELAY by default:
	// one missed hop batches single keystrokes into ~40ms delays, which reads
	// as "the product feels laggy" rather than as a bug.
	if tc, ok := ws.UnderlyingConn().(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	return &Conn{ws: ws}
}

// WriteBytes sends PTY data as a binary frame.
func (c *Conn) WriteBytes(p []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ws.WriteMessage(websocket.BinaryMessage, p)
}

// WriteControl sends a control message as a text frame.
func (c *Conn) WriteControl(m Control) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ws.WriteJSON(m)
}

// Read returns the next frame: messageType is websocket.BinaryMessage for PTY
// data or websocket.TextMessage for a control message.
func (c *Conn) Read() (messageType int, p []byte, err error) {
	return c.ws.ReadMessage()
}

// WriteCloseFrame sends a WebSocket close frame with the given code.
//
// The code is load-bearing, not decoration: it is how the client distinguishes
// a finished session (CloseNormalClosure -- do not reconnect) from a dropped
// connection (abnormal/no close frame -- reconnect and replay). Step 2's
// reconnect logic depends on that distinction.
func (c *Conn) WriteCloseFrame(code int, text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ws.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(code, text),
		time.Now().Add(2*time.Second),
	)
}

// IsCleanClose reports whether err is a normal end-of-session close rather
// than an unexpected drop.
func IsCleanClose(err error) bool {
	return websocket.IsCloseError(err,
		websocket.CloseNormalClosure,
		websocket.CloseGoingAway,
	)
}

func (c *Conn) Close() error { return c.ws.Close() }

// Raw exposes the underlying connection for close-handler wiring.
func (c *Conn) Raw() *websocket.Conn { return c.ws }
