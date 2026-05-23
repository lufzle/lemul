package mgmtapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lufzle/lemul/internal/tunnel"
)

// Workspace introspection for the operator console: what is on the disk, what is
// running, and what it is consuming.
//
// The motivation is section 13's largest standing risk -- once the data plane
// lives in customer accounts we cannot reproduce a failure or attach to a wedged
// sandbox. Until now the only way to see inside a task was to attach to a
// session and type, which means co-driving somebody's live Claude Code.
//
// Three properties hold across all of it:
//
//   - READS ONLY, and structurally so: there is no write verb on the other end
//     to call. That is a stronger guarantee than a handler that declines to
//     write, and it is the lesson section 2.5 records from viewer mode, where
//     input-dropping was enforced twice and still had a third door.
//   - NO FILE CONTENTS. Listings carry names, sizes and modes; bodies would put
//     customer source code in cleartext through our relay, which is the exact
//     claim section 2.7 exists to make good on. Adding them later is a decision
//     about E2E, not a feature toggle.
//   - NO COLD START. These use PickWorkspace rather than ensureWorkspace, on the
//     same reasoning as handleListSessions: looking is a read, and nobody should
//     be billed for a Fargate task because a console page was open.

// explorerTimeout is generous relative to the work but bounded: a directory on a
// cold page cache can take a moment, and the caller is a person watching a
// spinner.
const explorerTimeout = 10 * time.Second

// askWorkspace runs one introspection verb and translates the failure modes into
// HTTP the console can act on. Returns false once it has written a response.
//
// It takes an already-authorised wsScope, and that is the whole of Phase 4's
// change here. These handlers touch no rows, so there was nothing to hang a
// permission check on and the only containment was that the registry is keyed
// on (organization, workspace name) -- which meant any member of an
// organization could read the file tree, process list and command lines of
// every workspace in it. Attaching to a session is at least conspicuous; this
// was silent, which is why section 2.5 records it as raising the cost of the
// gap rather than sharing it.
func (s *Server) askWorkspace(w http.ResponseWriter, a wsScope, verb string, req any, out any) bool {
	sc, wid := a.scope, a.WS.Name
	t, err := s.reg.PickWorkspace(sc.TenantID, wid)
	if err != nil {
		// Not an error condition. A stopped workspace has nothing to show, and
		// saying so plainly beats a 500 that reads like a broken control plane.
		http.Error(w, "workspace has no running task", http.StatusConflict)
		return false
	}
	env, err := command(t, verb, req, explorerTimeout)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return false
	}
	if env.Type == tunnel.MsgError {
		var e tunnel.Error
		_ = env.Decode(&e)
		// There is no protocol version handshake, so a supervisor older than
		// these verbs answers "unknown message type ...". That is a capability
		// gap rather than a caller mistake, and 501 says so -- a 500 would send
		// somebody reading control-plane logs for a bug that is not there.
		if strings.HasPrefix(e.Message, "unknown message type") {
			http.Error(w, "this workspace's supervisor does not support the explorer", http.StatusNotImplemented)
			return false
		}
		http.Error(w, e.Message, http.StatusBadRequest)
		return false
	}
	if err := env.Decode(out); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return false
	}
	return true
}

// handleListDir serves one directory of the workspace volume.
//
// The path is workspace-relative and the supervisor is what enforces that --
// see internal/supervisor/fsjail.go. Nothing here validates it, deliberately:
// a check in this process would be a second, weaker copy of a rule that only
// means anything at the point of use.
func (s *Server) handleListDir(w http.ResponseWriter, r *http.Request) {
	a, ok := s.requireWorkspace(w, r)
	if !ok {
		return
	}
	req := tunnel.ListDir{Path: r.URL.Query().Get("path")}
	if v, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && v > 0 {
		req.Offset = v
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		req.Limit = v
	}

	var listing tunnel.DirListing
	if !s.askWorkspace(w, a, tunnel.MsgListDir, req, &listing) {
		return
	}
	if listing.Entries == nil {
		listing.Entries = []tunnel.DirEntry{}
	}
	writeJSON(w, http.StatusOK, listing)
}

// handleListProcesses reports what is running, attributed to sessions.
func (s *Server) handleListProcesses(w http.ResponseWriter, r *http.Request) {
	a, ok := s.requireWorkspace(w, r)
	if !ok {
		return
	}
	var list tunnel.ProcessList
	if !s.askWorkspace(w, a, tunnel.MsgListProcesses, nil, &list) {
		return
	}
	if list.Processes == nil {
		list.Processes = []tunnel.ProcessInfo{}
	}
	writeJSON(w, http.StatusOK, list)
}

// handleResources reports CPU, memory, disk and network for the task.
//
// The rates are computed inside the supervisor from a fixed-interval ring, not
// differenced between two of these calls -- otherwise a backgrounded tab or two
// simultaneous viewers would each produce a different and wrong answer.
func (s *Server) handleResources(w http.ResponseWriter, r *http.Request) {
	a, ok := s.requireWorkspace(w, r)
	if !ok {
		return
	}
	var usage map[string]any
	if !s.askWorkspace(w, a, tunnel.MsgResources, nil, &usage) {
		return
	}
	writeJSON(w, http.StatusOK, usage)
}
