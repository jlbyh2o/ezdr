import { CheckCircle2, CircleAlert, Loader2, TriangleAlert } from 'lucide-react'
import { useEffect, useState } from 'react'

import { DnsTable } from '@/components/dns-table'
import { ErrorAlert } from '@/components/error-alert'
import { StatusIcon } from '@/components/status-icon'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import {
  type Failover,
  FailoverState,
  type GetFailoverOptionsResponse,
  type Plan,
  PlanState,
} from '@/gen/ezdr/portal/v1/portal_pb'
import { errorMessage, failoverClient } from '@/lib/api'
import { formatDateTime, formatRelative } from '@/lib/format'


// FailoverDialog starts a failover of the plan, or follows the running one.
export function FailoverDialog({ plan, onClose }: { plan: Plan; onClose: () => void }) {
  const [failover, setFailover] = useState<Failover>()
  const [options, setOptions] = useState<GetFailoverOptionsResponse>()
  const [loaded, setLoaded] = useState(false)
  const [error, setError] = useState<string>()
  const [planned, setPlanned] = useState(true)
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)

  // Show the latest failover if it hasn't finished; otherwise the options.
  useEffect(() => {
    let active = true
    void (async () => {
      try {
        const cur = await failoverClient.getFailover({ planId: plan.id })
        const st = cur.failover?.state
        if (
          cur.failover &&
          (st === FailoverState.RUNNING || st === FailoverState.AWAITING_CONFIRMATION || st === FailoverState.FAILED ||
            plan.state === PlanState.FAILING_OVER ||
            plan.state === PlanState.FAILED_OVER ||
            plan.state === PlanState.FAILING_BACK)
        ) {
          if (active) setFailover(cur.failover)
        } else {
          const o = await failoverClient.getFailoverOptions({ planId: plan.id })
          if (active) {
            setOptions(o)
            setPlanned(o.plannedPossible)
          }
        }
      } catch (err) {
        if (active) setError(errorMessage(err))
      } finally {
        if (active) setLoaded(true)
      }
    })()
    return () => {
      active = false
    }
  }, [plan.id, plan.state])

  // Follow a running failover.
  const running = failover?.state === FailoverState.RUNNING
  useEffect(() => {
    if (!running) return
    const t = setInterval(() => {
      void failoverClient.getFailover({ planId: plan.id }).then((r) => r.failover && setFailover(r.failover), () => undefined)
    }, 2000)
    return () => clearInterval(t)
  }, [plan.id, running])

  async function act(fn: () => Promise<{ failover?: Failover }>) {
    setBusy(true)
    setError(undefined)
    try {
      const r = await fn()
      if (r.failover) setFailover(r.failover)
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setBusy(false)
    }
  }

  const planName = plan.appliedSpec?.name ?? plan.spec?.name ?? ''
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-3xl">
        <DialogHeader>
          <DialogTitle>Fail over {planName}</DialogTitle>
          <DialogDescription>
            The guests start on the DR host from their replicas. Replication stops, and the primary's copies are stopped and locked.
          </DialogDescription>
        </DialogHeader>
        <ErrorAlert message={error} />
        {!loaded && <Loader2 className="size-4 animate-spin" />}
        {failover ? (
          <Progress failover={failover} />
        ) : (
          options && (
            <div className="grid gap-4 text-sm">
              {options.problem && (
                <Alert variant="destructive">
                  <CircleAlert />
                  <AlertDescription>{options.problem}</AlertDescription>
                </Alert>
              )}
              <div className="grid gap-2">
                <label className="flex items-start gap-2">
                  <input type="radio" name="kind" disabled={!options.plannedPossible} checked={planned} onChange={() => setPlanned(true)} />
                  <span>
                    <span className="font-medium">Planned</span>: shut the guests down on the primary, replicate their final state, then start
                    them on the DR host. No data is lost.
                    {!options.plannedPossible && (
                      <span className="block text-xs text-muted-foreground">Needs an active plan and a connected primary.</span>
                    )}
                  </span>
                </label>
                <label className="flex items-start gap-2">
                  <input type="radio" name="kind" checked={!planned} onChange={() => setPlanned(false)} />
                  <span>
                    <span className="font-medium">Unplanned</span>: start the guests on the DR host from the newest replicated snapshot
                    {options.newestSnapshotAt && ` (${formatDateTime(options.newestSnapshotAt)}, ${formatRelative(options.newestSnapshotAt)})`}.
                    Changes after it are lost. The primary's copies are locked when it's reachable.
                  </span>
                </label>
              </div>
              <div className="text-muted-foreground">
                Primary {options.primaryOnline ? 'connected' : 'not connected'} · DR host {options.drOnline ? 'connected' : 'not connected'}
              </div>
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Guest</TableHead>
                    <TableHead>Startup order</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {options.guests.map((g) => (
                    <TableRow key={g.vmid}>
                      <TableCell>
                        <span className="font-mono text-xs">{g.vmid}</span> {g.name}
                        {g.problem && <div className="text-xs text-destructive">{g.problem}</div>}
                      </TableCell>
                      <TableCell className="text-xs">{g.startupOrder}</TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
              {options.dnsRecords.length > 0 && <DnsTable records={options.dnsRecords} />}
              <div className="grid gap-2">
                <Label htmlFor="confirm-name">
                  Type <span className="font-mono">{planName}</span> to confirm
                </Label>
                <Input id="confirm-name" value={name} onChange={(e) => setName(e.target.value)} autoComplete="off" />
              </div>
            </div>
          )
        )}
        <DialogFooter>
          {failover?.state === FailoverState.AWAITING_CONFIRMATION && (
            <>
              <Button variant="outline" disabled={busy} onClick={() => void act(() => failoverClient.confirmFailover({ planId: plan.id }))}>
                Confirm without switching DNS
              </Button>
              <Button
                disabled={busy}
                onClick={() => void act(() => failoverClient.confirmFailover({ planId: plan.id, switchDns: true }))}
              >
                Confirm and switch DNS
              </Button>
            </>
          )}
          {failover?.state === FailoverState.FAILED && (
            <Button disabled={busy} onClick={() => void act(() => failoverClient.retryFailover({ planId: plan.id }))}>
              Retry
            </Button>
          )}
          {!failover && options && (
            <Button
              variant="destructive"
              disabled={busy || !!options.problem || name.trim() !== planName}
              onClick={() => void act(() => failoverClient.startFailover({ planId: plan.id, planned, confirmName: name }))}
            >
              Fail over now
            </Button>
          )}
          <Button variant="outline" onClick={onClose}>
            {running ? 'Close (the failover continues)' : 'Close'}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function Progress({ failover: f }: { failover: Failover }) {
  return (
    <div className="grid gap-4 text-sm">
      <div className="text-xs text-muted-foreground">
        {f.planned ? 'Planned' : 'Unplanned'} failover started {formatRelative(f.startedAt)} by {f.startedBy}
        {f.breakGlass && ' on the DR host (break-glass)'}
        {f.snapshot && ` · final snapshot ${f.snapshot}`}
      </div>
      {f.state === FailoverState.AWAITING_CONFIRMATION && (
        <Alert>
          <TriangleAlert />
          <AlertTitle>The guests run on the DR host</AlertTitle>
          <AlertDescription>Verify them, then confirm. Switching DNS points the records below at the DR site.</AlertDescription>
        </Alert>
      )}
      {f.state === FailoverState.COMPLETED && (
        <Alert>
          <CheckCircle2 />
          <AlertTitle>Failed over</AlertTitle>
          <AlertDescription>Completed {formatRelative(f.completedAt)}.</AlertDescription>
        </Alert>
      )}
      {(f.state === FailoverState.ABORTED || f.state === FailoverState.FAILED) && (
        <Alert variant="destructive">
          <CircleAlert />
          <AlertTitle>{f.state === FailoverState.ABORTED ? 'The failover was undone' : 'A step failed'}</AlertTitle>
          <AlertDescription>{f.error}</AlertDescription>
        </Alert>
      )}
      <ol className="grid gap-1">
        {f.steps.map((s, i) => (
          <li key={i} className="flex gap-2">
            <span className="mt-0.5"><StatusIcon status={s.status} /></span>
            <span>
              {s.name}
              {s.detail && <span className="block text-xs text-muted-foreground break-words">{s.detail}</span>}
            </span>
          </li>
        ))}
      </ol>
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Guest</TableHead>
            <TableHead>Result</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {f.guests.map((g) => (
            <TableRow key={g.vmid}>
              <TableCell>
                <span className="font-mono text-xs">{g.vmid}</span> {g.name}
              </TableCell>
              <TableCell className="text-xs">
                <span className="flex items-center gap-1">
                  <StatusIcon status={g.status === 'running' ? 'done' : g.status} /> {g.status}
                </span>
                {g.detail && <div className="text-destructive">{g.detail}</div>}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
      {f.dnsRecords.length > 0 && <DnsTable records={f.dnsRecords} />}
      {f.notes.length > 0 && (
        <details className="text-xs">
          <summary className="cursor-pointer">Settings removed from guests ({f.notes.length})</summary>
          <ul className="list-disc pl-5 font-mono text-muted-foreground">
            {f.notes.map((n) => (
              <li key={n}>{n}</li>
            ))}
          </ul>
        </details>
      )}
    </div>
  )
}
