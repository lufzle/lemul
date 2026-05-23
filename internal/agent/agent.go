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
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
	"github.com/lufzle/lemul/internal/logging"
	"github.com/lufzle/lemul/internal/tunnel"
)

// ErrUnauthorized means the control plane rejected the agent's credential at
// the upgrade. It ends the retry loop, because it is the one failure that
// retrying cannot fix.
//
// A workspace credential is minted per placement and held in control-plane
// memory, so a control-plane restart invalidates every live task's credential
// permanently -- there is no later moment at which the same credential starts
// working. Treated as transient, that produced a task reconnecting every two
// seconds forever: unreachable, holding the workspace volume, and on Fargate
// billing indefinitely with nothing able to stop it. Exiting instead lets the
// scheduler reclaim it, and the control plane places a fresh task on demand.
var ErrUnauthorized = errors.New("control plane rejected the agent credential")

type Config struct {
	// URL is the control-plane tunnel endpoint, ws:// or wss://.
	URL string
	// Token authenticates the agent at the HTTP upgrade, before any stream
	// exists. Derived per organization and per workspace generation
	// (internal/creds), so it says WHICH tenant and task is dialling rather than
	// merely that some part of the fleet is.
	Token string
	// Params are identity query parameters (tenant, workspace, runner_id).
	Params map[string]string
	// Retry is the reconnect delay.
	Retry time.Duration
	// FatalOnUnauthorized ends the retry loop on a 401 rather than backing off.
	//
	// True for the CONTROL tunnel and false for the DATA one, and the asymmetry
	// is the whole of the two-tunnel orphan rule.
	//
	// A control 401 comes from the Management API, which read the workspace
	// record: it means this task is no longer the current generation and has
	// been replaced. Retrying cannot fix that, and a task that keeps trying
	// holds the workspace volume and bills indefinitely with nothing able to
	// stop it -- so it exits and lets the scheduler reclaim it.
	//
	// A data 401 means only that the RELAY did not accept the credential, which
	// a redeploy or a key it has not caught up to can both produce. Exiting
	// there would kill live PTYs for somebody else's fault, so it backs off like
	// any other failure and the sessions keep running unattached.
	FatalOnUnauthorized bool
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
		// The one error worth giving up on, and only on the tunnel where it
		// means what it says -- see FatalOnUnauthorized. Everything else, a
		// dropped connection, a relay redeploy, a network partition, is a reason
		// to keep trying, which is what keeps an unattended run alive (2.8).
		if cfg.FatalOnUnauthorized && errors.Is(err, ErrUnauthorized) {
			return err
		}
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
	// Announced by both agents from one place, so the runner and the supervisor
	// cannot drift apart on what they claim to speak.
	q.Set(tunnel.ProtocolParam, strconv.Itoa(tunnel.ProtocolVersion))
	u.RawQuery = q.Encode()

	hdr := http.Header{}
	if cfg.Token != "" {
		hdr.Set("Authorization", "Bearer "+cfg.Token)
	}

	ws, resp, err := websocket.DefaultDialer.DialContext(ctx, u.String(), hdr)
	if resp != nil {
		// Closed on both paths. gorilla returns the handshake response whether or
		// not the upgrade succeeded, and leaving it open leaks a connection per
		// failed dial -- which, on a 2 s retry loop against a control plane that
		// is refusing us, is every two seconds forever.
		defer func() { _ = resp.Body.Close() }()
	}
	if err != nil {
		if resp != nil {
			// 401/403 is a verdict on the credential rather than a hiccup, and
			// the credential cannot change without a new placement.
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				return fmt.Errorf("dial %s: %w (http %s)", u.Redacted(), ErrUnauthorized, resp.Status)
			}
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
	slog.Info("tunnel established", "url", u.Redacted())

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
	// Routed through slog rather than straight to stderr: yamux writes
	// pre-formatted lines that would otherwise bypass the handler entirely,
	// unlevelled and unstructured even where everything else is JSON. Debug,
	// because its loudest message is a routine "unexpected EOF" on every clean
	// disconnect -- real, but not worth waking anyone.
	c.LogOutput = logging.Writer(slog.Default(), slog.LevelDebug)
	return c
}

func yamuxConfig() *yamux.Config { return YamuxConfig() }
