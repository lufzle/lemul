// Command relay is the control-plane component. It is the only thing with a
// public listener.
//
// Two endpoints:
//
//	/tunnel  the runner dials in from the customer VPC and holds this open
//	/pty     a user's client connects here; the relay opens a yamux stream on
//	         the runner's tunnel and pumps bytes between the two
//
// In production the /pty handler authenticates the user, resolves their org's
// runner, and (on Fly) returns fly-replay to hand the connection to the right
// machine. Here it authenticates nothing and assumes a single runner, which is
// all step 2 needs to measure the mux's effect on fidelity.
package main

import (
	"flag"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
	"github.com/lufzle/tui-proxy-proto/proto"
	"github.com/lufzle/tui-proxy-proto/tunnel"
)

var addr = flag.String("addr", ":9000", "listen address")

var upgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true },
}

// registry holds the current runner tunnel. Production keys this by tenant.
type registry struct {
	mu   sync.RWMutex
	sess *yamux.Session
}

func (r *registry) set(s *yamux.Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sess = s
}

func (r *registry) get() *yamux.Session {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sess
}

var reg = &registry{}

func main() {
	flag.Parse()
	http.HandleFunc("/tunnel", handleTunnel)
	http.HandleFunc("/pty", handlePTY)
	log.Printf("relay listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

// handleTunnel accepts the runner's outbound connection and keeps it as the
// multiplexed transport for all sessions belonging to that runner.
func handleTunnel(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	conn := tunnel.NewWSConn(ws)

	// Relay is the yamux server: it opens streams, the runner accepts them.
	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = true
	cfg.LogOutput = io.Discard
	sess, err := yamux.Server(conn, cfg)
	if err != nil {
		log.Printf("tunnel: yamux: %v", err)
		return
	}
	reg.set(sess)
	log.Printf("runner tunnel registered from %s", ws.RemoteAddr())

	<-sess.CloseChan()
	reg.set(nil)
	log.Printf("runner tunnel closed")
}

// handlePTY joins one user WebSocket to one yamux stream on the runner tunnel.
func handlePTY(w http.ResponseWriter, r *http.Request) {
	sess := reg.get()
	if sess == nil {
		http.Error(w, "no runner connected", http.StatusServiceUnavailable)
		return
	}

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := proto.NewConn(ws)
	defer c.Close()

	stream, err := sess.Open()
	if err != nil {
		log.Printf("pty: open stream: %v", err)
		return
	}
	defer func() { _ = stream.Close() }()

	// The size travels in the query string and must reach the runner before it
	// forks -- so it goes in the hello frame, ahead of any data.
	hello := tunnel.Hello{
		Rows: uint16(queryUint(r, "rows", 24)),
		Cols: uint16(queryUint(r, "cols", 80)),
		Cmd:  r.URL.Query().Get("cmd"),
	}
	if err := tunnel.WriteControlFrame(stream, hello); err != nil {
		log.Printf("pty: hello: %v", err)
		return
	}
	log.Printf("session opened: size=%dx%d", hello.Cols, hello.Rows)

	// runner -> user. Data frames become binary websocket messages; the byte
	// payload is never inspected or transformed.
	go func() {
		defer c.Close()
		for {
			typ, payload, err := tunnel.ReadFrame(stream)
			if err != nil {
				// Stream closed: child exited or tunnel dropped. Tell the client
				// this was a clean end so it does not try to reconnect.
				_ = c.WriteCloseFrame(websocket.CloseNormalClosure, "session ended")
				return
			}
			if typ == tunnel.FrameData {
				if err := c.WriteBytes(payload); err != nil {
					return
				}
			}
		}
	}()

	// user -> runner. WebSocket frame type selects the tunnel frame type: this
	// is the translation point between the two framing schemes.
	for {
		mt, data, err := c.Read()
		if err != nil {
			return
		}
		var typ byte
		switch mt {
		case websocket.BinaryMessage:
			typ = tunnel.FrameData
		case websocket.TextMessage:
			typ = tunnel.FrameControl
		default:
			continue
		}
		if err := tunnel.WriteFrame(stream, typ, data); err != nil {
			return
		}
	}
}

func queryUint(r *http.Request, key string, def uint64) uint64 {
	if v, err := strconv.ParseUint(r.URL.Query().Get(key), 10, 16); err == nil && v > 0 {
		return v
	}
	return def
}
