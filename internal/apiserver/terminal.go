package apiserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/coder/websocket"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
)

// terminalMux intercepts /v1/sandboxes/{id}/terminal and serves the WebSocket;
// all other requests fall through to next (the gateway). It sits inside
// OwnerProxy, so a remote sandbox's upgrade is already proxied to its owner.
func terminalMux(term, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, ok := terminalSandboxID(r.URL.Path); ok && id != "" {
			term.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// terminalSandboxID returns the {id} from /v1/sandboxes/{id}/terminal.
func terminalSandboxID(p string) (string, bool) {
	const pre = "/v1/sandboxes/"
	if !strings.HasPrefix(p, pre) || !strings.HasSuffix(p, "/terminal") {
		return "", false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(p, pre), "/terminal")
	if id == "" || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
}

// TerminalHandler upgrades to a WebSocket and bridges it to a Terminal session.
// Auth is enforced upstream: the cookie/bearer middleware authenticates and the
// route is wrapped in RequireRole("admin", …) in server.go (a terminal is a root
// shell). websocket.Accept enforces the same-origin Origin check by default (ADR-0017).
func (s *SandboxService) TerminalHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := terminalSandboxID(r.URL.Path)
		if !ok {
			http.NotFound(w, r)
			return
		}
		name, err := s.mgr.Resolve(r.Context(), id)
		if err != nil {
			http.Error(w, "sandbox not found", http.StatusNotFound)
			return
		}
		c, err := websocket.Accept(w, r, nil) // nil opts => same-origin enforced (ADR-0017)
		if err != nil {
			return // Accept already wrote the response (e.g. 403 on bad Origin)
		}
		defer c.CloseNow()

		_ = s.mgr.BumpActivity(r.Context(), id) // a Terminal session is Activity
		// Prefer bash (tab completion + history) but fall back to sh where it isn't
		// installed. exec replaces the bootstrap sh so signals/PTY pass straight through.
		shell := []string{"/bin/sh", "-c", "if command -v bash >/dev/null 2>&1; then exec bash; else exec /bin/sh; fi"}
		sess, err := s.mgr.Backend().ExecInteractive(r.Context(), name, shell, true)
		if err != nil {
			_ = c.Close(websocket.StatusInternalError, "exec failed")
			return
		}
		defer sess.Close()
		bridgeTerminal(r.Context(), c, sess)
	})
}

type resizeMsg struct {
	Type string `json:"type"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

// bridgeTerminal copies sess.Stdout -> ws (binary) and ws -> sess.Stdin, parsing
// text control frames as resize requests. Returns when either side ends.
func bridgeTerminal(ctx context.Context, c *websocket.Conn, sess sandbox.Session) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// session stdout -> websocket. This MUST be the only reader of sess.Stdout():
	// the SDK's stdout is an io.Pipe, and a second concurrent reader (e.g. a
	// sess.Wait() that drains stdout) splits the stream — each pipe write goes to
	// just one waiting reader, so half the bytes (every other echoed keystroke)
	// would be stolen. End-of-session is detected here via EOF instead.
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := sess.Stdout().Read(buf)
			if n > 0 {
				if werr := c.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
					cancel()
					return
				}
			}
			if err != nil {
				_ = c.Close(websocket.StatusNormalClosure, "session ended")
				cancel()
				return
			}
		}
	}()

	// websocket -> session stdin (and resize control frames)
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageText {
			var m resizeMsg
			if json.Unmarshal(data, &m) == nil && m.Type == "resize" {
				_ = sess.Resize(ctx, m.Cols, m.Rows)
			}
			continue
		}
		if _, werr := sess.Stdin().Write(data); werr != nil {
			return
		}
	}
}
