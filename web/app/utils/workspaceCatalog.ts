export interface WorkspaceMount {
  id: string
  node: string
  status: string
  branch: string
  readOnly: boolean
}

export interface WorkspaceRow {
  name: string
  providers: string[]
  gitBacked: boolean
  gitMixed: boolean
  nonGitProviders: string[]
  mounts: WorkspaceMount[]
}

interface NodeLike { node_name: string, workspaces?: string[], git_workspaces?: string[] }
interface MountLike { name: string, read_only?: boolean }
interface SandboxLike { id: string, owner_node?: string, status?: string, branch?: string, workspaces?: MountLike[] }

// buildWorkspaceCatalog groups workspaces by logical name across the swarm:
// advertisers (and which of them mark it git-backed) from nodes, mounts from
// sandboxes. A workspace seen only via a mount (no advertiser) still gets a row.
export function buildWorkspaceCatalog(nodes: NodeLike[], sandboxes: SandboxLike[]): WorkspaceRow[] {
  type Acc = { providers: Set<string>, gitProviders: Set<string>, nonGitProviders: Set<string>, mounts: WorkspaceMount[] }
  const map = new Map<string, Acc>()
  const ensure = (name: string): Acc => {
    let e = map.get(name)
    if (!e) { e = { providers: new Set(), gitProviders: new Set(), nonGitProviders: new Set(), mounts: [] }; map.set(name, e) }
    return e
  }

  for (const n of nodes ?? []) {
    const git = new Set(n.git_workspaces ?? [])
    for (const w of n.workspaces ?? []) {
      const e = ensure(w)
      e.providers.add(n.node_name)
      ;(git.has(w) ? e.gitProviders : e.nonGitProviders).add(n.node_name)
    }
  }
  for (const s of sandboxes ?? []) {
    for (const m of s.workspaces ?? []) {
      ensure(m.name).mounts.push({
        id: s.id,
        node: s.owner_node ?? '',
        status: s.status ?? '',
        branch: s.branch ?? '',
        readOnly: !!m.read_only,
      })
    }
  }

  const rows: WorkspaceRow[] = []
  for (const [name, e] of map) {
    const providers = e.providers.size
      ? [...e.providers]
      : [...new Set(e.mounts.map(m => m.node).filter(Boolean))]
    rows.push({
      name,
      providers: providers.sort(),
      gitBacked: e.gitProviders.size > 0,
      gitMixed: e.gitProviders.size > 0 && e.nonGitProviders.size > 0,
      nonGitProviders: [...e.nonGitProviders].sort(),
      mounts: e.mounts,
    })
  }
  return rows.sort((a, b) => a.name.localeCompare(b.name))
}
