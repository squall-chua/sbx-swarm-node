// Maps the Provision form to a CreateSandbox request body (snake_case), dropping empty
// optional maps/strings so the server applies its defaults.
// One editable key/value row. The stable `id` keeps the input from remounting
// (and losing focus) while its key is being typed — a Record keyed by the
// in-progress key can't do that, and can't hold blank/duplicate keys mid-edit.
export type KVRow = { id: number; k: string; v: string }

export type ProvisionForm = {
  name: string; agent: string; template: string; cpus: number; memory_bytes: number; disk_gb: number
  workspaces: { name: string; read_only: boolean }[]
  clone: boolean; branch: string; strategy: string
  kits: string[]
  env: KVRow[]; labels: KVRow[]
  node_affinity: KVRow[]; node_anti_affinity: KVRow[]
}

// The daemon canonicalizes an unqualified saved template the way Docker does, so
// "myimage:v1" is listed back (and advertised to peers) as
// "docker.io/library/myimage:v1", and "myorg/myimage:v1" as
// "docker.io/myorg/myimage:v1". Both listed forms are registry-shaped, so
// requesting them tells placement the image can travel to any node — but a
// node that never saved it can't pull it. Requesting the bare form (the tag it
// was actually saved under) places it on the node that actually holds it, so
// strip a leading "docker.io/" — the "library/" segment on top of that is
// Docker's own default-namespace marker and comes off with it. This also
// reshapes a genuine Docker Hub reference like "docker.io/myorg/img:1" into
// "myorg/img:1", pinning it to nodes that already hold it instead of letting
// it travel — the safe reading when the two are ambiguous. An operator who
// means Docker Hub can still type "docker.io/myorg/img:1" directly into the
// template field. A real registry reference like "ghcr.io/org/img:1" is
// untouched.
const DOCKER_HUB_PREFIX = 'docker.io/'
const DOCKER_HUB_LIBRARY_PREFIX = 'docker.io/library/'

export function requestableTemplate(name: string): string {
  if (name.startsWith(DOCKER_HUB_LIBRARY_PREFIX)) return name.slice(DOCKER_HUB_LIBRARY_PREFIX.length)
  if (name.startsWith(DOCKER_HUB_PREFIX)) return name.slice(DOCKER_HUB_PREFIX.length)
  return name
}

export function buildCreateBody(f: ProvisionForm): Record<string, any> {
  const body: Record<string, any> = {
    agent: f.agent, cpus: f.cpus,
    memory_bytes: f.memory_bytes, disk_gb: f.disk_gb,
  }
  if (f.name) body.name = f.name // optional: blank => server derives a display name
  if (f.template) body.template = f.template // optional: sbx uses the agent's default image when omitted
  if (f.workspaces.length) body.workspaces = f.workspaces
  if (f.clone) { body.clone = true; if (f.branch) body.branch = f.branch }
  if (f.strategy) body.strategy = f.strategy
  if (f.kits.length) body.kits = f.kits // kit NAMES; the node resolves each to a reference
  for (const k of ['env', 'labels', 'node_affinity', 'node_anti_affinity'] as const) {
    const rec: Record<string, string> = {}
    for (const { k: key, v } of f[k]) {
      const t = key.trim()
      if (t) rec[t] = v // skip blank keys; last write wins on duplicates
    }
    if (Object.keys(rec).length) body[k] = rec
  }
  return body
}
