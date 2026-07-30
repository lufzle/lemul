// Package store holds the control plane's durable state.
//
// Phase 1 runs one tenant with hardcoded IDs (section 8), so the backing store
// is a JSON file rather than Postgres. One field genuinely needs the durability
// even at this stage: Workspace.Generation feeds the ECS RunTask client-token,
// so if a control-plane restart reset generations to zero, a retried dispatch
// would reuse a spent token and idempotency would stop protecting anything
// (2.8). Everything else here is a convenience.
//
// The interface exists so Postgres can replace Memory without touching callers.
package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var ErrNotFound = errors.New("not found")

// Workspace status values (the compute axis of section 2.4).
const (
	WorkspaceStopped  = "stopped"
	WorkspaceStarting = "starting"
	WorkspaceActive   = "active"
)

type Workspace struct {
	ID         string `json:"id"`
	TenantID   string `json:"tenant_id"`
	Generation uint64 `json:"generation"`
	TaskRef    string `json:"task_ref,omitempty"`
	Status     string `json:"status"`
}

type Session struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	CreatedAt   time.Time `json:"created_at"`
}

type Store interface {
	GetWorkspace(id string) (Workspace, error)
	PutWorkspace(w Workspace) error
	// NextGeneration increments and persists the workspace's generation,
	// returning the new value. Every placement must go through it.
	NextGeneration(id string) (uint64, error)

	GetSession(id string) (Session, error)
	PutSession(s Session) error
	ListSessions(workspaceID string) ([]Session, error)
}

type state struct {
	Workspaces map[string]Workspace `json:"workspaces"`
	Sessions   map[string]Session   `json:"sessions"`
}

// Memory is an in-process store, optionally persisted to a JSON file.
type Memory struct {
	path string

	mu sync.Mutex
	st state
}

// NewMemory returns a store. If path is non-empty it is loaded on open and
// rewritten on every mutation.
func NewMemory(path string) (*Memory, error) {
	m := &Memory{
		path: path,
		st: state{
			Workspaces: make(map[string]Workspace),
			Sessions:   make(map[string]Session),
		},
	}
	if path == "" {
		return m, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &m.st); err != nil {
		return nil, err
	}
	if m.st.Workspaces == nil {
		m.st.Workspaces = make(map[string]Workspace)
	}
	if m.st.Sessions == nil {
		m.st.Sessions = make(map[string]Session)
	}
	return m, nil
}

// flush writes the state out. Callers hold the lock. Write-then-rename so a
// crash mid-write cannot leave a truncated file where the generation counter
// lives.
func (m *Memory) flush() error {
	if m.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(m.st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}

func (m *Memory) GetWorkspace(id string) (Workspace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.st.Workspaces[id]
	if !ok {
		return Workspace{}, ErrNotFound
	}
	return w, nil
}

func (m *Memory) PutWorkspace(w Workspace) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.st.Workspaces[w.ID] = w
	return m.flush()
}

func (m *Memory) NextGeneration(id string) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.st.Workspaces[id]
	if !ok {
		return 0, ErrNotFound
	}
	w.Generation++
	m.st.Workspaces[id] = w
	if err := m.flush(); err != nil {
		return 0, err
	}
	return w.Generation, nil
}

func (m *Memory) GetSession(id string) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.st.Sessions[id]
	if !ok {
		return Session{}, ErrNotFound
	}
	return s, nil
}

func (m *Memory) PutSession(s Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.st.Sessions[s.ID] = s
	return m.flush()
}

func (m *Memory) ListSessions(workspaceID string) ([]Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Session
	for _, s := range m.st.Sessions {
		if s.WorkspaceID == workspaceID {
			out = append(out, s)
		}
	}
	return out, nil
}
