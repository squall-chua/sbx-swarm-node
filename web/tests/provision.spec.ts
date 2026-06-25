import { describe, it, expect } from 'vitest'
import { buildCreateBody } from '../app/components/ProvisionModal'

describe('buildCreateBody', () => {
  it('maps the form to a snake_case CreateSandbox body, dropping empties', () => {
    const body = buildCreateBody({
      agent: 'claude', template: 'base', cpus: 2, memory_bytes: 1073741824, disk_gb: 5,
      workspaces: [{ name: 'repo', read_only: true }],
      clone: true, branch: 'feat/x', strategy: 'bin-pack',
      env: [{ id: 0, k: 'FOO', v: 'bar' }], labels: [],
      node_affinity: [], node_anti_affinity: [],
    })
    expect(body.agent).toBe('claude')
    expect(body.workspaces).toEqual([{ name: 'repo', read_only: true }])
    expect(body.clone).toBe(true)
    expect(body.branch).toBe('feat/x')
    expect(body.env).toEqual({ FOO: 'bar' })
    expect('labels' in body).toBe(false)        // empty row lists dropped
    expect('node_affinity' in body).toBe(false)
  })

  it('converts KV rows to a record: trims keys, skips blanks, last write wins', () => {
    const body = buildCreateBody({
      agent: 'shell', template: '', cpus: 1, memory_bytes: 0, disk_gb: 0,
      workspaces: [], clone: false, branch: '', strategy: '',
      env: [
        { id: 0, k: 'FOO', v: '1' },
        { id: 1, k: '', v: 'dropped' },   // blank key dropped
        { id: 2, k: ' BAR ', v: '2' },    // key trimmed
        { id: 3, k: 'FOO', v: 'override' }, // duplicate: last wins
      ],
      labels: [], node_affinity: [], node_anti_affinity: [],
    })
    expect(body.env).toEqual({ FOO: 'override', BAR: '2' })
  })
})
