import { type Timestamp, timestampDate } from '@bufbuild/protobuf/wkt'

export function toDate(ts?: Timestamp): Date | undefined {
  return ts ? timestampDate(ts) : undefined
}

export function formatDateTime(ts?: Timestamp): string {
  const d = toDate(ts)
  return d ? d.toLocaleString() : '—'
}

export function formatRelative(ts?: Timestamp): string {
  const d = toDate(ts)
  if (!d) return 'never'
  const seconds = Math.round((d.getTime() - Date.now()) / 1000)
  const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: 'auto' })
  const abs = Math.abs(seconds)
  if (abs < 60) return rtf.format(seconds, 'second')
  if (abs < 3600) return rtf.format(Math.round(seconds / 60), 'minute')
  if (abs < 86400) return rtf.format(Math.round(seconds / 3600), 'hour')
  return rtf.format(Math.round(seconds / 86400), 'day')
}

export function formatBytes(n: bigint | number): string {
  let v = Number(n)
  if (!v) return '0 B'
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB']
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v >= 10 || i === 0 ? v.toFixed(0) : v.toFixed(1)} ${units[i]}`
}
