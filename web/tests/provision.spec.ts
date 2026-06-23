import { describe, it, expect } from 'vitest'
import { buildCreateBody } from '../app/components/ProvisionModal'

describe('buildCreateBody', () => {
  it('maps the form to a snake_case CreateSandbox body, dropping empties', () => {
    const body = buildCreateBody({
      agent: 'claude', template: 'base', cpus: 2, memory_bytes: 1073741824, disk_gb: 5,
      workspaces: [{ name: 'repo', read_only: true }],
      clone: true, branch: 'feat/x', strategy: 'bin-pack',
      env: { FOO: 'bar' }, labels: {}, node_affinity: {}, node_anti_affinity: {},
    })
    expect(body.agent).toBe('claude')
    expect(body.workspaces).toEqual([{ name: 'repo', read_only: true }])
    expect(body.clone).toBe(true)
    expect(body.branch).toBe('feat/x')
    expect(body.env).toEqual({ FOO: 'bar' })
    expect('labels' in body).toBe(false)        // empty maps dropped
    expect('node_affinity' in body).toBe(false)
  })
})
