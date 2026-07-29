# Per-sandbox egress allow — enforcement verified (bug did NOT reproduce)

**Date:** 2026-07-04 · **Daemon:** sbx v0.34.0 (api 0.16.0) · **SDK:** sbx-go-sdk v0.1.7

## Claim under investigation

Field diagnosis (agency `docs/.../2026-07-04-slice-6-manual-real-node-smoke.md`,
Finding 7): a per-sandbox egress allow set via `PolicyService.SetPolicy(scope=<swarmID>)`
*registers* (`sbx policy ls` shows `sandbox:<id> allow <host>`) but does **not** gate a
swarm-managed sandbox's egress; only a node-global allow worked.

Prime hypothesis from the task: `rec.BackendName` (what swarm-node forwards as
`sbx policy allow --sandbox <name>`) does not equal the identity the daemon matches.

## Result: hypothesis DISPROVEN — the mechanism works

On the current daemon, a per-sandbox allow **causally gates egress**. Reproduced live
end-to-end through the node's real REST → gateway → gRPC → manager → SDK → daemon chain.

Captured values (swarm-managed sandbox, agent `shell`, workspace `test`, 1 GiB):

| Identity | Value |
|---|---|
| Swarm ID (`rec.ID`) | `jzrdxd6r3pw3l6py.01KWP3C3TDS4N3PYCBJHWEHGPQ` |
| `rec.BackendName` (forwarded as `--sandbox`) | *same* as swarm ID |
| `sbx ls` SANDBOX column (daemon's identity) | *same* as swarm ID |
| API `name` field (`shell-test-wehgpq`) | cosmetic display name only — not used for `--sandbox` |

So `BackendName == swarm ID == the daemon's sandbox name`. There is no identity mismatch.

Behavioural proof (`curl https://<host>` from *inside* the sandbox, HTTP code):

1. Default policy → **403** (blocked).
2. `PUT /v1/sandboxes/<swarmID>/policy {allow, host}` → **200/404** (reachable). `example.com`→200, `api.minimax.io`→404.
3. `DELETE /v1/sandboxes/<swarmID>/policy/<host>` → **403** again (causal — the scoped rule was the cause).

The **proxy-swap path** (the actual failing path in Finding 7) also works: with a
per-sandbox custom secret set for the host, a request carrying the placeholder was both
**egressed** (200) and **swapped** — `postman-echo.com/headers` echoed
`authorization: Bearer REALSECRET123` in place of `sbx-cs-…`. Egress through the daemon's
MITM secret-swap proxy is enforced at the sandbox scope, not only globally.

## Why the field run failed but this doesn't

Not an identity/scope bug (swarm-node forwards the correct name) and not the daemon
refusing the scope (it enforces). The swarm-node policy code is unchanged since the field
run. The failure is best explained as **environmental at field time** (most likely a stale
long-running daemon process predating the v0.34.0 binary, or opencode's connection to the
provider having stalled on a blackholed pre-allow connection without retry). The node path
itself is provably correct.

## Action taken

- **No swarm-node code change.** Going node-global would violate least-privilege and is
  unnecessary — the per-sandbox scope enforces.
- **Regression test added:** `internal/node/egress_enforcement_integration_test.go`
  (`TestNode_PerSandboxEgress_Enforced`, `//go:build integration`). Asserts *enforcement*
  (403 → reachable → 403), closing the gap in
  `apiserver.TestPolicyService_PerSandboxScope`, which only checks registration against a
  fake. Enforcement lives in the closed daemon, so this is env-gated (red-by-default in CI;
  needs a live daemon + outbound internet). **Verified PASS** against sbx v0.34.0.

## If it recurs

Re-run `go test -tags integration ./internal/node/ -run TestNode_PerSandboxEgress_Enforced`.
If it fails there, the regression is in the daemon (swarm-node forwards the verified-correct
name) — capture `sbx ls`, `sbx policy ls <swarmID>`, and the daemon's running api_version,
and file against the closed `sbx` daemon rather than papering over it with a global rule.
