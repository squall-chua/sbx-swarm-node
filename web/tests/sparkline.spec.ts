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
  it('scales to a fixed max so small values stay small', () => {
    // 3 of 100 sits near the baseline (y≈19 of 20), not pinned to the top.
    expect(toPoints([3], 100, 20, 100)).toBe('0,19')
    // Without a fixed max the lone value auto-scales and pins to the top (y=0).
    expect(toPoints([3], 100, 20)).toBe('0,0')
  })
})
