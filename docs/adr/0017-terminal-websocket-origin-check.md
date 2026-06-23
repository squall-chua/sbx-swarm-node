# Terminal WebSocket is authorized by an Origin check, not a CSRF token

The Terminal session endpoint (`GET /v1/sandboxes/{id}/terminal`, a WebSocket upgrade) authenticates
with the same session cookie or bearer token as the rest of the API (ADR-0006), but its CSRF defense is
a server-side **same-origin `Origin` allowlist** on the handshake, not the double-submit token used for
REST mutations. A mismatched `Origin` is rejected before the upgrade.

Why: the upgrade is an HTTP `GET`, which the existing double-submit check (applied only to unsafe
methods) lets through, and a browser `WebSocket` cannot set the `X-CSRF-Token` header anyway. Without an
Origin check that combination is textbook Cross-Site WebSocket Hijacking — any page could open a terminal
into a logged-in user's sandbox using the auto-sent cookie. The browser sets `Origin` on the handshake
and page JavaScript cannot forge it, so an allowlist is the correct WS-idiomatic defense. The allowed
origin is the node's own scheme+host (the same-origin SPA); a configurable allowlist covers a
separately-hosted console.

Considered: requiring the CSRF token as a WebSocket subprotocol or query parameter — works for a custom
client but not the browser `WebSocket` API for the token-in-header case, and puts a secret in a URL that
lands in logs. Treating the upgrade as a mutating method in the existing middleware — still can't carry
the token from a browser, so it would block the console outright.
