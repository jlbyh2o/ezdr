import { useState } from 'react'
import { Link } from 'react-router'

import { Button } from '@/components/ui/button'
import type { GuestExclusion } from '@/gen/ezdr/portal/v1/portal_pb'
import { formatDateTime } from '@/lib/format'

// GuestStatus says whether a guest is protected, and lets a guest in no
// plan be marked unprotected (a deliberate choice, so plans stop warning)
// or back.
export function GuestStatus({
  protectedHere,
  inOtherPlan,
  plan,
  template,
  exclusion,
  setExcluded,
}: {
  protectedHere: boolean
  inOtherPlan: boolean
  // The protecting plan, to link to it.
  plan?: { id: string; name: string }
  template: boolean
  exclusion?: GuestExclusion
  setExcluded: (on: boolean) => Promise<void>
}) {
  const [busy, setBusy] = useState(false)
  const run = async (on: boolean) => {
    setBusy(true)
    await setExcluded(on)
    setBusy(false)
  }
  // A protected guest can also carry the mark (for example, if a plan adopted
  // it): show it, so it can be undone.
  const marked = exclusion && (
    <>
      <span className="text-muted-foreground" title={`Marked by ${exclusion.by}, ${formatDateTime(exclusion.at)}`}>
        · marked unprotected
      </span>
      <Button variant="ghost" size="xs" disabled={busy} onClick={() => void run(false)}>
        Undo
      </Button>
    </>
  )
  if (template) return <span className="text-muted-foreground">Template</span>
  if (plan) {
    return (
      <span className="inline-flex items-center gap-2">
        <Link to={`/plans/${plan.id}`} className="text-success hover:underline">
          Protected by {plan.name}
        </Link>
        {marked}
      </span>
    )
  }
  if (protectedHere || inOtherPlan) {
    return (
      <span className="inline-flex items-center gap-2">
        <span className="text-success">Protected</span>
        {marked}
      </span>
    )
  }
  if (exclusion) {
    return (
      <span className="inline-flex items-center gap-2">
        <span className="text-muted-foreground" title={`Marked by ${exclusion.by}, ${formatDateTime(exclusion.at)}`}>
          Unprotected
        </span>
        <Button variant="ghost" size="xs" disabled={busy} onClick={() => void run(false)}>
          Undo
        </Button>
      </span>
    )
  }
  return (
    <span className="inline-flex items-center gap-2">
      <span className="text-warning-foreground">Unconfigured</span>
      <Button variant="outline" size="xs" disabled={busy} onClick={() => void run(true)} title="Stop warning about this guest">
        Mark unprotected
      </Button>
    </span>
  )
}
