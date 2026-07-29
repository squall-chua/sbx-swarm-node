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
// "docker.io/library/myimage:v1". That listed form is registry-shaped, so
// requesting it tells placement the image can travel to any node — but a node
// that never saved it can't pull it. Requesting the bare tag places it on the
// node that actually holds it. Strip only that exact prefix: a genuine registry
// reference like "ghcr.io/org/img:1" or "docker.io/org/img:1" must travel as-is.
const DOCKER_LIBRARY_PREFIX = 'docker.io/library/'

export function requestableTemplate(name: string): string {
  return name.startsWith(DOCKER_LIBRARY_PREFIX) ? name.slice(DOCKER_LIBRARY_PREFIX.length) : name
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
