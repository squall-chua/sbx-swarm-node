import { describe, it, expect } from 'vitest'
import { buildWorkspaceCatalog } from '../app/utils/workspaceCatalog'

describe('buildWorkspaceCatalog', () => {
  it('aggregates advertisers, git flag, and mounts by name', () => {
    const nodes = [{ node_name: 'dev-node', workspaces: ['repo', 'plain'], git_workspaces: ['repo'] }]
    const sandboxes = [
      { id: 's1', owner_node: 'dev-node', status: 'running', branch: 'main', workspaces: [{ name: 'repo', read_only: false }] },
      { id: 's2', owner_node: 'dev-node', status: 'stopped', branch: '', workspaces: [{ name: 'repo', read_only: true }] },
    ]
    const rows = buildWorkspaceCatalog(nodes, sandboxes)
    const repo = rows.find(r => r.name === 'repo')!
    expect(repo.providers).toEqual(['dev-node'])
    expect(repo.gitBacked).toBe(true)
    expect(repo.gitMixed).toBe(false)
    expect(repo.mounts).toHaveLength(2)
    expect(repo.mounts[1].readOnly).toBe(true)
    const plain = rows.find(r => r.name === 'plain')!
    expect(plain.gitBacked).toBe(false)
    expect(plain.mounts).toHaveLength(0)
  })

  it('flags gitMixed when advertisers disagree', () => {
    const nodes = [
      { node_name: 'a', workspaces: ['repo'], git_workspaces: ['repo'] },
      { node_name: 'b', workspaces: ['repo'], git_workspaces: [] },
    ]
    const rows = buildWorkspaceCatalog(nodes, [])
    expect(rows[0].gitBacked).toBe(true)
    expect(rows[0].gitMixed).toBe(true)
    expect(rows[0].nonGitProviders).toEqual(['b'])
  })

  it('falls back to mount nodes when no node advertises the workspace', () => {
    const rows = buildWorkspaceCatalog([], [{ id: 's1', owner_node: 'dev-node', workspaces: [{ name: 'ghost' }] }])
    expect(rows[0].providers).toEqual(['dev-node'])
    expect(rows[0].gitBacked).toBe(false)
  })
})
