// Package tunnel carries the outbound connection both agents hold to the
// control plane.
//
// Two agents dial out and use this package identically (decision #6,
// task-dials-out): the runner, one per tenant, carrying control commands only;
// and the supervisor, one per workspace task, carrying that workspace's session
// data. Neither listens for inbound connections, so the customer opens no
// firewall rule and grants us no IAM. Tasks dial out; nothing listens inbound.
//
// Three pieces, because a yamux stream is a raw byte pipe:
//
//   - wsnet adapts a WebSocket into a net.Conn so yamux can multiplex over it.
//
//   - frame.go adds length-prefixed framing inside each stream. The
//     client<->relay hop can use WebSocket's own frame types to separate PTY
//     bytes from control messages, but that distinction does not survive the
//     tunnel -- yamux gives us bytes, nothing more. So the type tag has to be
//     carried explicitly.
//
//   - messages.go is the discriminated union of control messages, and documents
//     which side opens which stream.
package tunnel

import (
	"io"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// WSConn adapts a gorilla WebSocket to net.Conn so yamux can run over it.
//
// Reads only consume binary messages; anything else is skipped. Writes emit one
// binary message per call, which keeps yamux's own framing intact.
type WSConn struct {
	ws *websocket.Conn

	mu sync.Mutex // gorilla permits only one concurrent writer

	rmu sync.Mutex
	r   io.Reader // reader for the message currently being drained
}

func NewWSConn(ws *websocket.Conn) *WSConn {
	if tc, ok := ws.UnderlyingConn().(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	return &WSConn{ws: ws}
}

func (c *WSConn) Read(p []byte) (int, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	for {
		if c.r == nil {
			mt, r, err := c.ws.NextReader()
			if err != nil {
				return 0, err
			}
			if mt != websocket.BinaryMessage {
				continue
			}
			c.r = r
		}
		n, err := c.r.Read(p)
		if err == io.EOF {
			c.r = nil
			if n > 0 {
				return n, nil
			}
			continue // message drained; wait for the next one
		}
		return n, err
	}
}

func (c *WSConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ws.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *WSConn) Close() error         { return c.ws.Close() }
func (c *WSConn) LocalAddr() net.Addr  { return c.ws.LocalAddr() }
func (c *WSConn) RemoteAddr() net.Addr { return c.ws.RemoteAddr() }
func (c *WSConn) SetDeadline(t time.Time) error {
	return firstErr(c.ws.SetReadDeadline(t), c.ws.SetWriteDeadline(t))
}
func (c *WSConn) SetReadDeadline(t time.Time) error  { return c.ws.SetReadDeadline(t) }
func (c *WSConn) SetWriteDeadline(t time.Time) error { return c.ws.SetWriteDeadline(t) }

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
