package mgmtapi

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/lufzle/lemul/internal/store"
	"github.com/lufzle/lemul/internal/store/celldb"
	"github.com/lufzle/lemul/internal/tunnel"
)

// Telling a workspace task who a session belongs to.
//
// A workspace is a shared machine (section 2.3), so a session runs at its
// member's own uid, in their own home, with their own Claude Code state. The
// task cannot work that out for itself: it holds no record of who is in an
// organization, and a uid derived from anything local would have to be
// re-derived after every placement, against a volume that outlived the task.
//
// It travels on the CONTROL tunnel, and that is the load-bearing part. The relay
// holds no customer records, so a uid arriving from it would mean a compromised
// relay chooses whose files a session can read -- which is exactly the boundary
// being built. The service that reads the membership row is the service that
// states the answer, and the relay never learns it.
//
// Sent from the two places, and only the two places, that can precede a PTY
// fork: endpoint negotiation, which every attach must call first, and resume,
// which starts a process with no client attached. A task replaced under a live
// session is told again by the same route, because a client has to renegotiate
// an endpoint before it can reconnect.

// uidBase is the first uid handed to a member. Below it sit the image's own
// `node` user at 1000 and everything a distribution reserves. Mirrored by a
// CHECK in schema.sql; the database is the authority.
const uidBase = 2000

// homesRoot is where per-member homes live inside a workspace task.
//
// On the workspace VOLUME rather than the container filesystem: a replacement
// task starts with an empty disk, and a home that did not survive one would take
// the member's configuration and conversation history with it. Same reasoning
// that already put the config directory there.
const homesRoot = "/workspace/homes"

// memberIdentity is what a workspace task needs in order to run a session as the
// person who started it.
type memberIdentity struct {
	UID      uint32
	Username string
	Home     string
}

// resolveIdentity finds or allocates a uid for one member of a workspace.
//
// It takes the user explicitly rather than reading the caller off the scope,
// because the two are NOT the same person often enough to matter: a workspace
// owner attaching as a viewer watches somebody else's process, and that process
// goes on running as its own owner. Announcing the viewer's uid would move
// another member's session into the viewer's home the next time it forked.
//
// Lazily, because a workspace open to the whole organization HAS no membership
// rows -- nobody was added, so there is nothing to allocate against until
// somebody actually starts something. The row written here is a uid allocation
// rather than an access grant: whoever it is for already holds a session in this
// workspace, or was already authorised by accessTo to open one.
func (s *Server) resolveIdentity(ctx context.Context, a wsScope, userID string) (memberIdentity, error) {
	var row celldb.WorkspaceMember
	err := s.st.InTenant(ctx, a.Scope, func(q *celldb.Queries) error {
		// DO NOTHING on conflict, not DO UPDATE: this runs for anybody who can
		// reach the workspace, including its owner, and AddMember's upsert would
		// quietly demote them to 'user' on their own first session.
		if err := q.EnsureMember(ctx, celldb.EnsureMemberParams{
			WorkspaceID: a.WS.ID, UserID: userID, TenantID: a.TenantID, Role: roleUser,
		}); err != nil {
			return err
		}
		m, err := q.GetMembership(ctx, celldb.GetMembershipParams{
			WorkspaceID: a.WS.ID, UserID: userID, TenantID: a.TenantID,
		})
		if err != nil {
			return err
		}
		if m.Uid != nil {
			row = m
			return nil
		}
		// No uid yet. Take one from the workspace's counter and claim it.
		next, err := q.NextWorkspaceUID(ctx, celldb.NextWorkspaceUIDParams{
			ID: a.WS.ID, TenantID: a.TenantID,
		})
		if err != nil {
			return err
		}
		m, err = q.SetMemberUID(ctx, celldb.SetMemberUIDParams{
			WorkspaceID: a.WS.ID, UserID: userID, TenantID: a.TenantID, Uid: &next,
		})
		if err == nil {
			row = m
			return nil
		}
		// Updating no rows means somebody else claimed one between the read and
		// the write. Theirs stands; the number this transaction took is simply
		// spent, which costs nothing but a gap in the sequence.
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		row, err = q.GetMembership(ctx, celldb.GetMembershipParams{
			WorkspaceID: a.WS.ID, UserID: userID, TenantID: a.TenantID,
		})
		return err
	})
	if err != nil {
		return memberIdentity{}, err
	}
	if row.Uid == nil {
		return memberIdentity{}, fmt.Errorf(
			"no uid allocated for user %s in workspace %s", userID, a.WS.Name)
	}
	if *row.Uid < uidBase {
		// The CHECK refuses this, so reaching it means the constraint went
		// missing rather than that a caller did something. Refuse rather than
		// hand a workspace task a uid that could collide with the image's own.
		return memberIdentity{}, fmt.Errorf("uid %d is below the reserved floor %d",
			*row.Uid, uidBase)
	}
	uid := uint32(*row.Uid) //nolint:gosec // bounded below by the CHECK, above by int32
	name := unixName(uid)
	return memberIdentity{UID: uid, Username: name, Home: homesRoot + "/" + name}, nil
}

// unixName is the login name a member's sessions run under.
//
// Built from the uid, with their email address playing no part. A login name
// shows up in `ps`, in file ownership across /shared and in any command the
// agent runs, so deriving it from an address would publish one member's email to
// everybody else sharing the machine. The uid is already visible in all three
// places and says nothing beyond "somebody else".
func unixName(uid uint32) string { return fmt.Sprintf("lem%d", uid) }

// identityAnnounceTimeout bounds the wait for the task to acknowledge.
//
// Short, because the work at the far end is a map insert: a supervisor that
// cannot answer this in five seconds is not about to serve a PTY either. The
// wait itself is not optional -- the attach that follows would race the message
// otherwise -- so what it costs is what a wedged task adds to every attach, and
// that is the number worth keeping small.
const identityAnnounceTimeout = 5 * time.Second

// announceIdentity tells the workspace task who a session runs as.
//
// Warned about rather than fatal, and that is a deliberate division of labour:
// the supervisor REFUSES a session it has no identity for, so a message that did
// not arrive surfaces as a refused session naming the reason, instead of a
// session quietly running at the wrong uid in somebody else's home. Failing here
// too would only move the same error earlier.
func (s *Server) announceIdentity(a wsScope, sessionID string, who memberIdentity) {
	t, err := s.reg.PickWorkspace(a.TenantID, a.WS.Name)
	if err != nil {
		s.log.Warn("no workspace tunnel to announce a session identity on",
			"workspace", a.WS.Name, "session", sessionID)
		return
	}
	env, err := command(t, tunnel.MsgSessionIdentity, tunnel.SessionIdentity{
		SessionID: sessionID,
		UID:       who.UID,
		Username:  who.Username,
		Home:      who.Home,
	}, identityAnnounceTimeout)
	if err != nil || env.Type == tunnel.MsgError {
		s.log.Warn("announcing a session identity", "workspace", a.WS.Name,
			"session", sessionID, "uid", who.UID, "error", err)
		return
	}
	s.log.Info("session identity announced", "workspace", a.WS.Name,
		"session", sessionID, "uid", who.UID, "user", who.Username)
}

// prepareSession resolves the identity a session runs as and tells the task, in
// one call, because doing either without the other is never useful.
//
// userID is the SESSION'S owner, never the caller.
func (s *Server) prepareSession(ctx context.Context, a wsScope, sessionID, userID string) error {
	who, err := s.resolveIdentity(ctx, a, userID)
	if err != nil {
		return err
	}
	s.announceIdentity(a, sessionID, who)
	return nil
}
