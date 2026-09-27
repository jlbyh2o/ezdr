import { encode } from 'uqr'

// QrCode renders a QR code as SVG elements (no inline scripts or data URLs,
// so it works under the portal's Content Security Policy).
export function QrCode({ value, size = 192 }: { value: string; size?: number }) {
  const { data } = encode(value, { border: 2 })
  const n = data.length
  return (
    <svg
      viewBox={`0 0 ${n} ${n}`}
      width={size}
      height={size}
      shapeRendering="crispEdges"
      role="img"
      aria-label="QR code"
      className="rounded-md bg-white"
    >
      <rect width={n} height={n} fill="white" />
      {data.flatMap((row, y) =>
        row.map((dark, x) => (dark ? <rect key={`${x},${y}`} x={x} y={y} width={1} height={1} fill="black" /> : null)),
      )}
    </svg>
  )
}
