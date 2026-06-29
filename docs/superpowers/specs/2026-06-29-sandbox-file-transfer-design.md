# Sandbox file transfer (upload/download) — design

**Date:** 2026-06-29 · **Branch:** `m8b-console` · **Status:** approved, pre-plan

## Goal

Let an admin upload a file into, and download a file out of, a sandbox from the
console (and via the REST API). Transfer-only — no directory browser (deferred).
Fills the existing "Files" drawer tab, currently a "coming soon" placeholder.

## Why it's small

The backend already has the primitives, used today by bundle-publish:

- `Backend().CopyTo(ctx, name, hostPath, containerPath)` — host → container
- `Backend().CopyFrom(ctx, name, containerPath, hostPath)` — container → host

This feature is "expose those two over HTTP, staged through a host temp file" plus
the UI. No new backend capability.

## Architecture — mirror the terminal handler

A raw HTTP handler `filesMux(handler, next)` intercepting `/v1/sandboxes/{id}/files`,
wired **inside** `OwnerProxy`, exactly where `terminalMux` sits
(`internal/apiserver/server.go`, ~L121). Consequences:

- **Cross-node is free.** A remote sandbox's request is reverse-proxied (body
  stream included) to its owning node by `OwnerProxy` before `filesMux` sees it;
  the owner's local backend does the copy. No new routing code.
- **Auth context is present.** The HTTP auth middleware populates the role;
  the handler reads it with `auth.RoleFromContext(r.Context())`.
- Non-`/files` paths fall through to `next` unchanged.

These are raw endpoints (binary), not the grpc-gateway JSON path — same rationale
as the terminal WebSocket.

## Endpoints

### Upload — `PUT /v1/sandboxes/{id}/files?path=<dest>`
- Request body = the raw file bytes (one file per request; no multipart).
- **Destination resolution** (honoring "default to /home/agent unless a full path
  is given"):
  - `path` absolute (`/...`) → used verbatim as the destination file path.
  - `path` relative (e.g. `report.txt`) → resolved to `/home/agent/report.txt`.
  - `path` empty/omitted → `400` (a name is required).
  - Reject `..` segments.
- Flow: stream body → host temp file (`os.CreateTemp`) → `CopyTo(name, temp, dest)`
  → remove temp → `204 No Content`.
- **Size cap:** `http.MaxBytesReader`, default 100 MiB, config knob
  `max_upload_bytes` (0 = default). Over cap → `413`.

### Download — `GET /v1/sandboxes/{id}/files?path=<abs path>`
- `path` must be an absolute container path; else `400`.
- Flow: `CopyFrom(name, path, temp)` → stream temp to the response with
  `Content-Disposition: attachment; filename="<base>"` and
  `Content-Type: application/octet-stream` → remove temp.
- Errors map to `404` (no such file) / `500`.

## Decisions

- **Auth: admin-only, both directions.** Consistent with the Terminal tab
  (admin-only, strictly more powerful). Read-only users get neither. The handler
  returns `403` for a non-admin role, `401` if unauthenticated.
- **Path: arbitrary absolute container path**, matching `sbx cp` and the terminal's
  access level. The daemon enforces container boundaries; the host side is always a
  temp file we name (no host traversal risk). Upload additionally rejects `..`.
- **Default upload dir:** `/home/agent` (const `defaultUploadDir`).
- **Audit:** `file.upload` / `file.download` with actor + resolved path, like
  `git.publish`.

## UI — `web/app/components/drawer/FilesTab.vue` (replace the placeholder)

Admin-only (show the existing "admin only" alert otherwise, like the Git tab).

- **Upload:** `<input type=file>` + a destination-path field prefilled to
  `/home/agent/<filename>` (editable) → `PUT` the file bytes. Toast on done/error.
- **Download:** a path field + Download button → browser `GET`
  (the `Content-Disposition` header makes the browser save the file).

## Auth wiring detail

`filesMux` sits inside `OwnerProxy` and after the HTTP auth middleware, so the
request is authenticated and carries a role. The handler enforces `admin` itself
(raw handlers are not covered by the gRPC `authz` interceptor) → `401` if
unauthenticated, `403` if the role is not admin.

CSRF is handled by the existing auth middleware (`internal/auth/auth.go`): it
double-submit-checks every **unsafe** method, so the `PUT` upload is covered
automatically — the UI sends `X-CSRF-Token` (matching the readable CSRF cookie)
just like the other mutating routes; no route-list change. The `GET` download is a
safe method, authenticated by the httpOnly session cookie, so a plain browser
navigation/link downloads the file (cookie sent automatically, no CSRF header
needed).

## Components & boundaries

| Unit | Responsibility | Depends on |
|------|----------------|-----------|
| `internal/apiserver/files.go` | `filesMux` + the two handlers: parse/validate path, stage temp, role-gate, audit | `Backend().CopyTo/CopyFrom`, `auth.RoleFromContext`, `audit.Log` |
| `server.go` wire-up | one line: `v1 = filesMux(filesHandler, v1)` before `OwnerProxy` | — |
| config | `max_upload_bytes` knob | `internal/config` |
| `FilesTab.vue` | upload/download form, admin gate, toasts | `useApi`, `useSession` |

## Testing

- **Go (Fake backend):** add a `CopyToFunc` hook to the Fake (`CopyFromFunc` already
  exists). Round-trip a real temp file both directions; assert `CopyTo` receives the
  resolved dest; cover path defaulting (relative→/home/agent), `..` rejection,
  size-cap `413`, non-admin `403`, missing-path `400`, download `Content-Disposition`.
- **Vitest:** FilesTab renders the admin gate; upload posts to the right URL with the
  resolved default path; download triggers the GET.

## Out of scope (deferred)

Directory browsing/navigation, multi-file/recursive transfer, progress bars,
resumable uploads, uploading into the read-only clone workspace (writes there fail;
target a writable dir like `/home/agent`).
