package tunnel

// Message types on the two tunnels.
//
// Both the runner and the supervisor dial OUT and hold a yamux session; they
// differ only in which verbs they accept. Stream direction is fixed by
// convention:
//
//	control plane -> agent   one stream per command (or per attach)
//	agent -> control plane   ONE long-lived event stream, opened at registration
//
// That split is why no message carries a correlation ID: a command stream has
// exactly one request and one reply, and events are unsolicited by definition.
const (
	// Control plane -> runner (command stream).
	MsgStartWorkspace = "start_workspace"
	MsgStopWorkspace  = "stop_workspace"

	// Control plane -> supervisor.
	MsgAttach       = "attach"       // attach stream: becomes a PTY byte pipe on success
	MsgStopSession  = "stop_session" // command stream
	MsgListSessions = "list_sessions"
	// MsgResize travels on an established attach stream, not a command stream.
	// Terminal size cannot ride in the byte stream, so it needs its own channel:
	// the supervisor turns it into ioctl(TIOCSWINSZ) -> SIGWINCH.
	MsgResize = "resize"

	// Replies (agent -> control plane, on the same stream).
	MsgOK    = "ok"
	MsgError = "error"

	// Agent -> control plane (event stream).
	MsgHeadroom      = "headroom"
	MsgSessionExited = "session_exited"
)

// StartWorkspace asks the runner to place one workspace task.
//
// Generation is what makes the dispatch idempotent: the runner derives the ECS
// RunTask --client-token from WorkspaceID+Generation, so a dispatch retried
// after a timeout cannot produce two tasks for one workspace (section 2.8).
// Credential is workspace-scoped and short-TTL; the supervisor presents it when
// it dials out, and the relay binds it to the task identity on first connect.
type StartWorkspace struct {
	TenantID     string            `json:"tenant_id"`
	WorkspaceID  string            `json:"workspace_id"`
	Generation   uint64            `json:"generation"`
	Credential   string            `json:"credential"`
	ControlPlane string            `json:"control_plane"` // ws:// URL the supervisor dials
	Image        string            `json:"image,omitempty"`
	CPU          int               `json:"cpu,omitempty"`
	MemoryMB     int               `json:"memory_mb,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
}

// StopWorkspace stops a placed task. Ref is opaque to the control plane -- a
// PID for the local driver, a task ARN for ECS.
type StopWorkspace struct {
	WorkspaceID string `json:"workspace_id"`
	Ref         string `json:"ref"`
}

// Attach is the first message on an attach stream. On success the stream
// becomes a byte pipe for that session's PTY.
//
// The size must arrive before the supervisor forks: a PTY defaults to 0x0, and
// a resize sent afterwards is too late -- Claude Code has already drawn its
// first frame into a zero-size terminal (section 4.1).
//
// Mode "viewer" is input-dropping. The relay enforces it too, but the
// supervisor is the last line of defence: both attachers write the same PTY
// stdin, so a viewer that can write is silently a co-driver (section 2.5).
type Attach struct {
	SessionID string   `json:"session_id"`
	Rows      uint16   `json:"rows"`
	Cols      uint16   `json:"cols"`
	Mode      string   `json:"mode,omitempty"` // "control" (default) | "viewer"
	Create    bool     `json:"create,omitempty"`
	Cmd       []string `json:"cmd,omitempty"` // only when Create
	Term      string   `json:"term,omitempty"`
}

const (
	ModeControl = "control"
	ModeViewer  = "viewer"
)

// AttachOK is the reply to Attach. Created distinguishes a fresh PTY from a
// reattach, which is what lets the client tell the user which one happened.
type AttachOK struct {
	SessionID string `json:"session_id"`
	Created   bool   `json:"created"`
}

// StopSession applies Ctrl-C/Ctrl-D semantics to a running session (2.4).
type StopSession struct {
	SessionID string `json:"session_id"`
	Force     bool   `json:"force,omitempty"` // SIGKILL rather than SIGHUP
}

// SessionList is the reply to MsgListSessions.
type SessionList struct {
	Sessions []SessionInfo `json:"sessions"`
}

type SessionInfo struct {
	ID        string `json:"id"`
	Rows      uint16 `json:"rows"`
	Cols      uint16 `json:"cols"`
	Attachers int    `json:"attachers"`
	StartedAt string `json:"started_at"`
}

// Headroom is the supervisor's honest report for admission control. Fargate
// task size is fixed at launch, so the control plane must refuse a session
// rather than let an OOM kill every session in the workspace (section 2.4).
type Headroom struct {
	WorkspaceID string `json:"workspace_id"`
	MemFreeMB   int    `json:"mem_free_mb"`
	MemLimitMB  int    `json:"mem_limit_mb"`
	Sessions    int    `json:"sessions"`
}

// SessionExited reports a child process ending, so the control plane can drive
// the auto-stop cascade without polling.
type SessionExited struct {
	WorkspaceID string `json:"workspace_id"`
	SessionID   string `json:"session_id"`
	ExitCode    int    `json:"exit_code"`
}

// Resize carries a terminal size change on an attach stream.
type Resize struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

// Error is the failure reply on any command stream.
type Error struct {
	Message string `json:"message"`
}

// Ref is the reply to StartWorkspace.
type Ref struct {
	Ref string `json:"ref"`
}
