# Swarm identity separate from the join secret

Swarm membership is gated by a shared `cluster_secret` (the `memberlist` keyring), but the secret
alone cannot distinguish one swarm from another. We add a distinct **Swarm ID** (stable UUID) and a
human-readable **swarm name**, propagated via gossip and persisted per node, independent of the
secret.

Why: a node must tell its *previous* swarm from *a new* swarm on rejoin; two unrelated swarms that
happen to share a secret must not silently merge; and partition-heal must verify it is re-meeting the
same swarm. The first node mints the Swarm ID; joiners adopt and persist it. A peer presenting a
different Swarm ID under the same secret is refused with a misconfiguration alert. Adopting a new
Swarm ID requires explicit operator intent.

Considered: secret-only identity — simpler, but leaves "previous vs new swarm" ambiguous and risks a
silent merge of unrelated swarms.
