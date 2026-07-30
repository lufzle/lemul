package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lufzle/lemul-cc/internal/registry"
	"github.com/lufzle/lemul-cc/internal/store"
	"github.com/lufzle/lemul-cc/internal/tunnel"
)

func (s *Server) lockWorkspace(wid string) *sync.Mutex {
	v, _ := s.ensureLocks.LoadOrStore(wid, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

type createSessionResponse struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	Status      string `json:"status"`
}

// handleCreateSession creates a session, starting the workspace task if it is
// not already running (section 2.6: creating a session in a stopped workspace
// implicitly starts the workspace).
//
// It answers 202 rather than 200 because placement is asynchronous by nature --
// a Fargate cold start is 20-60 s.
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	wid := r.PathValue("wid")

	ctx, cancel := context.WithTimeout(r.Context(), s.opt.StartTimeout)
	defer cancel()

	if _, err := s.ensureWorkspace(ctx, wid); err != nil {
		log.Printf("ensure workspace %s: %v", wid, err)
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	// Refuse rather than let the failure be discovered inside the PTY. A session
	// admitted into a workspace whose Bedrock is misconfigured starts Claude Code
	// successfully and then dies on the user's first prompt, with an error that
	// says nothing about the First Time Use form (sections 8, 13).
	if err := s.checkPreflight(ctx, wid); err != nil {
		log.Printf("refusing session in %s: %v", wid, err)
		http.Error(w, err.Error(), http.StatusFailedDependency)
		return
	}

	sess := store.Session{
		ID:          newSessionID(),
		WorkspaceID: wid,
		CreatedAt:   time.Now().UTC(),
	}
	if err := s.st.PutSession(sess); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("session %s created in workspace %s", sess.ID, wid)

	writeJSON(w, http.StatusAccepted, createSessionResponse{
		ID:          sess.ID,
		WorkspaceID: wid,
		Status:      "created",
	})
}

// SessionStatus values on the session-process axis of section 2.4. The session
// record is durable; the process is not, so a session can exist without one.
const (
	SessionRunning = "running"
	SessionStopped = "stopped"
)

type sessionDoc struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
	Attachers int    `json:"attachers"`
	Rows      uint16 `json:"rows,omitempty"`
	Cols      uint16 `json:"cols,omitempty"`
}

type sessionListResponse struct {
	WorkspaceID string       `json:"workspace_id"`
	Sessions    []sessionDoc `json:"sessions"`
}

// handleListSessions reports the sessions in a workspace, newest first.
//
// It deliberately does NOT start the workspace: listing is a read, and a user
// asking what exists should not be billed for a Fargate task. If the task is not
// running, every session is reported stopped -- which is honest, because the
// records outlive the processes.
//
// Liveness comes from the supervisor rather than from our own bookkeeping. It is
// the only component that knows whether a PTY still exists, and asking it is
// what keeps this from drifting into a second source of truth.
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	wid := r.PathValue("wid")

	records, err := s.st.ListSessions(wid)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	live := s.liveSessions(wid)

	docs := make([]sessionDoc, 0, len(records))
	for _, rec := range records {
		d := sessionDoc{
			ID:        rec.ID,
			Status:    SessionStopped,
			CreatedAt: rec.CreatedAt.UTC().Format(time.RFC3339),
		}
		if info, ok := live[rec.ID]; ok {
			d.Status = SessionRunning
			d.Attachers = info.Attachers
			d.Rows, d.Cols = info.Rows, info.Cols
		}
		docs = append(docs, d)
	}
	// Newest first: the thing a user most likely wants back is the last one they
	// were using.
	sort.Slice(docs, func(i, j int) bool { return docs[i].CreatedAt > docs[j].CreatedAt })

	writeJSON(w, http.StatusOK, sessionListResponse{WorkspaceID: wid, Sessions: docs})
}

// liveSessions asks the workspace task which PTYs it still has. Returns nil when
// the workspace is not running, which is not an error.
func (s *Server) liveSessions(wid string) map[string]tunnel.SessionInfo {
	t, err := s.reg.PickWorkspace(wid)
	if err != nil {
		return nil
	}
	env, err := command(t, tunnel.MsgListSessions, nil, 10*time.Second)
	if err != nil || env.Type == tunnel.MsgError {
		log.Printf("list sessions on %s: %v", wid, err)
		return nil
	}
	var list tunnel.SessionList
	if err := env.Decode(&list); err != nil {
		return nil
	}
	out := make(map[string]tunnel.SessionInfo, len(list.Sessions))
	for _, info := range list.Sessions {
		out[info.ID] = info
	}
	return out
}

// ensureWorkspace returns a live tunnel to the workspace task, placing the task
// first if necessary.
func (s *Server) ensureWorkspace(ctx context.Context, wid string) (*registry.Tunnel, error) {
	mu := s.lockWorkspace(wid)
	mu.Lock()
	defer mu.Unlock()

	if t, err := s.reg.PickWorkspace(wid); err == nil {
		return t, nil
	}

	ws, err := s.st.GetWorkspace(wid)
	if errors.Is(err, store.ErrNotFound) {
		// Phase 1 has one tenant with hardcoded IDs, so an unknown workspace is
		// created on demand rather than 404ing. Phase 3 replaces this with the
		// real create API and permission checks (section 2.5).
		ws = store.Workspace{ID: wid, TenantID: s.opt.TenantID, Status: store.WorkspaceStopped}
		if err := s.st.PutWorkspace(ws); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}

	runner, err := s.reg.PickRunner(ws.TenantID)
	if err != nil {
		return nil, fmt.Errorf("no runner connected for tenant %s", ws.TenantID)
	}

	// A new generation for every placement: it is what the idempotency key is
	// derived from, and it invalidates the previous task's tunnel credential.
	gen, err := s.st.NextGeneration(wid)
	if err != nil {
		return nil, err
	}
	cred := s.creds.mintWorkspace(wid)

	ws.Status = store.WorkspaceStarting
	_ = s.st.PutWorkspace(ws)

	env, err := command(runner, tunnel.MsgStartWorkspace, tunnel.StartWorkspace{
		TenantID:     ws.TenantID,
		WorkspaceID:  wid,
		Generation:   gen,
		Credential:   cred,
		ControlPlane: s.agentURL(),
		Image:        s.opt.Image,
		Env:          s.workspaceEnv(),
	}, 60*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dispatch: %w", err)
	}
	if env.Type == tunnel.MsgError {
		var e tunnel.Error
		_ = env.Decode(&e)
		return nil, fmt.Errorf("runner refused: %s", e.Message)
	}
	var ref tunnel.Ref
	_ = env.Decode(&ref)

	ws.TaskRef = ref.Ref
	_ = s.st.PutWorkspace(ws)
	log.Printf("workspace %s gen %d dispatched to runner %s: ref=%s", wid, gen, runner.ID, ref.Ref)

	t, err := s.reg.WaitWorkspace(ctx, wid)
	if err != nil {
		return nil, fmt.Errorf("workspace %s did not dial in: %w", wid, err)
	}
	ws.Status = store.WorkspaceActive
	_ = s.st.PutWorkspace(ws)
	return t, nil
}

// workspaceEnv is the configuration a workspace task needs, delivered as
// environment rather than as command-line flags: it is the one mechanism that
// works identically for a local child process and an ECS task definition, so
// the two drivers cannot drift apart on how a task is configured.
func (s *Server) workspaceEnv() map[string]string {
	env := map[string]string{}
	if s.opt.GatewayURL != "" {
		env["LEMUL_GATEWAY_URL"] = s.opt.GatewayURL
		// Delivered to the task, never to a session: the supervisor strips it
		// from every child environment (see strippedFromChild).
		env["LEMUL_GATEWAY_KEY"] = s.opt.GatewayKey
	}
	if s.opt.OTelEndpoint != "" {
		env["LEMUL_OTEL_ENDPOINT"] = s.opt.OTelEndpoint
		if s.opt.OTelHeaders != "" {
			env["LEMUL_OTEL_HEADERS"] = s.opt.OTelHeaders
		}
		if s.opt.OTelProtocol != "" {
			env["LEMUL_OTEL_PROTOCOL"] = s.opt.OTelProtocol
		}
		if s.opt.OTelTraces {
			env["LEMUL_OTEL_TRACES"] = "1"
		}
	}
	if s.opt.BedrockPreflight {
		env["LEMUL_BEDROCK_PREFLIGHT"] = "1"
	}
	if s.opt.Region != "" {
		env["AWS_REGION"] = s.opt.Region
	}
	if s.opt.Pins != "" {
		// Consumed twice inside the task: the entrypoint renders them into
		// managed settings, and the preflight checks the same value. One setting,
		// so the two cannot drift.
		env["LEMUL_PINS"] = s.opt.Pins
	}
	return env
}

type endpointResponse struct {
	Transport  string `json:"transport"`
	Address    string `json:"address"`
	Credential string `json:"credential"`
	PeerPubkey string `json:"peer_pubkey,omitempty"`
}

// handleEndpoint tells the client where to connect.
//
// v0.1 only ever answers "relay". The indirection exists anyway because
// hardcoding the relay into the client makes E2E, `direct` and `tailnet` a
// rewrite rather than a per-tenant config change (section 2.7). peer_pubkey is
// empty until E2E lands; the field is in the contract from the start so adding
// it does not break older clients.
func (s *Server) handleEndpoint(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")
	sess, err := s.st.GetSession(sid)
	if err != nil {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, endpointResponse{
		Transport:  "relay",
		Address:    s.clientURL(r) + "/v1/sessions/" + sess.ID + "/attach",
		Credential: s.creds.mintAttach(sess.ID, 2*time.Minute),
	})
}

// agentURL is the base URL a placed workspace task dials back to. Unlike
// clientURL it cannot fall back to the request host: the request that triggers
// placement comes from the client, whose view of the control plane is not
// necessarily the task's.
func (s *Server) agentURL() string {
	return strings.TrimRight(s.opt.PublicURL, "/")
}

// clientURL is the base URL the client dials. It follows the request host so a
// developer hitting 127.0.0.1 is not handed a URL for something else.
func (s *Server) clientURL(r *http.Request) string {
	if s.opt.PublicURL != "" {
		return strings.TrimRight(s.opt.PublicURL, "/")
	}
	return "ws://" + r.Host
}
