package server

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/gorilla/websocket"

	"github.com/jbrozy/chatbox/internal/terminal"
)

// Close-Codes wie in terminals (siehe x-websocket in openapi.json).
const (
	closeAuthFailed    = 4001
	closeMissingUser   = 4002
	closeUnreachable   = 4003
	closeInvalidTarget = 4004
)

const (
	wsAuthTimeout  = 10 * time.Second
	wsDialAttempts = 5
	wsTouchEvery   = 30 * time.Second
)

// Authentifiziert wird über die erste Nachricht, nicht über Origin oder Header.
var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

type wsAuth struct {
	Type   string `json:"type"`
	Token  string `json:"token"`
	ChatID string `json:"chat_id,omitempty"`
}

// terminalWebSocket terminiert die Verbindung von Open WebUI, prüft die erste Nachricht,
// verbindet sich mit dem Terminal im Pod und kopiert danach Frames in beide Richtungen.
// policyID ist leer für die Route ohne /p/-Prefix.
func (h *Handler) terminalWebSocket(w http.ResponseWriter, r *http.Request, policyID, sessionID string) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade hat bereits geantwortet
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(wsAuthTimeout))
	msgType, msg, err := conn.ReadMessage()
	var auth wsAuth
	if err != nil || msgType != websocket.TextMessage || json.Unmarshal(msg, &auth) != nil {
		closeWS(conn, closeAuthFailed, "Auth timeout or invalid payload")
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	switch {
	case auth.Type != "auth":
		closeWS(conn, closeAuthFailed, "Expected auth message")
		return
	case !validToken(auth.Token, h.apiKey):
		closeWS(conn, closeAuthFailed, "Invalid API key")
		return
	}

	userID := r.URL.Query().Get("user_id")
	if userID == "" {
		closeWS(conn, closeMissingUser, "Missing user_id")
		return
	}
	if policyID == "" {
		policyID = defaultPolicy
	} else if !h.policies.exists(policyID) {
		closeWS(conn, closeInvalidTarget, fmt.Sprintf("Policy '%s' not found", policyID))
		return
	}
	ctxID, err := contextID(cmp.Or(r.Header.Get("X-Terminal-Context-Id"), r.URL.Query().Get("context_id")))
	if err != nil {
		closeWS(conn, closeInvalidTarget, err.Error())
		return
	}
	key := terminal.Key{UserID: userID, PolicyID: policyID, ContextID: ctxID}
	spec, _ := h.policies.spec(policyID)

	upstream, err := h.dialTerminal(r.Context(), key, spec, sessionID)
	if err != nil {
		slog.Error("terminal websocket failed", "user_id", userID, "policy_id", policyID, "session_id", sessionID, "err", err)
		closeWS(conn, closeUnreachable, "Terminal instance not reachable")
		return
	}
	defer upstream.Close()

	// Open Terminal nimmt die Chat-ID aus der Auth-Nachricht, weil der Proxy keine Header weitergibt.
	inst := upstream.inst
	if err := upstream.WriteJSON(wsAuth{Type: "auth", Token: inst.APIKey, ChatID: r.Header.Get("X-Session-Id")}); err != nil {
		closeWS(conn, closeUnreachable, "Terminal instance not reachable")
		return
	}

	release := h.backend.Connected(key)
	defer release()

	errc := make(chan error, 2)
	go func() { errc <- h.pump(upstream.Conn, conn, key) }()
	go func() { errc <- h.pump(conn, upstream.Conn, key) }()
	err = <-errc

	// Close-Code der Gegenseite weitergeben, z. B. 4004 wenn die Session im Pod fehlt.
	// 1005 und 1006 sind nur lokale Statuswerte und dürfen nicht gesendet werden.
	code, text := websocket.CloseNormalClosure, ""
	if ce, ok := err.(*websocket.CloseError); ok && ce.Code != websocket.CloseNoStatusReceived && ce.Code != websocket.CloseAbnormalClosure {
		code, text = ce.Code, ce.Text
	}
	closeWS(conn, code, text)
	closeWS(upstream.Conn, code, text)
}

type upstreamConn struct {
	*websocket.Conn
	inst terminal.Instance
}

// dialTerminal verbindet sich mit /api/terminals/{session_id} im Pod. Der Container kann
// kurz nach der Provisionierung noch nicht lauschen, deshalb mehrere Versuche.
func (h *Handler) dialTerminal(ctx context.Context, key terminal.Key, spec terminal.Spec, sessionID string) (*upstreamConn, error) {
	var lastErr error
	for attempt := range wsDialAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(h.retryDelay):
			}
		}
		inst, err := h.backend.Ensure(ctx, key, spec)
		if err != nil {
			return nil, err
		}
		target := inst.URL("ws") + "/api/terminals/" + url.PathEscape(sessionID)
		conn, resp, err := websocket.DefaultDialer.DialContext(ctx, target, nil)
		if err == nil {
			return &upstreamConn{Conn: conn, inst: inst}, nil
		}
		if resp != nil {
			resp.Body.Close()
		}
		h.backend.Invalidate(key)
		lastErr = err
	}
	return nil, lastErr
}

// pump kopiert Frames von src nach dst und behält dabei Text/Binär bei.
func (h *Handler) pump(dst, src *websocket.Conn, key terminal.Key) error {
	lastTouch := time.Now()
	for {
		msgType, data, err := src.ReadMessage()
		if err != nil {
			return err
		}
		if err := dst.WriteMessage(msgType, data); err != nil {
			return err
		}
		if time.Since(lastTouch) > wsTouchEvery {
			h.backend.Touch(key)
			lastTouch = time.Now()
		}
	}
}

// closeWS ist sicher parallel zu laufenden Writes (WriteControl).
func closeWS(conn *websocket.Conn, code int, reason string) {
	msg := websocket.FormatCloseMessage(code, reason)
	_ = conn.WriteControl(websocket.CloseMessage, msg, time.Now().Add(time.Second))
}
