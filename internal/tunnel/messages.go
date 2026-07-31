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
	MsgAttach        = "attach"        // attach stream: becomes a PTY byte pipe on success
	MsgStartSession  = "start_session" // command stream
	MsgStopSession   = "stop_session"  // command stream
	MsgDeleteSession = "delete_session"
	MsgListSessions  = "list_sessions"
	// Workspace introspection for the operator console. All three are READS and
	// there is deliberately no write counterpart -- read-only is structural here
	// rather than enforced, which is the strongest form it can take.
	MsgListDir       = "list_dir"
	MsgListProcesses = "list_processes"
	MsgResources     = "resources"
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
	MsgPreflight     = "preflight"
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

// StartSession brings a session's process back without attaching a client.
//
// Attach carries Create and could fork the PTY too, but only as a side effect of
// someone connecting. Resume is a distinct verb because the process axis and the
// client-presence axis are independent (2.4): an admin console resuming an
// overnight run, or `ourcli resume`, must be able to start the agent working
// without becoming its terminal.
//
// No size travels here. With no attacher there is no client size to honour, so
// the PTY takes the 24x80 default and the first attach corrects it through the
// resize it already sends -- which also delivers the repaint. That is the one
// case where starting at a default size is right rather than the 4.1 trap.
type StartSession struct {
	SessionID string   `json:"session_id"`
	Cmd       []string `json:"cmd,omitempty"`
	Term      string   `json:"term,omitempty"`
}

// StopSession applies Ctrl-C/Ctrl-D semantics to a running session (2.4).
type StopSession struct {
	SessionID string `json:"session_id"`
	Force     bool   `json:"force,omitempty"` // SIGKILL rather than SIGHUP
}

// DeleteSession ends a session and drops its conversation from the workspace
// volume (2.6). Destructive and not recoverable: the transcript is the session's
// memory, so this is the difference between "stop" and "forget".
type DeleteSession struct {
	SessionID string `json:"session_id"`
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

// ListDir asks for one directory's entries. Workspace-relative; the supervisor
// resolves it against the workspace root and refuses anything that escapes.
//
// Paginated and never recursive, because the tunnel caps a frame at 1 MiB and
// carries live PTY traffic alongside this -- one `node_modules` would exceed
// both the frame and anyone's patience.
type ListDir struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// DirEntry is one file or directory. Metadata only: no contents ride this
// channel, because section 2.7's claim is that we cannot read session content
// and file bodies through the relay would be a plainer contradiction of it than
// the PTY stream ever was.
type DirEntry struct {
	Name     string `json:"name"`
	IsDir    bool   `json:"is_dir"`
	Size     int64  `json:"size"`
	Mode     string `json:"mode"`
	ModTime  string `json:"mod_time"`
	IsSymlink bool  `json:"is_symlink,omitempty"`
}

// DirListing is the reply. Path echoes back the resolved workspace-relative
// path, so a client that asked for ".." learns where it actually landed.
type DirListing struct {
	Path    string     `json:"path"`
	Entries []DirEntry `json:"entries"`
	// Total is the entry count before pagination, so a client can say
	// "1000 of 84213" rather than silently truncating.
	Total     int  `json:"total"`
	Truncated bool `json:"truncated"`
}

// ProcessList reports what is running inside the task, each process attributed
// to the session it descends from. Never carries environments.
type ProcessList struct {
	Processes []ProcessInfo `json:"processes"`
	// Available is false where /proc cannot be read -- the local driver on
	// darwin -- so a client shows "unknown" instead of "nothing is running".
	Available bool `json:"available"`
}

type ProcessInfo struct {
	PID       int     `json:"pid"`
	PPID      int     `json:"ppid"`
	Name      string  `json:"name"`
	State     string  `json:"state"`
	RSSKB     int64   `json:"rss_kb"`
	CPUSecs   float64 `json:"cpu_secs"`
	StartedAt string  `json:"started_at,omitempty"`
	Cmdline   string  `json:"cmdline,omitempty"`
	SessionID string  `json:"session_id,omitempty"`
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

// PreflightReport is the supervisor's Bedrock verdict for its workspace.
//
// It is deliberately structured rather than a pass/fail flag: the control plane
// blocks session creation on it AND an admin has to be able to see, in the
// console, which model failed and what to do about it. "Bedrock is broken" is
// the unhelpful message the whole preflight exists to replace.
//
// It runs in the supervisor because that is the only component holding the
// sandbox task role -- the credential sessions actually use. A check run
// anywhere else would be testing a different principal (section 12.3).
type PreflightReport struct {
	WorkspaceID string `json:"workspace_id"`
	Region      string `json:"region,omitempty"`
	CheckedAt   string `json:"checked_at"`
	// Skipped is set when the workspace is not using Bedrock at all, as in local
	// development against a host login. Absence of a report means "not yet";
	// Skipped means "not applicable", and the two must not be confused.
	Skipped bool `json:"skipped,omitempty"`
	// Blocking is true when a required model failed for a reason that is about
	// configuration. Transient failures (throttling) are reported but do not
	// block, because they say nothing about whether access is set up.
	Blocking bool             `json:"blocking"`
	Models   []PreflightModel `json:"models,omitempty"`
}

type PreflightModel struct {
	Role          string `json:"role"`
	ModelID       string `json:"model_id"`
	Required      bool   `json:"required"`
	Authorization string `json:"authorization"`
	Invocable     bool   `json:"invocable"`
	ErrorCode     string `json:"error_code,omitempty"`
	Error         string `json:"error,omitempty"`
	Advice        string `json:"advice,omitempty"`
}

// FirstBlocker returns the model whose failure should be shown to a user whose
// session was refused.
func (p PreflightReport) FirstBlocker() (PreflightModel, bool) {
	for _, m := range p.Models {
		if m.Required && !m.Invocable {
			return m, true
		}
	}
	return PreflightModel{}, false
}

// Error is the failure reply on any command stream.
type Error struct {
	Message string `json:"message"`
}

// Ref is the reply to StartWorkspace.
type Ref struct {
	Ref string `json:"ref"`
}
