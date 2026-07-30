package controlplane

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"github.com/gorilla/websocket"
	"github.com/lufzle/lemul-cc/internal/proto"
	"github.com/lufzle/lemul-cc/internal/tunnel"
)

// handleAttach joins one user WebSocket to one stream on the workspace tunnel.
//
// The relay must be 8-bit clean and byte-transparent: no line processing, no
// sanitisation, no logging layer that touches the bytes (section 4.1). It is
// also the translation point between two framing schemes -- WebSocket frame
// types on the client hop, an explicit type tag inside the tunnel -- and
// getting that mapping wrong is the most plausible way to break fidelity: a
// control frame misread as data injects JSON into the user's terminal.
func (s *Server) handleAttach(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")

	// Credential first: an unauthenticated caller must not reach the upgrade.
	tok := r.URL.Query().Get("credential")
	if tok == "" {
		tok = bearer(r)
	}
	claimed, ok := s.creds.redeemAttach(tok)
	if !ok || claimed != sid {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

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

	mode := r.URL.Query().Get("mode")
	if mode != tunnel.ModeViewer {
		mode = tunnel.ModeControl
	}

	stream, err := t.Open()
	if err != nil {
		http.Error(w, "workspace tunnel unavailable", http.StatusServiceUnavailable)
		return
	}
	defer func() { _ = stream.Close() }()

	// The size must reach the supervisor before it forks: a PTY defaults to 0x0
	// and a resize arriving after the fork is too late, because Claude Code has
	// already drawn its first frame into a zero-size terminal (section 4.1).
	// So it travels in the attach request, ahead of any data.
	req := tunnel.Attach{
		SessionID: sid,
		Rows:      queryUint16(r, "rows", 24),
		Cols:      queryUint16(r, "cols", 80),
		Mode:      mode,
		Create:    true,
		Cmd:       s.opt.SessionCmd,
	}
	if err := tunnel.WriteMsg(stream, tunnel.MsgAttach, req); err != nil {
		http.Error(w, "attach failed", http.StatusBadGateway)
		return
	}
	reply, err := tunnel.ReadMsg(stream)
	if err != nil {
		http.Error(w, "attach failed", http.StatusBadGateway)
		return
	}
	if reply.Type == tunnel.MsgError {
		var e tunnel.Error
		_ = reply.Decode(&e)
		http.Error(w, e.Message, http.StatusConflict)
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
	defer c.Close()
	log.Printf("session %s attached via relay (created=%v mode=%s size=%dx%d)",
		sid, ok2.Created, mode, req.Cols, req.Rows)

	// supervisor -> user.
	go func() {
		defer c.Close()
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

func queryUint16(r *http.Request, key string, def uint16) uint16 {
	v, err := strconv.ParseUint(r.URL.Query().Get(key), 10, 16)
	if err != nil || v == 0 {
		return def
	}
	return uint16(v)
}
