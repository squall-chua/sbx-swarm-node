// Maps a numeric series to an SVG polyline "points" string over a width x height box.
// y is inverted (SVG origin top-left); scales to the series max (min 1 to avoid /0).
export function toPoints(values: number[], width: number, height: number): string {
  if (values.length === 0) return ''
  const max = Math.max(1, ...values)
  const step = values.length === 1 ? 0 : width / (values.length - 1)
  return values
    .map((v, i) => `${Math.round(i * step)},${Math.round(height - (v / max) * height)}`)
    .join(' ')
}
