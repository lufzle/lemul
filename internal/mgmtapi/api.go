package mgmtapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lufzle/lemul/internal/creds"
	"github.com/lufzle/lemul/internal/registry"
	"github.com/lufzle/lemul/internal/tunnel"
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
	// Any member of the workspace may start a session in it; a workspace nobody
	// added them to is a 404, as is one that does not exist. A workspace is no
	// longer conjured here either -- see ensureWorkspace.
	a, ok := s.requireWorkspace(w, r)
	if !ok {
		return
	}
	sc, wid := a.scope, a.WS.Name

	ctx, cancel := context.WithTimeout(r.Context(), s.opt.StartTimeout)
	defer cancel()

	// Admission runs BEFORE placement, for two reasons. A cold workspace has no
	// report, which reads as "nothing is running, therefore there is room" and
	// admits -- so nothing is lost by checking first. And a refusal should not
	// have cost the customer a Fargate cold start on the way to being refused,
	// which is the rule section 12.9 already applies to authorisation.
	if v := s.admitSession(sc, a.WS); !v.Admit {
		s.log.Warn("refusing session: admission", "org", sc.Org.Slug,
			"workspace", wid, "policy", a.WS.AdmissionPolicy, "reason", v.Reason)
		http.Error(w, v.Reason, http.StatusServiceUnavailable)
		return
	}

	if _, err := s.ensureWorkspace(ctx, sc, a.WS); err != nil {
		s.log.Error("ensuring the workspace failed", "org", sc.Org.Slug, "workspace", wid, "error", err)
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	// Refuse rather than let the failure be discovered inside the PTY. A session
	// admitted into a workspace whose Bedrock is misconfigured starts Claude Code
	// successfully and then dies on the user's first prompt, with an error that
	// says nothing about the First Time Use form (sections 8, 13).
	if err := s.checkPreflight(ctx, sc, wid); err != nil {
		s.log.Warn("refusing session", "org", sc.Org.Slug, "workspace", wid, "error", err)
		http.Error(w, err.Error(), http.StatusFailedDependency)
		return
	}

	sess, err := s.createSessionRecord(ctx, sc, a.WS.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.log.Info("session created",
		"session", sess.ID, "org", sc.Org.Slug, "workspace", wid, "user", sc.UserID)

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
	ID string `json:"id"`
	// Workspace is the NAME, set only on the organization-wide listing where a
	// caller cannot infer it from the URL.
	Workspace string `json:"workspace,omitempty"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
	Owner     string `json:"owner,omitempty"`
	OwnerName string `json:"owner_name,omitempty"`
	// Mine says whether the CALLER started this session, which is what decides
	// whether they can drive it or only watch (section 2.5). It is reported
	// rather than left to the client to work out, because a client would have
	// to learn its own user id to compare against Owner -- and nothing else
	// gives it one.
	Mine      bool   `json:"mine"`
	Attachers int    `json:"attachers"`
	Rows      uint16 `json:"rows,omitempty"`
	Cols      uint16 `json:"cols,omitempty"`
	// IdleTimeoutSecs is section 2.4's per-session override when one is set.
	// Absent means the session inherits its workspace's, which is the common
	// case and is why this is a pointer rather than a resolved number: showing
	// the inherited value would make a later change to the workspace look like
	// it had not applied.
	IdleTimeoutSecs *int `json:"idle_timeout_secs,omitempty"`
}

type sessionListResponse struct {
	// WorkspaceID is empty on the organization-wide listing, where each session
	// carries its own instead.
	WorkspaceID string       `json:"workspace_id,omitempty"`
	Sessions    []sessionDoc `json:"sessions"`
}

// handleListOrgSessions reports every session the caller may see in the
// organization, whichever workspace it is in.
//
// Not in section 2.6's original list, and added because the CLI needs it: a
// session id is resolved by unique prefix, the endpoint that acts on one is
// org-scoped, and demanding a workspace purely to look the id up would make the
// client ask for something the API does not.
//
// A read that places nothing, like every other listing.
func (s *Server) handleListOrgSessions(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	records, err := s.listOrgSessions(r.Context(), sc)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Workspace names, for the column a user reads. Built from what they may
	// see, which is a superset of the workspaces their sessions are in.
	names := map[string]string{}
	if wss, werr := s.listWorkspaces(r.Context(), sc); werr == nil {
		for _, ws := range wss {
			names[ws.ID] = ws.Name
		}
	}
	owners := s.displayNames(r.Context(), sc)

	// Liveness costs one tunnel round trip per CONNECTED workspace, so it is
	// gathered per workspace rather than per session.
	live := map[string]map[string]tunnel.SessionInfo{}
	docs := make([]sessionDoc, 0, len(records))
	for _, rec := range records {
		name := names[rec.WorkspaceID]
		if _, seen := live[name]; !seen && name != "" {
			live[name] = s.liveSessions(sc, name)
		}
		d := sessionDoc{
			ID:        rec.ID,
			Workspace: name,
			Status:    SessionStopped,
			CreatedAt: timestamp(rec.CreatedAt),
			Owner:     rec.UserID,
			OwnerName: owners[rec.UserID],
			Mine:      rec.UserID == sc.UserID,
		}
		if info, running := live[name][rec.ID]; running {
			d.Status = SessionRunning
			d.Attachers = info.Attachers
			d.Rows, d.Cols = info.Rows, info.Cols
		}
		docs = append(docs, d)
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].CreatedAt > docs[j].CreatedAt })
	writeJSON(w, http.StatusOK, sessionListResponse{Sessions: docs})
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
	// A workspace the caller has no standing in is a 404 rather than an empty
	// list. It used to answer empty for anything it could not resolve, which
	// was harmless while a workspace was created by asking for one -- and would
	// now be a way to probe which names exist.
	a, ok := s.requireWorkspace(w, r)
	if !ok {
		return
	}
	sc, wid := a.scope, a.WS.Name

	records, err := s.listSessions(r.Context(), sc, a.WS.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	live := s.liveSessions(sc, wid)
	names := s.displayNames(r.Context(), sc)

	docs := make([]sessionDoc, 0, len(records))
	for _, rec := range records {
		d := sessionDoc{
			ID:        rec.ID,
			Status:    SessionStopped,
			CreatedAt: timestamp(rec.CreatedAt),
			Owner:     rec.UserID,
			OwnerName: names[rec.UserID],
			Mine:      rec.UserID == sc.UserID,
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
func (s *Server) liveSessions(sc scope, wid string) map[string]tunnel.SessionInfo {
	t, err := s.reg.PickWorkspace(sc.TenantID, wid)
	if err != nil {
		return nil
	}
	env, err := command(t, tunnel.MsgListSessions, nil, 10*time.Second)
	if err != nil || env.Type == tunnel.MsgError {
		s.log.Warn("listing sessions on the workspace task failed",
			"org", sc.Org.Slug, "workspace", wid, "error", err)
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
//
// It takes a workspace RECORD rather than a name, and creates nothing. Phase 1
// created on demand here, so a typo in a URL manufactured a workspace and there
// was no verb that did it deliberately; creation is now POST /workspaces, with
// the permission check that belongs on it (section 2.5). The caller has already
// resolved and authorised the record by the time it gets here, which is also
// what removes the second lookup this used to do.
//
// EVERY failure path below records itself on the record before returning
// (notePlacementFailure), and that is not for the caller's benefit -- a
// blocking caller already has the error. It is for the one that is not there:
// since increment 4 the common caller is a goroutine answering nobody, so a
// failure written only to the return value would reach a log and stop. Doing it
// here rather than in that goroutine means the two callers cannot disagree
// about what a failed placement leaves behind.
func (s *Server) ensureWorkspace(ctx context.Context, sc scope, ws Workspace) (*registry.Tunnel, error) {
	// Everything outside the database is keyed on the NAME -- the registry, the
	// supervisor's -workspace argv, the preflight store and the derived
	// credential -- while the record carries both. Read it once here so the
	// conversion does not get open-coded further down (state.go).
	wid := ws.Name

	// Locked per (organization, workspace name), for the same reason the
	// registry is keyed that way: a name is unique only within an organization,
	// so locking on the name alone would serialise two unrelated workspaces and,
	// worse, read as if it had protected something it had not.
	mu := s.lockWorkspace(sc.TenantID + "/" + wid)
	mu.Lock()
	defer mu.Unlock()

	if t, err := s.reg.PickWorkspace(sc.TenantID, wid); err == nil {
		return t, nil
	}

	// A missing tunnel is not the same as a missing task.
	//
	// The supervisor reconnects every 2 s, so a network blip, an ALB rotation or
	// a relay redeploy all present here as "no tunnel" for a few seconds while a
	// perfectly healthy task is on its way back. Placing into that gap used to be
	// actively destructive: taking a new generation rotates the credential, the
	// live task's next dial is answered 401, and it exits by design -- killing the
	// unattended run section 2.4 exists to protect. The ECS driver would then
	// adopt that same still-running task and hand back its ARN, so the placement
	// could never succeed either.
	//
	// So wait for the task we already have before concluding we need another one.
	// A genuinely dead task costs this grace period once, which is far cheaper
	// than the alternative it replaces.
	if ws.TaskRef != nil && *ws.TaskRef != "" &&
		(ws.Status == WorkspaceActive || ws.Status == WorkspaceStarting) {
		wait, cancel := context.WithTimeout(ctx, s.reconnectGrace())
		t, err := s.reg.WaitWorkspace(wait, sc.TenantID, wid)
		cancel()
		if err == nil {
			s.log.Info("workspace reconnected; no placement needed",
				"org", sc.Org.Slug, "workspace", wid)
			return t, nil
		}
		s.log.Info("workspace did not reconnect; placing a replacement",
			"org", sc.Org.Slug, "workspace", wid, "grace", s.reconnectGrace())
	}

	// Runner tunnels are keyed by organization, and the runner proves which one
	// it belongs to with a credential derived for that organization -- so this
	// cannot reach a runner in somebody else's account.
	runner, err := s.reg.PickRunner(sc.TenantID)
	if err != nil {
		err = fmt.Errorf("no runner connected for organization %s", sc.Org.Slug)
		s.notePlacementFailure(ctx, sc, ws, "", err)
		return nil, err
	}

	// A new generation for every REAL placement. It is what the idempotency key
	// is derived from, and it invalidates the previous task's credential -- which
	// is why reaching it has to mean "we are replacing the task", not merely "we
	// could not see it for a moment".
	gen, err := s.nextGeneration(ctx, sc, ws.ID)
	if err != nil {
		s.notePlacementFailure(ctx, sc, ws, "", err)
		return nil, err
	}
	// The read-modify-write that once pinned this counter at zero is gone: the
	// increment and the read are one UPDATE ... RETURNING, so there is no stale
	// copy to write back over it (see nextGeneration).
	ws.Generation = gen
	// Derived, not stored. The credential is a function of (organization,
	// workspace, generation) under a key that outlives the process, so a
	// restarted control plane recognises a task its predecessor placed instead
	// of 401ing it into exiting and taking its sessions with it (internal/creds).
	cred := s.signer.WorkspaceToken(sc.TenantID, wid, uint64(gen))

	if err := s.setWorkspaceStatus(ctx, sc, ws.ID, WorkspaceStarting, ""); err != nil {
		// Logged rather than swallowed: a status write that fails leaves the
		// record disagreeing with reality, and the next placement decision reads
		// that record.
		s.log.Error("recording workspace status", "workspace", wid, "error", err)
	}

	env, err := command(runner, tunnel.MsgStartWorkspace, tunnel.StartWorkspace{
		TenantID:     sc.TenantID,
		WorkspaceID:  wid,
		Generation:   uint64(gen),
		Credential:   cred.Secret(),
		ControlPlane: s.agentURL(),
		Relay:        s.relayURL(),
		Image:        s.opt.Image,
		Env:          s.workspaceEnv(),
		// The workspace's filesystem, carried over from its previous task.
		//
		// ECS gives a task a new volume or one built from a snapshot, and never
		// an existing volume, so this is the whole of how a stopped workspace
		// comes back with its members' homes on it. Empty means a blank disk,
		// which is right exactly once: the first time a workspace starts.
		SnapshotID: derefOr(ws.SnapshotID),
	}, 60*time.Second)
	if err != nil {
		err = fmt.Errorf("dispatch: %w", err)
		s.notePlacementFailure(ctx, sc, ws, "", err)
		return nil, err
	}
	if env.Type == tunnel.MsgError {
		var e tunnel.Error
		_ = env.Decode(&e)
		err = fmt.Errorf("runner refused: %s", e.Message)
		s.notePlacementFailure(ctx, sc, ws, "", err)
		return nil, err
	}
	var ref tunnel.Ref
	_ = env.Decode(&ref)

	if err := s.setWorkspaceStatus(ctx, sc, ws.ID, WorkspaceStarting, ref.Ref); err != nil {
		s.log.Error("recording task reference", "workspace", wid, "error", err)
	}
	s.log.Info("workspace dispatched", "org", sc.Org.Slug,
		"workspace", wid, "generation", gen, "runner", runner.ID, "ref", ref.Ref)

	t, err := s.reg.WaitWorkspace(ctx, sc.TenantID, wid)
	if err != nil {
		err = fmt.Errorf("workspace %s did not dial in: %w", wid, err)
		// The task WAS dispatched, so the record keeps its reference and stays
		// `starting`: it may be slow rather than dead, and forgetting the
		// reference here would leave the only handle for stopping it in a log.
		s.notePlacementFailure(ctx, sc, ws, ref.Ref, err)
		return nil, err
	}
	if err := s.setWorkspaceStatus(ctx, sc, ws.ID, WorkspaceActive, ref.Ref); err != nil {
		s.log.Error("recording workspace status", "workspace", wid, "error", err)
	}
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
	// What a session runs, stated ONCE, here, on the way to the task.
	//
	// It used to be configured independently in this service and in the relay,
	// each defaulting to `claude` on its own, and each sent down its own tunnel
	// at fork time -- so a deployment that told one and not the other ran a
	// different program depending on which path created the session, silently.
	//
	// Two reasons it lands here rather than being made to agree. This is the
	// path that already carries every other task-level fact, for the reason in
	// the comment above; and section 2.5 forbids the alternative outright, since
	// a command named on the DATA tunnel is arbitrary code at a member's uid
	// chosen by the service that holds no customer record. The supervisor is now
	// the only thing that knows, and both of its create paths read the one
	// answer.
	//
	// JSON rather than a space-separated string, because an argv element may
	// itself contain spaces -- `sh -c 'a; b'` is one argument, and the e2e
	// fidelity suite is made of them. Joining on spaces would re-split that into
	// three, which is the kind of corruption that shows up as a session exiting
	// immediately with a message from a shell.
	if len(s.opt.SessionCmd) > 0 {
		if b, err := json.Marshal(s.opt.SessionCmd); err == nil {
			env["LEMUL_SESSION_CMD"] = string(b)
		}
	}
	// The reporting cadence, told to the task rather than defaulted separately
	// at both ends. This side measures staleness in multiples of it (section
	// 2.4), so two independent defaults would silently move the line at which a
	// report stops being trusted.
	if s.headroomInterval > 0 {
		env["LEMUL_HEADROOM_INTERVAL"] = s.headroomInterval.String()
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
	// Mode echoes what the credential actually authorises, which is not
	// necessarily what was asked for. A client that requested control and is
	// handed viewer should be able to say so rather than discovering it as
	// keystrokes going nowhere.
	Mode       string `json:"mode,omitempty"`
	PeerPubkey string `json:"peer_pubkey,omitempty"`
}

// handleEndpoint tells the client where to connect.
//
// v0.1 only ever answers "relay". The indirection exists anyway because
// hardcoding the relay into the client makes E2E, `direct` and `tailnet` a
// rewrite rather than a per-tenant config change (section 2.7). peer_pubkey is
// empty until E2E lands; the field is in the contract from the start so adding
// it does not break older clients.
// The mode is chosen HERE rather than at attach, and travels inside the signed
// credential, so the relay enforces what was authorised rather than what the
// connecting client claims about itself.
//
// SECTION 2.5's owner-scoped attach, which is what this endpoint was missing.
// A session is user-specific while a workspace is shared, so authorising attach
// at the workspace level would drop one person into another's live Claude Code
// with a keyboard. The rule:
//
//	the session's own user   -> the mode they asked for
//	a workspace owner asking for viewer -> viewer
//	a workspace owner asking for control -> 403, naming --viewer
//	anyone else              -> 404, via requireSession
//
// An owner is REFUSED rather than quietly downgraded. Handing back a viewer
// credential to somebody who asked to drive is a surprise discovered as
// keystrokes going nowhere; making them say --viewer means watching a
// colleague's session is always a thing they chose to do. It is conspicuous
// too -- an attach bumps the attacher count the owner can see in `lem` -- which
// is what makes viewer acceptable here at all where silent reading would not
// be.
func (s *Server) handleEndpoint(w http.ResponseWriter, r *http.Request) {
	a, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	sc, sess := a.scope, a.Sess

	mode := r.URL.Query().Get("mode")
	if mode != tunnel.ModeViewer {
		mode = tunnel.ModeControl
	}
	if !a.isMine() {
		// Reaching here means the caller owns the workspace: requireSession
		// already refused everybody else with a 404.
		if mode != tunnel.ModeViewer {
			http.Error(w, "this session belongs to another user; attach with --viewer to watch it",
				http.StatusForbidden)
			return
		}
		s.log.Info("owner attaching to another user's session as a viewer",
			"session", sess.ID, "org", sc.Org.Slug, "owner", sess.UserID, "viewer", sc.UserID)
	}

	// Placement happens HERE, not at attach, and AFTER the decision above.
	//
	// It used to live in the attach handler, which could reach a runner because
	// it was the same process. The relay is its own service now: no runner
	// tunnel, no store, no way to place anything -- and the two deliberately
	// never call each other, so there is nowhere else for this to go. The client
	// already calls endpoint negotiation first, so no round trip is added; what
	// changes is that this request pays the 20-60 s cold start rather than the
	// attach that follows it.
	//
	// After the authorisation decision because the order is itself a rule: a
	// caller who may not drive this session must not be able to make us start a
	// Fargate task on their behalf. Refusing first costs them nothing and costs
	// the organization nothing.
	ctx, cancel := context.WithTimeout(r.Context(), s.opt.StartTimeout)
	defer cancel()

	if _, err := s.ensureWorkspace(ctx, sc, a.WS); err != nil {
		s.log.Error("ensuring the workspace for an attach",
			"org", sc.Org.Slug, "workspace", a.WS.Name, "error", err)
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	// The same gate as session-create and resume. Attach carries Create, so it
	// can bring a process into existence on its own -- letting it through here
	// would just move the failure to the user's first prompt (section 12.3).
	if err := s.checkPreflight(ctx, sc, a.WS.Name); err != nil {
		s.log.Warn("refusing an attach endpoint", "session", sess.ID, "error", err)
		http.Error(w, err.Error(), http.StatusFailedDependency)
		return
	}

	// Who this session runs as, told to the task on the CONTROL tunnel before a
	// credential is handed out.
	//
	// Here rather than at session-create because attach carries Create and can
	// fork a PTY on its own, and because a replacement task has no memory of
	// anything: a client must renegotiate before reconnecting, so this is the one
	// place that is guaranteed to run first every time. It is the session's OWN
	// user, not the caller's -- a workspace owner attaching as a viewer watches
	// somebody else's process, and that process keeps running as its owner.
	if err := s.prepareSession(ctx, a.wsScope, sess.ID, sess.UserID); err != nil {
		s.log.Error("resolving the session's identity",
			"session", sess.ID, "workspace", a.WS.Name, "error", err)
		http.Error(w, "could not resolve who this session runs as",
			http.StatusInternalServerError)
		return
	}

	// The credential carries the organization and the workspace NAME, not the
	// workspace uuid. Those are what a relay holding no database needs to find
	// the tunnel: the registry is keyed on (organization, name), so a uuid in
	// here would have to be resolved against the cell first -- exactly the
	// dependency splitting the relay out is meant to remove.
	cred, err := s.signer.AttachToken(sess.ID, sc.TenantID, a.WS.Name, sc.Org.Slug, mode, creds.AttachTTL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, endpointResponse{
		Transport:  "relay",
		Address:    s.clientURL(r) + "/v1/sessions/" + sess.ID + "/attach",
		Credential: cred.Secret(),
		Mode:       mode,
	})
}

// agentURL is the base URL a placed workspace task dials back to. Unlike
// clientURL it cannot fall back to the request host: the request that triggers
// placement comes from the client, whose view of the control plane is not
// necessarily the task's.
func (s *Server) agentURL() string {
	return strings.TrimRight(s.opt.PublicURL, "/")
}

// clientURL is the base URL the client dials for the ATTACH WebSocket, which is
// the relay's address rather than this service's.
//
// It follows the request host only as a last resort, and that fallback is now a
// development convenience rather than a sane default: once the relay is its own
// service, answering with this process's host sends the client to a listener
// that has no attach handler at all. -relay-url is what a real deployment sets.
func (s *Server) clientURL(r *http.Request) string {
	if u := s.relayURL(); u != "" {
		return u
	}
	if s.opt.PublicURL != "" {
		return strings.TrimRight(s.opt.PublicURL, "/")
	}
	return "ws://" + r.Host
}

// relayURL is where the session relay listens, trimmed. Empty when this
// deployment has not been told, which the callers each answer differently.
func (s *Server) relayURL() string {
	return strings.TrimRight(s.opt.RelayURL, "/")
}
