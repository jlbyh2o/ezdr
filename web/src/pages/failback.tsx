import { CheckCircle2, CircleAlert, Loader2, RefreshCw, TriangleAlert } from 'lucide-react'
import { useCallback, useEffect, useState } from 'react'
import { useNavigate } from 'react-router'

import { DnsTable } from '@/components/dns-table'
import { ErrorAlert } from '@/components/error-alert'
import { StatusIcon } from '@/components/status-icon'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { type Failback, type FailbackPreflight, FailbackState, type Plan } from '@/gen/ezdr/portal/v1/portal_pb'
import { errorMessage, failoverClient } from '@/lib/api'
import { formatBytes, formatDateTime, formatRelative } from '@/lib/format'
import { operationPath } from '@/lib/operations'

const unfinished = [FailbackState.RUNNING, FailbackState.AWAITING_CONFIRMATION, FailbackState.FAILED]

// FailbackDialog checks what failing the plan back would do and starts it,
// then opens its page. A failback that hasn't finished opens its page
// instead.
export function FailbackDialog({ plan, onClose }: { plan: Plan; onClose: () => void }) {
  const navigate = useNavigate()
  // The last failback, when it was undone, to show why.
  const [aborted, setAborted] = useState<Failback>()
  const [preflight, setPreflight] = useState<FailbackPreflight>()
  const [checking, setChecking] = useState(false)
  const [error, setError] = useState<string>()
  const [name, setName] = useState('')
  const [discard, setDiscard] = useState(false)
  const [busy, setBusy] = useState(false)

  const runPreflight = useCallback(async () => {
    setChecking(true)
    setError(undefined)
    try {
      const r = await failoverClient.runFailbackPreflight({ planId: plan.id })
      setPreflight(r.preflight)
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setChecking(false)
    }
  }, [plan.id])

  useEffect(() => {
    let active = true
    void (async () => {
      try {
        const cur = await failoverClient.getFailback({ planId: plan.id })
        if (!active) return
        if (cur.failback && unfinished.includes(cur.failback.state)) {
          navigate(operationPath.failback(plan.id, cur.failback.id))
          return
        }
        if (cur.failback?.state === FailbackState.ABORTED) setAborted(cur.failback)
        await runPreflight()
      } catch (err) {
        if (active) setError(errorMessage(err))
      }
    })()
    return () => {
      active = false
    }
  }, [plan.id, runPreflight, navigate])

  async function start() {
    setBusy(true)
    setError(undefined)
    try {
      const r = await failoverClient.startFailback({ planId: plan.id, confirmName: name, discardDiverged: discard })
      navigate(operationPath.failback(plan.id, r.failback?.id))
    } catch (err) {
      setError(errorMessage(err))
      setBusy(false)
    }
  }

  const planName = plan.appliedSpec?.name ?? plan.spec?.name ?? ''
  const blocked = !preflight || preflight.problems.length > 0 || (preflight.diverged && !discard)
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-3xl">
        <DialogHeader>
          <DialogTitle>Fail back {planName}</DialogTitle>
          <DialogDescription>
            The DR host's changes are copied back to the primary while the guests keep running, then the guests stop on the DR host, the
            last changes are copied, and they start on the primary. Replication resumes afterwards.
          </DialogDescription>
        </DialogHeader>
        <ErrorAlert message={error} />
        <div className="grid gap-4 text-sm">
          {aborted && (
            <Alert variant="destructive">
              <CircleAlert />
              <AlertTitle>The last failback was undone {formatRelative(aborted.completedAt ?? aborted.startedAt)}</AlertTitle>
              <AlertDescription>{aborted.error}</AlertDescription>
            </Alert>
          )}
          {checking && !preflight && (
            <div className="flex items-center gap-2 text-muted-foreground">
              <Loader2 className="size-4 animate-spin" /> Checking both hosts…
            </div>
          )}
          {preflight && <PreflightView preflight={preflight} />}
          {preflight?.diverged && (
            <label className="flex items-start gap-2">
              <input type="checkbox" checked={discard} onChange={(e) => setDiscard(e.target.checked)} />
              <span>
                Discard the primary's changes listed above. They can't be recovered: if you may need them (for example, work
                done just before an outage), back up those guests on the primary first.
              </span>
            </label>
          )}
          {preflight && preflight.problems.length === 0 && (
            <div className="grid gap-2">
              <Label htmlFor="confirm-name">
                Type <span className="font-mono">{planName}</span> to confirm
              </Label>
              <Input id="confirm-name" value={name} onChange={(e) => setName(e.target.value)} autoComplete="off" />
            </div>
          )}
        </div>
        <DialogFooter>
          <Button variant="outline" disabled={checking} onClick={() => void runPreflight()}>
            <RefreshCw /> Check again
          </Button>
          <Button disabled={busy || checking || blocked || name.trim() !== planName} onClick={() => void start()}>
            Fail back now
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

// FailbackActions are what can be done with a failback now: confirm it, or
// retry a failed step.
export function FailbackActions({ failback, onChanged }: { failback: Failback; onChanged: (f: Failback) => void }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()
  async function act(fn: () => Promise<{ failback?: Failback }>) {
    setBusy(true)
    setError(undefined)
    try {
      const r = await fn()
      if (r.failback) onChanged(r.failback)
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setBusy(false)
    }
  }
  const planId = failback.planId
  return (
    <div className="grid justify-items-end gap-2">
      <div className="flex flex-wrap justify-end gap-2">
        {failback.state === FailbackState.AWAITING_CONFIRMATION && (
          <>
            <Button variant="outline" disabled={busy} onClick={() => void act(() => failoverClient.confirmFailback({ planId }))}>
              Confirm without switching DNS
            </Button>
            <Button disabled={busy} onClick={() => void act(() => failoverClient.confirmFailback({ planId, switchDns: true }))}>
              Confirm and switch DNS back
            </Button>
          </>
        )}
        {failback.state === FailbackState.FAILED && (
          <Button disabled={busy} onClick={() => void act(() => failoverClient.retryFailback({ planId }))}>
            Retry
          </Button>
        )}
      </div>
      <ErrorAlert message={error} />
    </div>
  )
}

function PreflightView({ preflight: p }: { preflight: FailbackPreflight }) {
  return (
    <>
      <div className="text-xs text-muted-foreground">Checked {formatDateTime(p.checkedAt)}</div>
      {p.problems.length > 0 && (
        <Alert variant="destructive">
          <CircleAlert />
          <AlertTitle>Failing back isn't possible yet</AlertTitle>
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
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Dataset</TableHead>
            <TableHead>Copied from</TableHead>
            <TableHead>To copy</TableHead>
            <TableHead>Discarded on the primary</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {p.datasets.map((d) => (
            <TableRow key={d.dataset}>
              <TableCell className="font-mono text-xs">{d.dataset}</TableCell>
              <TableCell className="font-mono text-xs">{d.commonSnapshot || '—'}</TableCell>
              <TableCell className="text-xs">about {formatBytes(d.copyBytes)}</TableCell>
              <TableCell className="text-xs">
                {d.discardedSnapshots.length === 0 && d.divergedBytes === 0n ? (
                  'nothing'
                ) : (
                  <span className="text-destructive">
                    {formatBytes(d.divergedBytes)}
                    {d.discardedSnapshots.length > 0 && ` and ${d.discardedSnapshots.join(', ')}`}
                  </span>
                )}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
      {p.configChanges.length > 0 && (
        <div className="grid gap-1">
          <div className="font-medium">Changes made at the DR site</div>
          <div className="text-xs text-muted-foreground">Failing back doesn't carry these over: the primary keeps its own configuration.</div>
          <ul className="list-disc pl-5 text-xs">
            {p.configChanges.map((c) => (
              <li key={c}>{c}</li>
            ))}
          </ul>
        </div>
      )}
    </>
  )
}

// FailbackProgress shows a failback's steps, copy rounds, guests, and DNS
// records.
export function FailbackProgress({ failback: f }: { failback: Failback }) {
  return (
    <div className="grid gap-4 text-sm">
      {f.discardDiverged && <div className="text-xs text-muted-foreground">The primary's diverged data was discarded.</div>}
      {f.state === FailbackState.AWAITING_CONFIRMATION && (
        <Alert>
          <TriangleAlert />
          <AlertTitle>The guests run on the primary again</AlertTitle>
          <AlertDescription>Verify them, then confirm. Switching DNS back points the records below at their production values.</AlertDescription>
        </Alert>
      )}
      {f.state === FailbackState.COMPLETED && (
        <Alert>
          <CheckCircle2 />
          <AlertTitle>Failed back</AlertTitle>
          <AlertDescription>Completed {formatRelative(f.completedAt)}.</AlertDescription>
        </Alert>
      )}
      {f.state === FailbackState.FAILED && (
        <Alert variant="destructive">
          <CircleAlert />
          <AlertTitle>A step failed</AlertTitle>
          <AlertDescription>{f.error}</AlertDescription>
        </Alert>
      )}
      <ol className="grid gap-1">
        {f.steps.map((s, i) => (
          <li key={i} className="flex gap-2">
            <span className="mt-0.5"><StatusIcon status={s.status} /></span>
            <span>
              {s.name}
              {s.detail && <span className="block text-xs break-words text-muted-foreground">{s.detail}</span>}
            </span>
          </li>
        ))}
      </ol>
      {f.rounds.length > 0 && (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Copy</TableHead>
              <TableHead>Snapshot</TableHead>
              <TableHead>Copied</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {f.rounds.map((r, i) => (
              <TableRow key={r.snapshot}>
                <TableCell className="text-xs">{r.final ? 'Final' : `Round ${i + 1}`}</TableCell>
                <TableCell className="font-mono text-xs">{r.snapshot}</TableCell>
                <TableCell className="text-xs">{formatBytes(r.bytes)}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
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
          <summary className="cursor-pointer">Cleanup on the DR host ({f.notes.length})</summary>
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
