package ptysession

import (
	"sort"
	"sync"
	"time"
)

// Manager owns every PTY in one workspace task -- one per session (section
// 2.3). The workspace, not the session, owns the task, which is why sessions
// share a filesystem and why the second session in a running workspace starts
// in about a second instead of paying a Fargate cold start.
type Manager struct {
	ringBytes  int
	nudgeDelay time.Duration

	mu       sync.Mutex
	sessions map[string]*Session
	onExit   func(id string, code int)
}

type Options struct {
	// RingBytes bounds the per-session replay buffer.
	RingBytes int
	// NudgeDelay is the gap between the two resize ioctls on attach.
	NudgeDelay time.Duration
	// OnExit is called after a child exits and its attachers are closed. The
	// supervisor forwards it up the tunnel as a session_exited event so the
	// control plane can drive the auto-stop cascade without polling.
	OnExit func(id string, code int)
}

func NewManager(o Options) *Manager {
	if o.RingBytes <= 0 {
		o.RingBytes = defaultRingBytes
	}
	if o.NudgeDelay <= 0 {
		o.NudgeDelay = defaultNudgeDelay
	}
	return &Manager{
		ringBytes:  o.RingBytes,
		nudgeDelay: o.NudgeDelay,
		sessions:   make(map[string]*Session),
		onExit:     o.OnExit,
	}
}

// Create forks a new session. It fails rather than replacing an existing ID:
// a duplicate create would orphan a running Claude Code process.
func (m *Manager) Create(spec Spec) (*Session, error) {
	m.mu.Lock()
	if _, ok := m.sessions[spec.ID]; ok {
		m.mu.Unlock()
		return nil, ErrExists
	}
	m.mu.Unlock()

	s, err := newSession(spec, m.ringBytes, m.nudgeDelay)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	// Re-check: newSession forks outside the lock, so a concurrent Create with
	// the same ID could have won the race.
	if _, ok := m.sessions[spec.ID]; ok {
		m.mu.Unlock()
		_ = s.Stop(true)
		return nil, ErrExists
	}
	m.sessions[spec.ID] = s
	m.mu.Unlock()

	go s.readLoop(m.reap)
	return s, nil
}

func (m *Manager) reap(id string, code int) {
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
	if m.onExit != nil {
		m.onExit(id, code)
	}
}

func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	return s, ok
}

func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// List returns sessions ordered by ID so callers get a stable view.
func (m *Manager) List() []*Session {
	m.mu.Lock()
	out := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (m *Manager) Stop(id string, force bool) error {
	s, ok := m.Get(id)
	if !ok {
		return ErrNotFound
	}
	return s.Stop(force)
}

// StopAll is used on supervisor shutdown.
func (m *Manager) StopAll(force bool) {
	for _, s := range m.List() {
		_ = s.Stop(force)
	}
}
