package relay

import (
	"encoding/json"
	"net/http"

	"github.com/gorilla/websocket"
	"github.com/lufzle/lemul/internal/proto"
	"github.com/lufzle/lemul/internal/tunnel"
)

// handleAttach joins one user WebSocket to one stream on a workspace's data
// tunnel.
//
// The relay must be 8-bit clean and byte-transparent: no line processing, no
// sanitisation, no logging layer that touches the bytes (section 4.1). It is
// also the translation point between two framing schemes -- WebSocket frame
// types on the client hop, an explicit type tag inside the tunnel -- and
// getting that mapping wrong is the most plausible way to break fidelity: a
// control frame misread as data injects JSON into the user's terminal.
//
// WHAT THIS NO LONGER DOES, and why each one had to go:
//
//   - It does not read the session record. Everything it needs is in the signed
//     credential, which is what made this service separable in the first place.
//   - It does not place the workspace. There is no runner tunnel here and no
//     store to read, and the two services deliberately never call each other,
//     so placement moved to endpoint negotiation -- which the client already
//     calls first.
//   - It does not check the Bedrock preflight, for the same reason and to the
//     same place.
//   - It does not name the program a created session runs. That was the last
//     thing this service could still decide about the inside of a workspace,
//     and section 2.5's rule about the uid covers it with room to spare: a
//     command chosen here is arbitrary code at a member's uid, chosen by the
//     one service designed to be untrusted with customer data.
//
// The cost is one honest failure that did not exist before: a task can die
// between endpoint negotiation and attach, and this end can no longer fix that.
// It answers 503 telling the client to negotiate again, which beats a hang.
func (s *Server) handleAttach(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")

	// Credential first: an unauthenticated caller must not reach the upgrade.
	tok := r.URL.Query().Get("credential")
	if tok == "" {
		tok = bearer(r)
	}
	claims, ok := s.redeemAttach(tok)
	if !ok || claims.SessionID != sid {
		jsonError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	// Everything below is scoped by what WE SIGNED, never by anything the
	// caller supplied. There is no bearer token on this endpoint -- a browser
	// cannot set one on a WebSocket handshake -- so the credential is the only
	// thing here with any authority. The workspace NAME travels in it too, so
	// routing needs no lookup: the registry is keyed on (organization, name).
	tenant, wsName := claims.TenantID, claims.WorkspaceName

	t, err := s.reg.PickWorkspace(tenant, wsName)
	if err != nil {
		// The window this opens is real and small: endpoint negotiation placed
		// the task and waited for it, and it has gone away since. Naming the
		// remedy matters because the client CAN fix it by asking again, and
		// nothing here can.
		jsonError(w, http.StatusServiceUnavailable,
			"this workspace has no live task; ask for an endpoint again")
		return
	}

	// The mode comes from the CREDENTIAL, not the query string. A viewer is
	// input-dropping (section 2.5), so letting the connecting client name its
	// own mode would mean the enforcement point trusts the party it is
	// enforcing against. Endpoint negotiation decides -- including whether the
	// caller may drive somebody else's session at all -- and this end only
	// carries out what was signed.
	mode := claims.Mode
	if mode != tunnel.ModeViewer {
		mode = tunnel.ModeControl
	}

	stream, err := t.Open()
	if err != nil {
		jsonError(w, http.StatusServiceUnavailable, "workspace tunnel unavailable")
		return
	}
	defer func() { _ = stream.Close() }()

	// The size must reach the supervisor before it forks: a PTY defaults to 0x0
	// and a resize arriving after the fork is too late, because Claude Code has
	// already drawn its first frame into a zero-size terminal (section 4.1). So
	// it travels in the attach request, ahead of any data.
	req := tunnel.Attach{
		SessionID: sid,
		Rows:      queryUint16(r, "rows", 24),
		Cols:      queryUint16(r, "cols", 80),
		Mode:      mode,
		// Create is what makes `lem --session <id>` cover reattach AND resume:
		// the supervisor forks when the PTY is absent and picks --resume or
		// --session-id from the transcript on the volume (section 12.7).
		//
		// WHAT it forks is not ours to say -- see the note on tunnel.Attach.
		Create: true,
	}
	if err := tunnel.WriteMsg(stream, tunnel.MsgAttach, req); err != nil {
		jsonError(w, http.StatusBadGateway, "attach failed")
		return
	}
	reply, err := tunnel.ReadMsg(stream)
	if err != nil {
		jsonError(w, http.StatusBadGateway, "attach failed")
		return
	}
	if reply.Type == tunnel.MsgError {
		var e tunnel.Error
		_ = reply.Decode(&e)
		jsonError(w, http.StatusConflict, e.Message)
		return
	}
	var ok2 tunnel.AttachOK
	_ = reply.Decode(&ok2)

	// Only now upgrade: everything above can still answer with an HTTP status,
	// which is a far better error for the client than an immediate close frame.
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := proto.NewConn(ws)
	defer func() { _ = c.Close() }()
	// Logged with the workspace NAME and the session id, which is all this
	// service knows -- and deliberately all it knows. There is no user here to
	// attribute the attach to; endpoint negotiation did that, where the identity
	// was.
	s.log.Info("client attached",
		"session", sid, "workspace", wsName, "tenant", tenant,
		"created", ok2.Created, "mode", mode, "cols", req.Cols, "rows", req.Rows)

	// supervisor -> user.
	go func() {
		defer func() { _ = c.Close() }()
		for {
			typ, payload, err := tunnel.ReadFrame(stream)
			if err != nil {
				// Stream gone without a session_ended marker: the tunnel dropped
				// or the task died. Leave it as an abnormal close so the client
				// treats it as a drop and can reconnect.
				return
			}
			switch typ {
			case tunnel.FrameData:
				if err := c.WriteBytes(payload); err != nil {
					return
				}
			case tunnel.FrameControl:
				// The supervisor sends exactly one control message on an
				// established attach stream: the child exited. The close CODE is
				// how the client distinguishes "session over, do not reconnect"
				// from "connection dropped, reconnect".
				_ = c.WriteCloseFrame(websocket.CloseNormalClosure, "session ended")
				return
			}
		}
	}()

	// user -> supervisor. The WebSocket frame type selects the tunnel frame
	// type; this is the only place the two schemes meet.
	for {
		mt, data, err := c.Read()
		if err != nil {
			return
		}
		switch mt {
		case websocket.BinaryMessage:
			// A viewer's keystrokes are dropped here as well as in the
			// supervisor. Both attachers write the same PTY stdin, so a viewer
			// that can write is silently a co-driver (section 2.5).
			if mode == tunnel.ModeViewer {
				continue
			}
			if err := tunnel.WriteFrame(stream, tunnel.FrameData, data); err != nil {
				return
			}
		case websocket.TextMessage:
			var m proto.Control
			if err := json.Unmarshal(data, &m); err != nil {
				continue
			}
			if m.Type != proto.TypeResize || mode == tunnel.ModeViewer {
				continue
			}
			if err := tunnel.WriteMsg(stream, tunnel.MsgResize, tunnel.Resize{
				Rows: m.Rows, Cols: m.Cols,
			}); err != nil {
				return
			}
		}
	}
}
