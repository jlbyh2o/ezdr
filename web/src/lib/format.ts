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
