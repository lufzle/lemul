package mgmtapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/lufzle/lemul/internal/store"
	"github.com/lufzle/lemul/internal/tunnel"
)

// The session-process axis of section 2.4. Compute, process and client presence
// are independent, and these handlers move exactly one of them: the process.
// Attaching and detaching a client is handleAttach's business, and starting or
// stopping the workspace task is the compute axis -- conflating any two produces
// the wrong behaviour for a real case, which is why stop below refuses to touch
// the task and resume is allowed to start it.

// handleStopSession applies Ctrl-C/Ctrl-D semantics (section 2.4). The
// conversation survives on the workspace volume, so this is an interruption, not
// data loss -- and resume picks the history back up.
//
// It deliberately does NOT start the workspace. A stopped workspace holds no
// processes, so the session is already in the state the caller asked for, and
// paying a 20-60 s Fargate cold start to deliver a signal to nothing would be
// absurd. Same reasoning as handleListSessions.
func (s *Server) handleStopSession(w http.ResponseWriter, r *http.Request) {
	// Administering a session follows the WORKSPACE, unlike attaching to one:
	// its owner may stop anything running in it, whoever started it. Everyone
	// else can only reach their own, and requireSession has already turned
	// anything else into a 404 (section 2.5).
	a, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	sc, sess, sid := a.scope, a.Sess, a.Sess.ID

	force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"

	t, _, err := s.tunnelForSession(r.Context(), sc, sess)
	if err != nil {
		// Workspace not running: nothing to stop, and that is success rather than
		// an error. Stop has to be idempotent -- a UI button and a retried request
		// must both land on "it is stopped".
		writeJSON(w, http.StatusOK, sessionStateResponse{ID: sid, Status: SessionStopped})
		return
	}

	env, err := command(t, tunnel.MsgStopSession, tunnel.StopSession{
		SessionID: sid,
		Force:     force,
	}, 15*time.Second)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	if env.Type == tunnel.MsgError {
		// The supervisor answers "session not found" when the PTY is already gone.
		// That is the same idempotent success as above, not a failure.
		var e tunnel.Error
		_ = env.Decode(&e)
		s.log.Warn("stopping the session failed", "session", sid, "error", e.Message)
	}
	s.log.Info("session stopped", "session", sid, "force", force)
	writeJSON(w, http.StatusOK, sessionStateResponse{ID: sid, Status: SessionStopped})
}

type sessionStateResponse struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	Status      string `json:"status"`
}

type patchSessionRequest struct {
	// IdleTimeoutSecs is section 2.4's per-session override, and the reason it
	// is a pointer to a pointer's worth of meaning: absent leaves it alone, null
	// clears it back to inheriting the workspace, and 0 is the explicit opt-out
	// for a session that is MEANT to sit there. Three distinct states, none of
	// which a plain int can express.
	IdleTimeoutSecs  *int `json:"idle_timeout_secs,omitempty"`
	ClearIdleTimeout bool `json:"inherit_idle_timeout,omitempty"`
}

// handlePatchSession sets a session's idle-timeout override (section 2.4).
//
// Authorised like stop rather than like attach: administering a session follows
// the WORKSPACE, so its owner may change anything running in it, while everyone
// else reaches only their own and requireSession has already made anything else
// a 404.
func (s *Server) handlePatchSession(w http.ResponseWriter, r *http.Request) {
	a, ok := s.requireSession(w, r)
	if !ok {
		return
	}

	var req patchSessionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.IdleTimeoutSecs == nil && !req.ClearIdleTimeout {
		http.Error(w, `send "idle_timeout_secs" or "inherit_idle_timeout"`, http.StatusBadRequest)
		return
	}
	var secs *int32
	if !req.ClearIdleTimeout {
		if *req.IdleTimeoutSecs < 0 {
			http.Error(w, `"idle_timeout_secs" cannot be negative; 0 disables the idle stop`,
				http.StatusBadRequest)
			return
		}
		v := int32(*req.IdleTimeoutSecs)
		secs = &v
	}

	sess, err := s.setSessionIdleTimeout(r.Context(), a.Scope, a.Sess.ID, secs)
	if err != nil {
		s.log.Error("setting a session idle timeout", "session", a.Sess.ID, "error", err)
		http.Error(w, "could not change the idle timeout", http.StatusInternalServerError)
		return
	}
	s.log.Info("session idle timeout changed", "session", sess.ID,
		"secs", secs, "by", a.UserID)
	doc := sessionDoc{
		ID:        sess.ID,
		Status:    SessionStopped,
		CreatedAt: timestamp(sess.CreatedAt),
		Owner:     sess.UserID,
		Mine:      sess.UserID == a.UserID,
	}
	if sess.IdleTimeoutSecs != nil {
		doc.IdleTimeoutSecs = ptr(int(*sess.IdleTimeoutSecs))
	}
	writeJSON(w, http.StatusOK, doc)
}

// handleResumeSession brings a session's process back, starting the workspace
// first if it is stopped (section 2.4: a `stopped` workspace with a `running`
// session is impossible, so resuming the session implies resuming the compute).
//
// Whether this actually resumes a conversation or starts a fresh one is not
// decided here. The supervisor picks --resume or --session-id from the transcript
// on the workspace volume, because a control-plane flag would go stale the moment
// a replacement task came up with a fresh disk (see supervisor/claudeargs.go).
func (s *Server) handleResumeSession(w http.ResponseWriter, r *http.Request) {
	a, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	sc, sess, sid := a.scope, a.Sess, a.Sess.ID

	ctx, cancel := context.WithTimeout(r.Context(), s.opt.StartTimeout)
	defer cancel()

	wsName := a.WS.Name
	t, err := s.ensureWorkspace(ctx, sc, a.WS)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	// The same gate as session-create and attach. Resume forks a Claude Code
	// process just as they do, so letting it through would just move the failure
	// to the user's first prompt (section 12.3).
	if err := s.checkPreflight(ctx, sc, wsName); err != nil {
		s.log.Warn("refusing resume", "session", sid, "error", err)
		http.Error(w, err.Error(), http.StatusFailedDependency)
		return
	}

	// The other route that forks a PTY, and therefore the other place the task
	// has to be told who the session runs as. Resume attaches no client, so
	// endpoint negotiation never runs for it -- the identity would otherwise
	// arrive only when somebody later connected, which is after the process
	// exists.
	//
	// sess.UserID, not the caller: a workspace owner may resume somebody else's
	// session, and it goes on being theirs.
	if err := s.prepareSession(ctx, a.wsScope, sid, sess.UserID); err != nil {
		s.log.Error("resolving the session's identity", "session", sid, "error", err)
		http.Error(w, "could not resolve who this session runs as",
			http.StatusInternalServerError)
		return
	}

	// No command here either. Resume and attach-create fork the same program
	// because the task was told which one at placement (workspaceEnv), rather
	// than because two call sites were kept in step.
	env, err := command(t, tunnel.MsgStartSession, tunnel.StartSession{
		SessionID: sid,
	}, 30*time.Second)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	if env.Type == tunnel.MsgError {
		var e tunnel.Error
		_ = env.Decode(&e)
		http.Error(w, e.Message, http.StatusConflict)
		return
	}
	s.log.Info("session resumed", "session", sid, "workspace", sess.WorkspaceID)
	writeJSON(w, http.StatusOK, sessionStateResponse{
		ID:          sid,
		WorkspaceID: sess.WorkspaceID,
		Status:      SessionRunning,
	})
}

// handleDeleteSession ends a session and drops its conversation (section 2.6).
//
// Destructive in a way stop is not: stop interrupts and resume recovers, but the
// transcript IS the session's memory, so this is unrecoverable. It therefore
// refuses a session that is still running unless ?force=1 -- a running session is
// one someone may be attached to, and "delete" arriving from a mis-click should
// not silently kill a live agent run and burn its history.
//
// The record is dropped even when the workspace is stopped and the transcript
// cannot be reached. Leaving the record behind would leave a session a user can
// see, try to resume, and get nothing from; the alternative -- refusing to delete
// until the workspace is started -- would mean paying a cold start to forget
// something.
func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	// Resolved by hand rather than through requireSession, for one reason: a
	// session that is already gone must answer 204, not 404. Delete is
	// idempotent -- a retried request has to land on the outcome it asked for --
	// and requireSession cannot tell that case from "you may not see this".
	sc, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	sid := r.PathValue("sid")
	sess, err := s.getSession(r.Context(), sc, sid)
	if errors.Is(err, store.ErrNotFound) {
		w.WriteHeader(http.StatusNoContent) // already gone
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a, err := s.sessionAccess(r.Context(), sc, sess)
	if err != nil || !a.isVisible() {
		// Indistinguishable from a session that never existed, which is also
		// what a plain member's listing says about it.
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}

	force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"

	if t, wsName, terr := s.tunnelForSession(r.Context(), sc, sess); terr == nil {
		if !force {
			if info, live := s.liveSessions(sc, wsName)[sid]; live {
				msg := "session is still running"
				if info.Attachers > 0 {
					msg += " with " + strconv.Itoa(info.Attachers) + " attached client(s)"
				}
				http.Error(w, msg+"; stop it first, or pass ?force=1 to end it and drop the conversation",
					http.StatusConflict)
				return
			}
		}
		env, cerr := command(t, tunnel.MsgDeleteSession, tunnel.DeleteSession{SessionID: sid}, 30*time.Second)
		if cerr != nil {
			http.Error(w, cerr.Error(), http.StatusServiceUnavailable)
			return
		}
		if env.Type == tunnel.MsgError {
			var e tunnel.Error
			_ = env.Decode(&e)
			http.Error(w, e.Message, http.StatusInternalServerError)
			return
		}
	} else if !force {
		// No tunnel, so the conversation cannot be dropped from the volume. Say so
		// rather than deleting the record and leaving an orphan transcript that a
		// future session id could never reach but that still occupies the disk.
		http.Error(w,
			"workspace is not running, so the conversation cannot be dropped; pass ?force=1 to delete the record anyway",
			http.StatusConflict)
		return
	}

	if err := s.deleteSession(r.Context(), sc, sid); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.log.Info("session deleted", "session", sid, "workspace", sess.WorkspaceID, "force", force)
	w.WriteHeader(http.StatusNoContent)
}
