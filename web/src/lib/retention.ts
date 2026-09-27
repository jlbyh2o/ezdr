import { create } from '@bufbuild/protobuf'

import { type RetentionTier, RetentionTierSchema } from '@/gen/ezdr/plan/v1/plan_pb'

const HOUR = 3600
const DAY = 24 * HOUR

const t = (count: number, periodSeconds: number, keepAll = false) =>
  create(RetentionTierSchema, { count, periodSeconds, keepAll })

// Mirrors the presets in internal/plan/retention.go.
export const drPresets: { label: string; tiers: () => RetentionTier[] }[] = [
  { label: 'Balanced', tiers: () => [t(1, DAY, true), t(14, DAY), t(8, 7 * DAY)] },
  { label: 'Hourly and daily', tiers: () => [t(1, HOUR, true), t(24, HOUR), t(14, DAY)] },
  { label: 'Minimal', tiers: () => [t(1, DAY, true), t(7, DAY)] },
  { label: 'Extended', tiers: () => [t(1, DAY, true), t(14, DAY), t(8, 7 * DAY), t(12, 30 * DAY)] },
]

export const primaryPresets: { label: string; tiers: () => RetentionTier[] }[] = [
  { label: '24 hours', tiers: () => [t(1, DAY, true)] },
  { label: 'Hourly and 7 days', tiers: () => [t(1, HOUR, true), t(24, HOUR), t(7, DAY)] },
  { label: '1 hour', tiers: () => [t(1, HOUR, true)] },
]

export const units = [
  { label: 'minutes', seconds: 60 },
  { label: 'hours', seconds: HOUR },
  { label: 'days', seconds: DAY },
  { label: 'weeks', seconds: 7 * DAY },
]

// splitPeriod picks the largest unit that divides the period evenly.
export function splitPeriod(seconds: number): { value: number; unit: number } {
  for (const u of [...units].reverse()) {
    if (seconds > 0 && seconds % u.seconds === 0) return { value: seconds / u.seconds, unit: u.seconds }
  }
  return { value: seconds / 60, unit: 60 }
}

export function duration(seconds: number): string {
  if (seconds && seconds % DAY === 0) return `${seconds / DAY}d`
  if (seconds && seconds % HOUR === 0) return `${seconds / HOUR}h`
  if (seconds && seconds % 60 === 0) return `${seconds / 60}m`
  return `${seconds}s`
}

// grid renders tiers in zrepl's grid syntax, like internal/plan.Grid.
export function grid(tiers: RetentionTier[]): string {
  return tiers.map((x) => `${x.count}x${duration(x.periodSeconds)}${x.keepAll ? '(keep=all)' : ''}`).join(' | ')
}

export function describeInterval(seconds: number): string {
  if (seconds % HOUR === 0) return seconds === HOUR ? 'every hour' : `every ${seconds / HOUR} hours`
  if (seconds % 60 === 0) return seconds === 60 ? 'every minute' : `every ${seconds / 60} minutes`
  return `every ${seconds} seconds`
}
