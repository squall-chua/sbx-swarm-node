# Design — let a custom secret's value be updated

Date: 2026-07-28
Branch: `fix/secret-value-update` (off `main` @ `7909669`)
Probed against: `sbx` v0.37.0 (api `0.24.0`), sbx-go-sdk v0.1.9

## The problem

The node can create a custom secret. It can never change the value of one.

`SetSecret` calls `SDKBackend.SecretSet` (`internal/sandbox/sdkbackend.go:528`),
which calls `sdksecret.SetCustom` without a placeholder. The daemon refuses a
second write to the same `(scope, env)` unless the caller re-supplies the
existing placeholder. So every update fails.

This means secret rotation through the node API does not work. There is no
workaround through the API: delete-then-create would issue a new placeholder,
which changes the env value inside every running sandbox.

## What the probes showed

Run by hand against the live daemon on 2026-07-28. All four writes used a
closed stdin, so nothing below depends on an interactive terminal.

1. First write succeeds and the daemon generates a placeholder.

   ```
   Saved custom secret placeholder "sbx-cs-353FkAEsuibzuZxx" for target
   "pt-probe.example.com" env "PT_PROBE" in scope "(global)"
   ```

2. Second write with **no** placeholder fails loudly, exit 1:

   ```
   ERROR: custom secret env "PT_PROBE" already exists in scope "_"
          with placeholder "sbx-cs-353FkAEsuibzuZxx"
   ```

3. Second write with a **different** placeholder fails the same way.

4. Second write with the **same** placeholder succeeds, exit 0.

A separate probe settled the target-host question. Writing
`--host b.example.com` over an entry that held `a.example.com`, with the same
placeholder, left **only** `b.example.com`. The host list is replaced, not
appended.

### Note on the earlier guess

An earlier note suspected `SetCustom` of hitting a non-interactive overwrite
prompt that silently cancels and exits 0. That is not what happens. The failure
is loud and the exit code is 1. The real fault is the missing placeholder.

## The change

Look the placeholder up before writing. One function, about ten lines.

```go
func (b *SDKBackend) SecretSet(ctx context.Context, scope string, s CustomSecret) error {
	// The daemon rejects a second write to the same (scope, env) unless the caller
	// re-supplies the existing placeholder, so an update needs a read first. Reusing
	// it is also what makes rotation safe: the sandbox env value stays put and only
	// the real secret behind the proxy changes.
	if s.Placeholder == "" {
		// ponytail: on a read failure, fall through and let SetCustom report the
		// real error. That is today's behaviour, so no new failure mode.
		if cur, err := b.SecretList(ctx, scope); err == nil {
			for _, c := range cur.Custom {
				if c.Env == s.Env {
					s.Placeholder = c.Placeholder
					break
				}
			}
		}
	}
	err := sdksecret.SetCustom(ctx, b.cl, scope, sdksecret.CustomSecret{
		Host: s.Host, Env: s.Env, Value: s.Value, Placeholder: s.Placeholder,
	})
	return scrubSecretValue(err, s.Value)
}
```

`CustomSecret` already carries a `Placeholder` field
(`internal/sandbox/backend.go:134`), so no type changes.

`SecretList` already filters rows to the exact scope, and the daemon's
uniqueness rule is per scope, so the two agree.

### Why reuse the placeholder rather than delete and re-create

The placeholder is the value the sandbox actually sees in its env var. Keeping
it means a rotation is invisible inside the sandbox: only the real secret behind
the proxy changes. Deleting and re-creating would issue a new placeholder and
break every sandbox already holding the old one.

### Why the read error is swallowed

Without the lookup an update fails. Returning the read error would replace one
confusing message with another. Falling through leaves today's behaviour intact,
so a failed read is never worse than the status quo.

## The semantics this settles

`SetSecret` is **create or replace this env's entry in this scope**. The key is
`(scope, env)`. It is not "add a host".

Write that into two places, because a reader of `SetSecret(host, env, value)`
could reasonably guess the other meaning:

- the `SetSecretRequest` comment in `proto/sbxswarm/v1/policy.proto:113`,
- the `SetSecret` doc comment in `internal/apiserver/policyservice.go:152`.

### An asymmetry left alone

`SecretSet` keys on env. `SecretRemove` keys on host, because
`sdksecret.RemoveCustom` only accepts a host. Both work as written.

This is safe because a caller reads the list before deleting, so it always holds
the current host. The console does exactly that. Not worth changing.

## The check

Extend `TestSDKBackend_SecretRoundTrip`
(`internal/sandbox/sdkbackend_integration_test.go:267`) with a second
`SecretSet` at a new value. Assert two things:

- the second write returns no error,
- the placeholder read back afterwards is unchanged.

That test today runs Set, List, Remove and never writes twice. That gap is how
the bug survived.

The test is behind the `integration` build tag and an env gate, so it does not
run in CI. This host has no docker and no `sbx` in CI, and every other
daemon-touching test in the repo sits behind the same gate. No new precedent.

## Deliberately not doing

- **No proto field for a caller-supplied placeholder.** The lookup covers every
  caller the node has.
- **No fake-backend model of the daemon's collision rule.** The fake would then
  need to track placeholders, and the integration test already covers the real
  contract.
- **No `findPlaceholder` helper with its own unit test.** The risk here is the
  daemon contract, not a four-line loop.
- **No audit change.** A create and an update both record `secret.set` with the
  host as the target. Telling them apart is a separate ask.

## Out of scope

The same probe session settled three other open questions that are not about
secrets. They need their own home and are not part of this branch:

- Sandbox SSH is **not** reachable off-host. sandboxd listens on a unix socket
  with no TCP port, and published ports bind loopback unless a host IP is given,
  which the node never passes. Not a security hole.
- Disk enforcement is still blocked upstream. `sbx create` on v0.37.0 offers
  `--cpus` and `--memory` and no disk flag.
- Re-advertising templates over gossip is about fifteen lines, because bulk
  fields already propagate on memberlist's default thirty-second push and pull.
  It buys nothing until the node can create a template.
