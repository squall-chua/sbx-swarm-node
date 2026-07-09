# A PublishWork deliverable is keyed by (workspace, source, target), not the sandbox

A PublishWork produces one upstream **deliverable** — a pull request, merge request, or Gerrit change.
Its identity is the tuple **(workspace, Source branch, target)**. Re-publishing the same Source branch
to the same target updates the same deliverable in place (a PR/MR PATCH, a Gerrit new patchset) rather
than creating a second one — and this holds even when the second publish comes from a *different*
sandbox. The sandbox id is deliberately **not** part of the key.

For `pull_request` we look up the open PR by `(head=owner:source, base=target)`; for `merge_request`
by `(source_branch, target_branch, state=opened)`; for `gerrit_change` we derive the `Change-Id`
trailer as `I<sha1(remoteURL \0 source \0 target)>` so a re-push lands on the same change. All three
therefore answer the same question — "is there already an open deliverable for this source→target on
this workspace?" — and give the same in-place-update behavior.

Why sandbox-independent: GitHub already *forces* it — you cannot open a second open PR for the same
`(head, base)`, so a sandbox-scoped key is impossible to honor there anyway. Rather than have Gerrit
(which does not enforce it) mean something different from GitHub, we make one uniform rule. It also
matches how the work actually reaches the remote: a same-repo publish pushes the Source branch to
`origin` under its own name, so two sandboxes using the same branch name already overwrite each other's
pushed branch — collapsing them onto one deliverable is consistent with that, not new breakage.

Trade-off: two sandboxes that happen to use the same Source branch name targeting the same base share
one deliverable instead of getting isolated ones. We accept this because distinct tasks already use
distinct branch names in practice, and the alternative (a per-sandbox key) cannot be honored on GitHub
and would make Gerrit and GitHub diverge. This is hard to reverse once changes carry the derived
Change-Id: the derivation is what makes re-publishes idempotent, so changing the key later would orphan
every change already pushed under the old derivation.
