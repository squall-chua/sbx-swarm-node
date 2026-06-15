# Browser console uses cookie sessions; API clients use bearer tokens

The web console authenticates via an httpOnly / Secure / SameSite=Strict **session cookie**, obtained
by exchanging a known API key at `POST /v1/auth/session`. Programmatic clients authenticate with
`Authorization: Bearer <api_key>`. Both resolve to an API key + role (`admin` / `read-only`). Cookie
mutations are CSRF-protected (double-submit token or a required custom header).

Why: the browser `EventSource` (SSE) API **cannot set request headers**, and the entire live console
runs on SSE (plus a WebSocket terminal). Cookies are sent automatically by `EventSource` and WS
upgrades, and `httpOnly` keeps the token out of reach of XSS — neither is true for a bearer token held
in `localStorage`. API clients have no such constraint, so they keep the simpler header path.

Considered: a custom fetch-based SSE client so the console could use bearer headers everywhere — avoids
cookies and CSRF handling, but requires a bespoke SSE implementation and leaves a JS-readable token.
