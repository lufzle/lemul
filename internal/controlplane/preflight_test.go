package controlplane

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lufzle/lemul-cc/internal/tunnel"
)

func newTestServer() *Server { return &Server{preflights: newPreflightStore()} }

// The whole point: a session must not be admitted into a workspace we already
// know cannot reach a model, and the refusal has to carry the fix.
func TestCheckPreflightBlocksOnEvidence(t *testing.T) {
	s := newTestServer()
	s.preflights.put(tunnel.PreflightReport{
		WorkspaceID: "w1",
		Blocking:    true,
		Models: []tunnel.PreflightModel{{
			Role: "opus", ModelID: "us.anthropic.claude-opus-5", Required: true,
			Invocable: false, ErrorCode: "ValidationException",
			Advice: "Submit the Anthropic First Time Use form",
		}},
	})

	err := s.checkPreflight(context.Background(), "w1")
	if err == nil {
		t.Fatal("admitted a session into a workspace with a blocking preflight")
	}
	if !strings.Contains(err.Error(), "First Time Use") {
		t.Errorf("refusal does not carry the advice: %v", err)
	}
	if !strings.Contains(err.Error(), "us.anthropic.claude-opus-5") {
		t.Errorf("refusal does not name the model: %v", err)
	}
}

// Fail open on the ABSENCE of evidence: an older supervisor or a bug in our own
// event path must not take the product down.
func TestCheckPreflightFailsOpenWhenNoReportArrives(t *testing.T) {
	s := newTestServer()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := s.checkPreflight(ctx, "w1"); err != nil {
		t.Errorf("refused a session with no report at all: %v", err)
	}
	if elapsed := time.Since(start); elapsed > preflightWait {
		t.Errorf("waited %v, longer than the caller's deadline allowed", elapsed)
	}
}

// A workspace that is not using Bedrock reports skipped, and skipped is not a
// failure.
func TestCheckPreflightAllowsSkipped(t *testing.T) {
	s := newTestServer()
	s.preflights.put(tunnel.PreflightReport{WorkspaceID: "w1", Skipped: true})
	if err := s.checkPreflight(context.Background(), "w1"); err != nil {
		t.Errorf("refused a session in a non-Bedrock workspace: %v", err)
	}
}

// Throttling means the check reached no verdict, not that access is missing.
// Refusing sessions over it would be a self-inflicted outage.
func TestCheckPreflightAllowsNonBlockingFailure(t *testing.T) {
	s := newTestServer()
	s.preflights.put(tunnel.PreflightReport{
		WorkspaceID: "w1",
		Blocking:    false, // supervisor already classified it as transient
		Models: []tunnel.PreflightModel{{
			Role: "opus", Required: true, Invocable: false,
			ErrorCode: "ThrottlingException",
		}},
	})
	if err := s.checkPreflight(context.Background(), "w1"); err != nil {
		t.Errorf("refused a session over a transient failure: %v", err)
	}
}

// A report that arrives while session-create is already waiting must unblock it,
// since the check runs asynchronously after the task registers.
func TestCheckPreflightWaitsForALateReport(t *testing.T) {
	s := newTestServer()
	go func() {
		time.Sleep(100 * time.Millisecond)
		s.preflights.put(tunnel.PreflightReport{WorkspaceID: "w1", Blocking: false})
	}()

	start := time.Now()
	if err := s.checkPreflight(context.Background(), "w1"); err != nil {
		t.Fatalf("checkPreflight: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Error("returned before the report arrived; it cannot have waited")
	}
}

// A replacement task must be judged on its own check. Model access is often
// granted after a failure -- that is the recovery path -- so a verdict that
// outlived its task would keep a fixed account looking broken.
func TestForgetDropsAStaleVerdict(t *testing.T) {
	s := newTestServer()
	s.preflights.put(tunnel.PreflightReport{WorkspaceID: "w1", Blocking: true})
	if _, ok := s.preflights.get("w1"); !ok {
		t.Fatal("report was not stored")
	}
	s.preflights.forget("w1")
	if _, ok := s.preflights.get("w1"); ok {
		t.Error("verdict survived the task that produced it")
	}
}
