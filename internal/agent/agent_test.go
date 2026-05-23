package agent

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// countingHandler records lifecycle callbacks without doing any work.
type countingHandler struct{ disconnects atomic.Int32 }

func (h *countingHandler) OnConnect(net.Conn) error { return nil }
func (h *countingHandler) OnStream(c net.Conn)      { _ = c.Close() }
func (h *countingHandler) OnDisconnect(error)       { h.disconnects.Add(1) }

func wsURL(s *httptest.Server) string { return "ws" + strings.TrimPrefix(s.URL, "http") }

// The defect this pins: a workspace credential is derived per placement, so a
// task that has been REPLACED holds one that will never be valid again.
// Retrying it produced a task reconnecting every two seconds indefinitely --
// unreachable, holding the workspace volume, and on Fargate billing with
// nothing able to stop it.
//
// Opt-in since Phase 5, and the opt is the whole two-tunnel orphan rule: this
// is the CONTROL tunnel's policy, where a 401 came from the service that read
// the workspace record and therefore means "you have been replaced". The data
// tunnel's 401 means something much weaker -- see the test below.
func TestRunStopsWhenTheCredentialIsRejected(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	h := &countingHandler{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := Run(ctx, Config{
		URL: wsURL(srv), Token: "stale", Retry: 10 * time.Millisecond,
		FatalOnUnauthorized: true,
	}, h)

	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("dialled %d times, want exactly 1 -- a rejected credential never becomes valid", got)
	}
	// The handler still hears about it, so the supervisor can log why the task
	// is going away rather than vanishing silently.
	if h.disconnects.Load() != 1 {
		t.Errorf("OnDisconnect called %d times, want 1", h.disconnects.Load())
	}
}

func TestRunStopsOnForbidden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := Run(ctx, Config{
		URL: wsURL(srv), Retry: 10 * time.Millisecond, FatalOnUnauthorized: true,
	}, &countingHandler{})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
}

// THE DATA TUNNEL'S POLICY, and the half that would be easy to get wrong by
// leaving the old behaviour in place for both.
//
// A 401 from the RELAY says only that the relay would not take the credential,
// which a redeploy or a key it has not caught up to both produce. Exiting there
// would end every live PTY in the task for a fault that is not the task's --
// so it retries, and the sessions keep running with nobody able to attach until
// it comes back.
//
// The workspace is not left reachable-but-stale by this: a task that really HAS
// been replaced loses its control tunnel, where the 401 is authoritative, and
// exits from there.
func TestRunKeepsRetryingWhenUnauthorizedIsNotFatal(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	err := Run(ctx, Config{
		URL: wsURL(srv), Token: "stale", Retry: 10 * time.Millisecond,
		FatalOnUnauthorized: false,
	}, &countingHandler{})

	// It ends because the CONTEXT ended, not because it gave up.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context deadline", err)
	}
	if got := attempts.Load(); got < 2 {
		t.Errorf("dialled %d times; a data-tunnel 401 must be retried, not fatal", got)
	}
}

// The other half, and the more important one to keep: everything that is NOT a
// verdict on the credential must keep retrying, because a relay redeploy or a
// network partition has to be a blip rather than a lost session (section 2.8).
func TestRunKeepsRetryingOtherFailures(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{URL: wsURL(srv), Retry: 5 * time.Millisecond}, &countingHandler{})
	}()

	deadline := time.After(3 * time.Second)
	for attempts.Load() < 3 {
		select {
		case <-deadline:
			cancel()
			t.Fatalf("only %d attempts; a 503 should be retried", attempts.Load())
		case err := <-done:
			t.Fatalf("Run returned early on a retryable failure: %v", err)
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("Run did not return after cancel")
	}
}

// A dial that fails with no HTTP response at all -- nothing listening -- is
// retryable too, and must not be mistaken for a rejected credential.
func TestRunRetriesWhenThereIsNoServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	h := &countingHandler{}
	err := Run(ctx, Config{URL: "ws://127.0.0.1:1", Retry: 10 * time.Millisecond}, h)

	if errors.Is(err, ErrUnauthorized) {
		t.Fatal("a refused connection was treated as a rejected credential")
	}
	if h.disconnects.Load() < 2 {
		t.Errorf("disconnects = %d, want repeated retries", h.disconnects.Load())
	}
}
