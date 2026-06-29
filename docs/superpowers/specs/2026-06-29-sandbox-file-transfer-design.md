# Sandbox file transfer (upload/download) — design

**Date:** 2026-06-29 · **Branch:** `m8b-console` · **Status:** approved (grilled), pre-plan
**Glossary:** see `CONTEXT.md` → **File transfer** (upload = operator→sandbox, download =
sandbox→operator; admin-only; not an Operation).

## Goal

Let an admin upload a file into, and download a file out of, a sandbox from the
console (and via the REST API). Transfer-only — single file per request, no
directory browser (deferred). Fills the existing "Files" drawer tab, currently a
"coming soon" placeholder.

## Why it's small

The backend already has the primitives, used today by bundle-publish:

- `Backend().CopyTo(ctx, name, hostPath, containerPath)` — host → container
- `Backend().CopyFrom(ctx, name, containerPath, hostPath)` — container → host

Both shell out to the node's local **`sbx cp`** CLI (Docker-cp semantics) — not a
daemon API call. Two consequences, both verified live (2026-06-29):

- **`sbx cp` auto-starts a stopped sandbox** (like `sbx exec`). So file transfer
  works on a sandbox in any state — no explicit start. Note: a *download* therefore
  **wakes an idle-stopped sandbox** (it then idle-stops again on schedule).
- **Docker-cp naming:** a bare-directory destination makes the file inherit the
  *source* basename — and our source is a random host temp file — so the destination
  must be a full **file** path, never a bare dir.

This feature is "expose those two over HTTP, staged through a host temp file" + UI.
No new backend capability.

## Architecture — clone the terminal handler

A raw HTTP handler `filesMux(handler, next)` intercepting `/v1/sandboxes/{id}/files`,
wired **inside** `OwnerProxy`, exactly where `terminalMux` sits
(`internal/apiserver/server.go`, ~L122). Consequences:

- **Cross-node is free and streamed.** `OwnerProxy` is an `httputil.ReverseProxy`
  with `FlushInterval: -1` (streams the body, no buffering) and forwards the caller's
  bearer/cookie. So a remote sandbox's request is reverse-proxied to its owner; the
  owner re-authenticates the same credential and does the `cp`. No new routing code.
- **Auth context is present.** The auth middleware (`mw.Authenticate`) wraps the
  whole `/v1/` surface, so the handler reads the role via `auth.RoleFromContext`.
- Non-`/files` paths fall through to `next` unchanged.

Raw endpoints (binary), not the grpc-gateway JSON path — same rationale as the
terminal WebSocket.

## Endpoints

### Upload — `PUT /v1/sandboxes/{id}/files?path=<dest>`
- Request body = the raw file bytes (one file/request; no multipart).
- **Destination resolution** ("default to /home/agent unless a full path is given"):
  - `path` absolute (`/...`) → used verbatim as the destination **file** path.
  - `path` relative (e.g. `report.txt`) → `/home/agent/report.txt` (`defaultUploadDir = /home/agent`).
  - `path` empty/omitted → `400`.
  - Reject `..` segments and **trailing-slash / bare-directory** dests (would mis-name
    the file — see Docker-cp note above).
- Flow: stream body → host temp file (`os.CreateTemp`) → `CopyTo(name, temp, dest)`
  → remove temp → `204 No Content`. Bump **Activity** (upload is a control-plane input).
- **Size cap:** `http.MaxBytesReader`, default 100 MiB, config `max_upload_bytes`
  (0 → default), enforced on the **owner** node (where `cp` runs). Over cap → `413`.

### Download — `GET /v1/sandboxes/{id}/files?path=<abs path>`
- `path` must be an absolute container path; else `400`.
- Flow: `CopyFrom(name, path, temp)` → stream temp to the response with
  `Content-Disposition: attachment; filename="<base>"` and
  `Content-Type: application/octet-stream` → remove temp. Does **not** bump Activity
  (download is a read). If the result is not a regular file (e.g. the path is a
  directory) → `400`; missing path → `404`.
- Staged through a host temp file (not end-to-end streamed — `sbx cp` only writes to
  a path). Accepted limitation; bounded by the file's size.

## Decisions

- **Auth: admin-only, both directions**, via `mw.RequireRole("admin", filesHandler)`.
  Download is admin-gated because it exfiltrates; upload because it mutates. `401`
  unauthenticated, `403` non-admin.
- **Path:** arbitrary absolute container path (matches `sbx cp` and the terminal's
  reach). Upload additionally requires a full file path (no bare dir) and rejects `..`.
- **Default upload dir:** `/home/agent` (verified to exist, writable, owned by the
  `agent` user).
- **Activity:** upload **is** Activity (resets idle clock); download is **not**
  (a read, like Get/Publish). Mirrors the glossary.
- **Audit:** both directions — `file.upload` / `file.download` with actor + resolved
  path + outcome.

### Also in scope — close the terminal authz gap
The Terminal handler is currently authN-only: it never checks the role, so a
**read-only key can open a root shell**. Raw handlers bypass the gRPC `authz`
interceptor and must self-enforce. As part of this work, wrap the terminal in
`mw.RequireRole("admin", …)` too, so the auth posture is consistent (a shell is
strictly more powerful than file transfer). One-line change in `server.go`.

## Auth wiring detail

`filesMux` (and `terminalMux`) sit inside `OwnerProxy` and after `mw.Authenticate`,
so requests are authenticated and carry a role. Authorization is enforced by wrapping
each raw handler in `RequireRole("admin", …)` (the gRPC interceptor does not cover
raw handlers).

CSRF: `mw.Authenticate` double-submit-checks every **unsafe** method, so the `PUT`
upload is covered automatically — the UI sends `X-CSRF-Token` like other mutations.
The `GET` download is a safe method, cookie-authenticated, so a plain browser
navigation downloads the file (no CSRF header needed).

Cross-node: for a remote sandbox the request is proxied to the owner, which
re-runs `Authenticate` + `RequireRole` on the forwarded credential — so the admin
gate, CSRF, and size cap all apply on the owner.

## Components & boundaries

| Unit | Responsibility | Depends on |
|------|----------------|-----------|
| `internal/apiserver/files.go` | `filesMux` + the two handlers: resolve/validate path, stage temp, transfer, audit, bump-activity-on-upload | `Backend().CopyTo/CopyFrom`, `mgr.Resolve`/`BumpActivity`, `audit.Log` |
| `server.go` wire-up | `v1 = filesMux(mw.RequireRole("admin", filesHandler), v1)`; wrap terminal in `RequireRole("admin", …)` | `auth.Middleware` |
| config | `max_upload_bytes` knob (0 → 100 MiB) | `internal/config` |
| `FilesTab.vue` | upload/download form, admin gate, toasts | `useApi`, `useSession` |

## UI — `web/app/components/drawer/FilesTab.vue` (replace the placeholder)

Admin-only (show the existing "admin only" alert otherwise, like the Git tab).

- **Upload:** `<input type=file>` + a destination-path field prefilled to
  `/home/agent/<filename>` (editable) → `PUT` the bytes with `X-CSRF-Token`. Toast.
- **Download:** a path field + Download button → browser `GET` (the
  `Content-Disposition` header saves the file).

## Testing

- **Go (Fake backend):** add a `CopyToFunc` hook (`CopyFromFunc` exists). Round-trip a
  real temp file both directions; assert `CopyTo` gets the resolved dest; cover path
  defaulting (relative→/home/agent), `..`/trailing-slash rejection, size-cap `413`,
  non-admin `403`, unauth `401`, missing-path `400`, download `Content-Disposition`,
  upload bumps `LastActivity` / download does not, audit rows for both.
- **Vitest:** FilesTab admin gate; upload posts to the resolved default path; download
  triggers the GET. Terminal-gate regression is Go-side (RequireRole on the route).

## Out of scope (deferred)

Directory browsing/navigation, multi-file/recursive transfer, progress bars,
resumable uploads, end-to-end streaming (staged via host temp), a download size cap
(admin-only, bounded by sandbox contents), uploading into the read-only clone
workspace (writes there fail; target a writable dir like `/home/agent`).
