import { describe, it, expect } from 'vitest'
import { toPoints } from '../app/components/Sparkline'

describe('toPoints', () => {
  it('maps values to an SVG polyline over the given box, scaling to max', () => {
    const pts = toPoints([0, 50, 100], 100, 20) // width=100, height=20
    expect(pts).toBe('0,20 50,10 100,0')
  })
  it('handles an empty series', () => {
    expect(toPoints([], 100, 20)).toBe('')
  })
})
