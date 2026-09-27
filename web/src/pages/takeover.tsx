import { CircleAlert, Loader2, TriangleAlert } from 'lucide-react'
import { useEffect, useState } from 'react'

import { ErrorAlert } from '@/components/error-alert'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { type Plan, type Takeover, TakeoverState } from '@/gen/ezdr/portal/v1/portal_pb'
import { errorMessage, planClient } from '@/lib/api'
import { formatBytes, formatRelative } from '@/lib/format'

// TakeoverDialog runs the takeover preflight for an adopted plan and shows
// what taking over would do.
export function TakeoverDialog({ plan, onClose }: { plan: Plan; onClose: () => void }) {
  // Each run of the preflight has a number; the result records which run it
  // answers, so a newer run shows as checking.
  const [run, setRun] = useState(0)
  const [result, setResult] = useState<{ run: number; takeover?: Takeover; error?: string }>()
  useEffect(() => {
    let active = true
    planClient.runTakeoverPreflight({ id: plan.id }).then(
      (r) => active && setResult({ run, takeover: r.takeover }),
      (e) => active && setResult({ run, error: errorMessage(e) }),
    )
    return () => {
      active = false
    }
  }, [plan.id, run])
  const checking = result?.run !== run
  const takeover = result?.takeover
  const error = checking ? undefined : result?.error

  const t = plan.spec?.takeover
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-3xl">
        <DialogHeader>
          <DialogTitle>Take over the existing zrepl setup</DialogTitle>
          <DialogDescription>
            {plan.spec?.name} replaces <span className="font-mono">{t?.sourceJob}</span> on the primary and{' '}
            <span className="font-mono">{t?.pullJob}</span> on the DR host. The preflight below changes nothing.
          </DialogDescription>
        </DialogHeader>
        <ErrorAlert message={error} />
        {checking && (
          <div className="flex items-center gap-2 text-sm text-muted-foreground">
            <Loader2 className="size-4 animate-spin" /> Checking snapshots on both hosts…
          </div>
        )}
        {takeover?.preflight && !checking && <PreflightReport takeover={takeover} />}
        <DialogFooter>
          <Button variant="outline" onClick={() => setRun((n) => n + 1)} disabled={checking}>
            Check again
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function PreflightReport({ takeover }: { takeover: Takeover }) {
  const p = takeover.preflight
  if (!p) return null
  const full = p.datasets.filter((d) => d.fullSend)
  const fullBytes = full.reduce((n, d) => n + d.fullSendBytes, 0n)
  return (
    <div className="grid gap-4 text-sm">
      <div className="text-xs text-muted-foreground">Checked {formatRelative(p.checkedAt)}</div>
      {takeover.state === TakeoverState.BLOCKED && (
        <Alert variant="destructive">
          <CircleAlert />
          <AlertTitle>The takeover can't start</AlertTitle>
          <AlertDescription>
            <ul className="list-disc pl-5">
              {p.problems.map((m) => (
                <li key={m}>{m}</li>
              ))}
            </ul>
          </AlertDescription>
        </Alert>
      )}
      {p.warnings.length > 0 && (
        <Alert>
          <TriangleAlert />
          <AlertDescription>
            <ul className="list-disc pl-5">
              {p.warnings.map((m) => (
                <li key={m}>{m}</li>
              ))}
            </ul>
          </AlertDescription>
        </Alert>
      )}
      <p>
        {p.datasets.length - full.length} of {p.datasets.length} dataset(s) continue incrementally
        {full.length > 0 && `; ${full.length} need a full send (${formatBytes(fullBytes)})`}.
      </p>
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Dataset</TableHead>
            <TableHead>Continues from</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {p.datasets.map((d) => (
            <TableRow key={d.dataset}>
              <TableCell className="font-mono text-xs">{d.dataset}</TableCell>
              <TableCell className="font-mono text-xs">
                {d.fullSend ? <span className="font-sans">full send, {formatBytes(d.fullSendBytes)}</span> : d.commonSnapshot}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
      <div className="grid gap-1">
        <div className="font-medium">zrepl</div>
        <div className="text-muted-foreground">
          Primary {p.primaryZreplVersion || 'not installed'}
          {p.upgradePrimary && ' (will be upgraded to 0.7)'} · DR host {p.drZreplVersion || 'not installed'}
          {p.upgradeDr && ' (will be upgraded to 0.7)'}
        </div>
      </div>
      {p.droppedDatasets.length > 0 && (
        <List title="No longer replicated (replicas stay on the DR host)" items={p.droppedDatasets} mono />
      )}
      {p.otherJobs.length > 0 && <List title="Other hand-written jobs, left in place" items={p.otherJobs} mono />}
      <List
        title={`Released after the first sync: the old jobs' holds and bookmarks (${p.primaryReleases.length + p.drReleases.length})`}
        items={[...p.primaryReleases.map((r) => `primary: ${r}`), ...p.drReleases.map((r) => `DR host: ${r}`)]}
        mono
        collapsed
      />
    </div>
  )
}

function List({ title, items, mono, collapsed }: { title: string; items: string[]; mono?: boolean; collapsed?: boolean }) {
  const body = (
    <ul className={`list-disc space-y-0.5 pl-5 text-muted-foreground ${mono ? 'font-mono text-xs break-all' : ''}`}>
      {items.map((i) => (
        <li key={i}>{i}</li>
      ))}
    </ul>
  )
  if (collapsed) {
    return (
      <details className="grid gap-1">
        <summary className="cursor-pointer font-medium">{title}</summary>
        {body}
      </details>
    )
  }
  return (
    <div className="grid gap-1">
      <div className="font-medium">{title}</div>
      {body}
    </div>
  )
}
