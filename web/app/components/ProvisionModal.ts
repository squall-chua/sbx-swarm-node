// Maps the Provision form to a CreateSandbox request body (snake_case), dropping empty
// optional maps/strings so the server applies its defaults.
export type ProvisionForm = {
  agent: string; template: string; cpus: number; memory_bytes: number; disk_gb: number
  workspaces: { name: string; read_only: boolean }[]
  clone: boolean; branch: string; strategy: string
  env: Record<string, string>; labels: Record<string, string>
  node_affinity: Record<string, string>; node_anti_affinity: Record<string, string>
}

export function buildCreateBody(f: ProvisionForm): Record<string, any> {
  const body: Record<string, any> = {
    agent: f.agent, template: f.template, cpus: f.cpus,
    memory_bytes: f.memory_bytes, disk_gb: f.disk_gb,
  }
  if (f.workspaces.length) body.workspaces = f.workspaces
  if (f.clone) { body.clone = true; if (f.branch) body.branch = f.branch }
  if (f.strategy) body.strategy = f.strategy
  for (const k of ['env', 'labels', 'node_affinity', 'node_anti_affinity'] as const) {
    if (Object.keys(f[k]).length) body[k] = f[k]
  }
  return body
}
