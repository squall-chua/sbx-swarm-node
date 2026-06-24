import { describe, it, expect } from 'vitest'
import { useStatus } from '../app/composables/useStatus'

describe('useStatus', () => {
  const s = useStatus()

  // The whole reason there are two maps: "running" means opposite things.
  it('keeps operation and sandbox vocabularies distinct on shared words', () => {
    expect(s.operation('running').color).toBe('warning') // op in progress
    expect(s.sandbox('running').color).toBe('success') // sandbox healthy
  })

  it('falls back to neutral and echoes unknown status as the label', () => {
    expect(s.sandbox('wat')).toMatchObject({ color: 'neutral', label: 'wat' })
    expect(s.operation('wat')).toMatchObject({ color: 'neutral', label: 'wat' })
  })
})
