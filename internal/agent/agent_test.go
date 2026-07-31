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

// The defect this pins: a workspace credential is minted per placement and kept
// in control-plane memory, so a control-plane restart invalidates it forever.
// Retrying it produced a task reconnecting every two seconds indefinitely --
// unreachable, holding the workspace volume, and on Fargate billing with
// nothing able to stop it.
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

	err := Run(ctx, Config{URL: wsURL(srv), Token: "stale", Retry: 10 * time.Millisecond}, h)

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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := Run(ctx, Config{URL: wsURL(srv), Retry: 10 * time.Millisecond}, &countingHandler{})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
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
