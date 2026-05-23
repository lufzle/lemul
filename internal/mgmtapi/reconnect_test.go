package mgmtapi

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/lufzle/lemul/internal/agent"
	"github.com/lufzle/lemul/internal/registry"
	"github.com/lufzle/lemul/internal/tunnel"
)

// fakeTunnel is a registered tunnel over an in-memory pipe. Enough to be picked
// out of the registry, which is all these tests need.
func fakeTunnel(t *testing.T, org, wid string) *registry.Tunnel {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })

	// The far end answers OK to anything.
	//
	// It used to be a bare yamux.Client that accepted nothing, which made every
	// control-tunnel command sent to a fake tunnel wait out its whole timeout --
	// two tests quietly cost ten seconds each the first time a handler started
	// sending one. A fake that never replies is not a cheaper supervisor, it is a
	// wedged one.
	go func() {
		cs, err := yamux.Client(b, agent.YamuxConfig())
		if err != nil {
			return
		}
		for {
			stream, err := cs.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = stream.Close() }()
				env, err := tunnel.ReadMsg(stream)
				if err != nil {
					return
				}
				_ = tunnel.WriteMsg(stream, tunnel.MsgOK,
					tunnel.SessionInfo{ID: env.Type})
			}()
		}
	}()
	sess, err := yamux.Server(a, agent.YamuxConfig())
	if err != nil {
		t.Fatalf("yamux: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	// The organization is part of the registry key now, so a tunnel registered
	// under the wrong one is simply not found -- which would look like the
	// reconnect never happened.
	return registry.NewTunnel(wid, org, wid, "pipe", sess)
}

// placedWorkspace creates a workspace that already has a task, at a chosen
// generation -- the state a task's tunnel dropping leaves behind.
func placedWorkspace(t *testing.T, s *testEnv, sc scope, name string, gen int64) Workspace {
	t.Helper()
	ctx := context.Background()
	ws, err := s.createWorkspace(ctx, sc, name, accessOwner)
	if err != nil {
		t.Fatalf("createWorkspace: %v", err)
	}
	for range gen {
		if _, err := s.nextGeneration(ctx, sc, ws.ID); err != nil {
			t.Fatalf("nextGeneration: %v", err)
		}
	}
	if err := s.setWorkspaceStatus(ctx, sc, ws.ID, WorkspaceActive,
		"arn:aws:ecs:us-east-2:1:task/c/live"); err != nil {
		t.Fatalf("setWorkspaceStatus: %v", err)
	}
	ws, err = s.getWorkspaceByName(ctx, sc, name)
	if err != nil {
		t.Fatalf("re-read workspace: %v", err)
	}
	return ws
}

func generationOf(t *testing.T, s *testEnv, sc scope, name string) int64 {
	t.Helper()
	ws, err := s.getWorkspaceByName(context.Background(), sc, name)
	if err != nil {
		t.Fatalf("getWorkspaceByName: %v", err)
	}
	return ws.Generation
}

// The partition kill, as a test.
//
// A healthy task's tunnel drops for a moment -- a blip, an ALB rotation, a relay
// redeploy -- and the user retries. That retry used to take a new generation,
// which invalidated the credential the live task was about to reconnect with, so
// the task was answered 401 and exited by design, killing the unattended run.
// The workspace could not be placed either, because the driver adopted that same
// still-running task.
//
// Nothing here may advance the generation.
func TestAReconnectingTaskIsNotReplaced(t *testing.T) {
	s, sc := testServer(t)
	s.opt.ReconnectGrace = 2 * time.Second
	ws := placedWorkspace(t, s, sc, "w1", 5)

	// The supervisor's retry, arriving while ensureWorkspace waits.
	go func() {
		time.Sleep(100 * time.Millisecond)
		s.reg.AddWorkspace(fakeTunnel(t, sc.TenantID, "w1"))
	}()

	got, err := s.ensureWorkspace(context.Background(), sc, ws)
	if err != nil {
		t.Fatalf("ensureWorkspace: %v", err)
	}
	if got == nil {
		t.Fatal("no tunnel returned")
	}

	if got := generationOf(t, s, sc, "w1"); got != 5 {
		t.Fatalf("generation moved to %d; the live task's credential has just been "+
			"invalidated and its sessions will die", got)
	}
}

// The grace is bounded: a task that is genuinely gone must not stall placement
// forever. Getting as far as "no runner connected" proves the wait ended and a
// real placement was attempted.
func TestAGoneTaskIsReplacedAfterTheGrace(t *testing.T) {
	s, sc := testServer(t)
	s.opt.ReconnectGrace = 50 * time.Millisecond
	ws := placedWorkspace(t, s, sc, "w1", 5)

	start := time.Now()
	_, err := s.ensureWorkspace(context.Background(), sc, ws)
	if err == nil {
		t.Fatal("expected placement to fail with no runner connected")
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("returned after %v, before the grace elapsed", elapsed)
	}
}

// A workspace that has never been placed has nothing to wait for, so it must go
// straight to placement rather than paying the grace on a first connect.
func TestAWorkspaceWithNoTaskDoesNotWait(t *testing.T) {
	s, sc := testServer(t)
	s.opt.ReconnectGrace = 10 * time.Second
	// Created but never placed: ensureWorkspace no longer creates one, so the
	// record has to exist before it is asked for a tunnel.
	ws, err := s.createWorkspace(context.Background(), sc, "fresh", accessOwner)
	if err != nil {
		t.Fatalf("createWorkspace: %v", err)
	}

	start := time.Now()
	if _, err := s.ensureWorkspace(context.Background(), sc, ws); err == nil {
		t.Fatal("expected placement to fail with no runner connected")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("waited %v for a task that was never placed", elapsed)
	}
}

// A live tunnel short-circuits everything: no wait, no placement, no generation
// change. This is the common path and the cheapest one.
func TestALiveTunnelIsReturnedImmediately(t *testing.T) {
	s, sc := testServer(t)
	s.opt.ReconnectGrace = 10 * time.Second
	ws := placedWorkspace(t, s, sc, "w1", 5)
	s.reg.AddWorkspace(fakeTunnel(t, sc.TenantID, "w1"))

	start := time.Now()
	if _, err := s.ensureWorkspace(context.Background(), sc, ws); err != nil {
		t.Fatalf("ensureWorkspace: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("took %v with a live tunnel already registered", elapsed)
	}
	if got := generationOf(t, s, sc, "w1"); got != 5 {
		t.Fatalf("generation moved to %d with a live tunnel", got)
	}
}
