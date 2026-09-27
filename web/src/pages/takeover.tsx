import { CheckCircle2, Circle, CircleAlert, Loader2, MinusCircle, TriangleAlert, XCircle } from 'lucide-react'
import { useEffect, useState } from 'react'

import { ErrorAlert } from '@/components/error-alert'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { type Plan, type Takeover, TakeoverState } from '@/gen/ezdr/portal/v1/portal_pb'
import { errorMessage, planClient } from '@/lib/api'
import { formatBytes, formatRelative } from '@/lib/format'

// TakeoverDialog runs the takeover preflight for an adopted plan, shows what
// taking over would do, and after confirmation follows the takeover's
// progress. A takeover that's already running is shown directly.
export function TakeoverDialog({ plan, onClose }: { plan: Plan; onClose: () => void }) {
  // The plan stops adopting once the takeover completes; keep the job names.
  const [adopted] = useState(plan.spec?.takeover)
  // Each preflight run has a number; the result records which run it
  // answers, so a newer run shows as checking.
  const [run, setRun] = useState(0)
  const [result, setResult] = useState<{ run: number; takeover?: Takeover; error?: string }>()
  const [progress, setProgress] = useState<Takeover>()
  const [starting, setStarting] = useState(false)
  const [startError, setStartError] = useState<string>()

  useEffect(() => {
    let active = true
    void (async () => {
      try {
        if (run === 0) {
          const current = await planClient.getTakeover({ id: plan.id })
          if (current.takeover?.state === TakeoverState.RUNNING) {
            if (active) setProgress(current.takeover)
            return
          }
        }
        const r = await planClient.runTakeoverPreflight({ id: plan.id })
        if (active) setResult({ run, takeover: r.takeover })
      } catch (err) {
        if (active) setResult({ run, error: errorMessage(err) })
      }
    })()
    return () => {
      active = false
    }
  }, [plan.id, run])

  // Follow a running takeover.
  const running = progress?.state === TakeoverState.RUNNING
  useEffect(() => {
    if (!running) return
    const timer = setInterval(() => {
      void planClient.getTakeover({ id: plan.id }).then((r) => r.takeover && setProgress(r.takeover), () => undefined)
    }, 2000)
    return () => clearInterval(timer)
  }, [plan.id, running])

  async function start() {
    setStarting(true)
    setStartError(undefined)
    try {
      const r = await planClient.startTakeover({ id: plan.id })
      setProgress(r.takeover)
    } catch (err) {
      setStartError(errorMessage(err))
    } finally {
      setStarting(false)
    }
  }

  const checking = !progress && result?.run !== run
  const takeover = result?.takeover
  const error = checking ? undefined : result?.error
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-3xl">
        <DialogHeader>
          <DialogTitle>Take over the existing zrepl setup</DialogTitle>
          <DialogDescription>
            {plan.spec?.name} replaces <span className="font-mono">{adopted?.sourceJob}</span> on the primary and{' '}
            <span className="font-mono">{adopted?.pullJob}</span> on the DR host.
            {!progress && ' The preflight below changes nothing.'}
          </DialogDescription>
        </DialogHeader>
        {progress ? (
          <ProgressView takeover={progress} />
        ) : (
          <>
            <ErrorAlert message={error} />
            {checking && (
              <div className="flex items-center gap-2 text-sm text-muted-foreground">
                <Loader2 className="size-4 animate-spin" /> Checking snapshots on both hosts…
              </div>
            )}
            {takeover?.preflight && !checking && <PreflightReport takeover={takeover} />}
            {takeover?.state === TakeoverState.READY && !checking && (
              <p className="text-sm text-muted-foreground">
                Taking over upgrades zrepl where needed, removes the old pull job, replaces the old source job with EZDR's, adds
                EZDR's pull job, and checks that the first replication is incremental. If any of that fails, both hosts get
                their original zrepl.yml back and the old jobs run again. Only then are the old jobs' holds and bookmarks
                released.
              </p>
            )}
            <ErrorAlert message={startError} />
          </>
        )}
        <DialogFooter>
          {progress ? (
            <Button variant="outline" onClick={onClose}>
              {running ? 'Close (the takeover continues)' : 'Close'}
            </Button>
          ) : (
            <>
              <Button variant="outline" onClick={() => setRun((n) => n + 1)} disabled={checking || starting}>
                Check again
              </Button>
              <Button onClick={() => void start()} disabled={checking || starting || takeover?.state !== TakeoverState.READY}>
                Take over now
              </Button>
            </>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

const stepIcon: Record<string, React.ReactNode> = {
  pending: <Circle className="size-4 text-muted-foreground" />,
  running: <Loader2 className="size-4 animate-spin" />,
  done: <CheckCircle2 className="size-4 text-emerald-700" />,
  failed: <XCircle className="size-4 text-destructive" />,
  skipped: <MinusCircle className="size-4 text-muted-foreground" />,
}

function ProgressView({ takeover: t }: { takeover: Takeover }) {
  return (
    <div className="grid gap-4 text-sm">
      {t.state === TakeoverState.COMPLETED && (
        <Alert>
          <CheckCircle2 />
          <AlertTitle>Taken over: the plan is active</AlertTitle>
          {t.error && <AlertDescription>{t.error}</AlertDescription>}
        </Alert>
      )}
      {t.state === TakeoverState.ROLLED_BACK && (
        <Alert variant="destructive">
          <CircleAlert />
          <AlertTitle>The takeover failed; both hosts run the old setup again</AlertTitle>
          <AlertDescription>{t.error}</AlertDescription>
        </Alert>
      )}
      {t.state === TakeoverState.FAILED && (
        <Alert variant="destructive">
          <CircleAlert />
          <AlertTitle>The takeover failed and couldn't be fully undone</AlertTitle>
          <AlertDescription>
            {t.error}. See the steps below. Each host's original configuration is kept as /etc/zrepl/zrepl.yml.ezdr-takeover-*.
          </AlertDescription>
        </Alert>
      )}
      <ol className="grid gap-2">
        {t.steps.map((s) => (
          <li key={s.name} className="flex gap-2">
            <span className="mt-0.5">{stepIcon[s.status] ?? stepIcon.pending}</span>
            <div>
              <div className={s.status === 'skipped' ? 'text-muted-foreground' : ''}>{s.name}</div>
              {s.detail && <div className="text-xs break-words text-muted-foreground">{s.detail}</div>}
            </div>
          </li>
        ))}
      </ol>
    </div>
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
