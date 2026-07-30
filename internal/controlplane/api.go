package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
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

	sess := store.Session{
		ID:          "s-" + newSecret()[:12],
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
