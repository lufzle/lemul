// Package agent is the outbound half of the tunnel, shared by the runner and
// the supervisor.
//
// Both dial OUT and hold the connection open; neither listens. That is the
// whole security story of section 2.2 -- no ingress into the customer VPC, no
// firewall rule, no IAM grant to us -- and it is why both components can share
// one implementation despite doing entirely different jobs once connected.
package agent

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
	"github.com/lufzle/lemul-cc/internal/tunnel"
)

type Config struct {
	// URL is the control-plane tunnel endpoint, ws:// or wss://.
	URL string
	// Token authenticates the agent at the HTTP upgrade, before any stream
	// exists. Phase 1 uses a static shared secret; Phase 3 makes it per-tenant
	// and rotatable.
	Token string
	// Params are identity query parameters (tenant, workspace, runner_id).
	Params map[string]string
	// Retry is the reconnect delay.
	Retry time.Duration
}

// Handler reacts to tunnel lifecycle.
type Handler interface {
	// OnConnect runs once per successful connection. events is a stream the
	// agent opened for unsolicited messages; the control plane accepts exactly
	// one such stream per tunnel.
	OnConnect(events net.Conn) error
	// OnStream handles one command stream opened by the control plane. It runs
	// on its own goroutine and owns closing the stream.
	OnStream(stream net.Conn)
	// OnDisconnect runs when the tunnel drops, before the retry sleep.
	OnDisconnect(err error)
}

// Run dials and serves until ctx is cancelled, reconnecting on failure.
//
// A dropped tunnel is deliberately not fatal for either agent: the runner is
// stateless so reconnecting costs nothing, and the supervisor's PTYs keep
// running in the meantime -- a relay redeploy is a reconnect, not a lost
// session (section 2.8).
func Run(ctx context.Context, cfg Config, h Handler) error {
	if cfg.Retry <= 0 {
		cfg.Retry = 2 * time.Second
	}
	for {
		err := connectAndServe(ctx, cfg, h)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		h.OnDisconnect(err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(cfg.Retry):
		}
	}
}

func connectAndServe(ctx context.Context, cfg Config, h Handler) error {
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}
	q := u.Query()
	for k, v := range cfg.Params {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()

	hdr := http.Header{}
	if cfg.Token != "" {
		hdr.Set("Authorization", "Bearer "+cfg.Token)
	}

	ws, resp, err := websocket.DefaultDialer.DialContext(ctx, u.String(), hdr)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("dial %s: %w (http %s)", u.Redacted(), err, resp.Status)
		}
		return fmt.Errorf("dial %s: %w", u.Redacted(), err)
	}

	sess, err := yamux.Client(tunnel.NewWSConn(ws), yamuxConfig())
	if err != nil {
		_ = ws.Close()
		return fmt.Errorf("yamux: %w", err)
	}
	defer func() { _ = sess.Close() }()

	events, err := sess.Open()
	if err != nil {
		return fmt.Errorf("open event stream: %w", err)
	}
	defer func() { _ = events.Close() }()

	if err := h.OnConnect(events); err != nil {
		return err
	}
	log.Printf("tunnel established to %s", u.Redacted())

	go func() {
		<-ctx.Done()
		_ = sess.Close()
	}()

	for {
		stream, err := sess.Accept()
		if err != nil {
			return err
		}
		go h.OnStream(stream)
	}
}

// YamuxConfig is shared by both ends so keepalive settings cannot drift apart.
//
// Keepalive is the health check for the whole design: it is what tells the
// control plane a tunnel is dead so it can pick a survivor from the set, and
// what tells an agent to reconnect after a partition. 15 s also keeps the
// connection non-idle under an ALB idle timeout (section 2.2).
func YamuxConfig() *yamux.Config {
	c := yamux.DefaultConfig()
	c.EnableKeepAlive = true
	c.KeepAliveInterval = 15 * time.Second
	c.LogOutput = os.Stderr
	return c
}

func yamuxConfig() *yamux.Config { return YamuxConfig() }
