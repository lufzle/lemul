package controlplane

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/lufzle/lemul-cc/internal/store"
	"github.com/lufzle/lemul-cc/internal/tunnel"
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
	sid := r.PathValue("sid")
	sess, err := s.st.GetSession(sid)
	if err != nil {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}

	force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"

	t, err := s.reg.PickWorkspace(sess.WorkspaceID)
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
		log.Printf("stop session %s: %s", sid, e.Message)
	}
	log.Printf("session %s stopped (force=%v)", sid, force)
	writeJSON(w, http.StatusOK, sessionStateResponse{ID: sid, Status: SessionStopped})
}

type sessionStateResponse struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	Status      string `json:"status"`
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
	sid := r.PathValue("sid")
	sess, err := s.st.GetSession(sid)
	if err != nil {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.opt.StartTimeout)
	defer cancel()

	t, err := s.ensureWorkspace(ctx, sess.WorkspaceID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	// The same gate as session-create and attach. Resume forks a Claude Code
	// process just as they do, so letting it through would just move the failure
	// to the user's first prompt (section 12.3).
	if err := s.checkPreflight(ctx, sess.WorkspaceID); err != nil {
		log.Printf("refusing resume of %s: %v", sid, err)
		http.Error(w, err.Error(), http.StatusFailedDependency)
		return
	}

	env, err := command(t, tunnel.MsgStartSession, tunnel.StartSession{
		SessionID: sid,
		Cmd:       s.opt.SessionCmd,
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
	log.Printf("session %s resumed in workspace %s", sid, sess.WorkspaceID)
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
	sid := r.PathValue("sid")
	sess, err := s.st.GetSession(sid)
	if errors.Is(err, store.ErrNotFound) {
		w.WriteHeader(http.StatusNoContent) // already gone
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"

	if t, terr := s.reg.PickWorkspace(sess.WorkspaceID); terr == nil {
		if !force {
			if info, ok := s.liveSessions(sess.WorkspaceID)[sid]; ok {
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

	if err := s.st.DeleteSession(sid); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("session %s deleted from workspace %s (force=%v)", sid, sess.WorkspaceID, force)
	w.WriteHeader(http.StatusNoContent)
}
