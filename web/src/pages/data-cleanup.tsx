import { CheckCircle2, CircleAlert, Loader2 } from 'lucide-react'
import { useEffect, useState } from 'react'
import { useNavigate } from 'react-router'

import { ErrorAlert } from '@/components/error-alert'
import { StatusBadge } from '@/components/status-badge'
import { StatusIcon } from '@/components/status-icon'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import {
  type DataCleanup,
  DataCleanupKind,
  type DataCleanupPreview,
  DataCleanupState,
  type Plan,
} from '@/gen/ezdr/portal/v1/portal_pb'
import { errorMessage, planClient } from '@/lib/api'
import { formatBytes, formatRelative } from '@/lib/format'
import { usePoll } from '@/lib/use-poll'

// Cleaning up replicated data: deleting a plan's data with the plan, and
// removing what a takeover's old zrepl jobs left (docs/design/cleanup.md).

// CleanupPreview lists what a cleanup removes on each host.
export function CleanupPreview({ preview }: { preview: DataCleanupPreview }) {
  return (
    <div className="grid gap-4 text-sm">
      {preview.problems.length > 0 && (
        <Alert variant="destructive">
          <CircleAlert />
          <AlertTitle>Nothing can be removed until these are resolved</AlertTitle>
          <AlertDescription>
            <ul className="list-disc pl-5">
              {preview.problems.map((p) => (
                <li key={p} className="break-words">
                  {p}
                </li>
              ))}
            </ul>
          </AlertDescription>
        </Alert>
      )}
      {preview.hosts.map((h) => (
        <div key={h.hostId} className="grid gap-1">
          <div className="font-medium">
            {h.hostname} <span className="font-normal text-muted-foreground">({h.role})</span>
            {h.datasets.length > 0 && <span className="font-normal text-muted-foreground"> · frees {formatBytes(h.reclaimBytes)}</span>}
          </div>
          {h.datasets.length === 0 ? (
            <p className="text-muted-foreground">Nothing to remove.</p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Dataset</TableHead>
                  <TableHead>Removes</TableHead>
                  <TableHead className="text-right">Space</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {h.datasets.map((d) => (
                  <TableRow key={d.dataset}>
                    <TableCell className="font-mono text-xs break-all whitespace-normal">
                      {d.dataset}
                      {d.skipped.map((s) => (
                        <div key={s} className="font-sans text-muted-foreground">
                          Kept: {s}
                        </div>
                      ))}
                      {d.problems.map((p) => (
                        <div key={p} className="font-sans text-destructive">
                          {p}
                        </div>
                      ))}
                    </TableCell>
                    <TableCell className="text-xs">
                      {d.destroy
                        ? `the dataset and its ${d.snapshots} snapshot(s)`
                        : `${d.snapshots} snapshot(s)${d.bookmarks ? ` and ${d.bookmarks} bookmark(s)` : ''} named ${d.prefix}*`}
                    </TableCell>
                    <TableCell className="text-right text-xs">{formatBytes(d.reclaimBytes)}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
          {h.releaseJobs.length > 0 && (
            <p className="text-xs text-muted-foreground">
              First releases the holds and cursors of zrepl job(s) {h.releaseJobs.join(', ')}.
            </p>
          )}
          {h.notes.map((n) => (
            <p key={n} className="text-xs text-muted-foreground">
              Left alone: {n}
            </p>
          ))}
        </div>
      ))}
    </div>
  )
}

// CleanupSteps shows a cleanup's steps and outcome.
function CleanupSteps({ cleanup: c }: { cleanup: DataCleanup }) {
  return (
    <ol className="grid gap-2 text-sm">
      {c.steps.map((s, i) => (
        <li key={i} className="flex gap-2">
          <span className="mt-0.5">
            <StatusIcon status={s.status} />
          </span>
          <div className="min-w-0">
            <div className={s.status === 'skipped' ? 'text-muted-foreground' : ''}>{s.name}</div>
            {s.detail && <div className="text-xs break-words text-muted-foreground">{s.detail}</div>}
          </div>
        </li>
      ))}
    </ol>
  )
}

// CleanupActions offers retrying or canceling a failed cleanup.
function CleanupActions({ cleanup, onChanged, cancelLabel }: { cleanup: DataCleanup; onChanged: () => void; cancelLabel: string }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()
  if (cleanup.state !== DataCleanupState.FAILED) return null
  async function act(fn: () => Promise<unknown>) {
    setBusy(true)
    setError(undefined)
    try {
      await fn()
      onChanged()
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setBusy(false)
    }
  }
  const req = { planId: cleanup.planId, kind: cleanup.kind }
  return (
    <div className="grid gap-2">
      <div className="flex flex-wrap gap-2">
        <Button onClick={() => void act(() => planClient.retryDataCleanup(req))} disabled={busy}>
          Retry
        </Button>
        <Button variant="outline" onClick={() => void act(() => planClient.cancelDataCleanup(req))} disabled={busy}>
          {cancelLabel}
        </Button>
      </div>
      <ErrorAlert message={error} />
    </div>
  )
}

// DeletePlanDialog deletes a deactivated plan, and optionally its
// replicated data first.
export function DeletePlanDialog({ plan, onClose, onDeleted }: { plan: Plan; onClose: () => void; onDeleted: () => void }) {
  const name = plan.spec?.name ?? ''
  const [withData, setWithData] = useState(false)
  const [preview, setPreview] = useState<DataCleanupPreview>()
  const [previewError, setPreviewError] = useState<string>()
  const [typed, setTyped] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()

  useEffect(() => {
    if (!withData) return
    let active = true
    void planClient.previewPlanDataDeletion({ planId: plan.id }).then(
      (r) => active && setPreview(r.preview),
      (e) => active && setPreviewError(errorMessage(e)),
    )
    return () => {
      active = false
    }
  }, [withData, plan.id])

  async function remove() {
    setBusy(true)
    setError(undefined)
    try {
      await planClient.deletePlan({ id: plan.id, deleteData: withData, confirmName: typed.trim() })
      if (withData) onClose()
      else onDeleted()
    } catch (err) {
      setError(errorMessage(err))
      setBusy(false)
    }
  }

  const blocked = withData && (!preview || preview.problems.length > 0 || typed.trim() !== name)
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-3xl">
        <DialogHeader>
          <DialogTitle>Delete {name}?</DialogTitle>
          <DialogDescription>
            The plan's settings are removed and its guests are no longer protected by it. Its replicas on the DR host and its snapshots on
            the primary are kept unless you delete them too.
          </DialogDescription>
        </DialogHeader>
        <div className="flex items-start gap-2">
          <input id="delete-data" type="checkbox" className="mt-1" checked={withData} onChange={(e) => {
              setPreview(undefined)
              setPreviewError(undefined)
              setWithData(e.target.checked)
            }} />
          <Label htmlFor="delete-data" className="grid gap-1 leading-snug font-normal">
            <span className="font-medium">Also delete its replicated data</span>
            <span className="text-muted-foreground">
              Destroys the replicas on the DR host and deletes the plan's snapshots on the primary. The guests' disks on the primary are
              not touched. This can't be undone.
            </span>
          </Label>
        </div>
        {withData && (
          <>
            <ErrorAlert message={previewError} />
            {!preview && !previewError && (
              <div className="flex items-center gap-2 text-sm text-muted-foreground">
                <Loader2 className="size-4 animate-spin" /> Checking both hosts…
              </div>
            )}
            {preview && <CleanupPreview preview={preview} />}
            {preview && preview.problems.length === 0 && (
              <div className="grid gap-2">
                <Label htmlFor="confirm-name">
                  Type <span className="font-mono">{name}</span> to confirm
                </Label>
                <Input id="confirm-name" value={typed} onChange={(e) => setTyped(e.target.value)} autoComplete="off" />
              </div>
            )}
          </>
        )}
        <ErrorAlert message={error} />
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="destructive" disabled={busy || blocked} onClick={() => void remove()}>
            {withData ? 'Delete plan and data' : 'Delete plan'}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

// DeletionCard follows the deletion of a plan's data, and leaves for the
// plan list once the plan is gone.
export function DeletionCard({ plan }: { plan: Plan }) {
  const navigate = useNavigate()
  const { data, reload } = usePoll(() => planClient.getDataCleanup({ planId: plan.id, kind: DataCleanupKind.PLAN }), 3000)
  const c = data?.cleanup
  useEffect(() => {
    if (data && !c) navigate('/plans')
  }, [data, c, navigate])
  if (!c) return null
  const failed = c.state === DataCleanupState.FAILED
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          Deleting the plan and its data{' '}
          <StatusBadge tone={failed ? 'destructive' : 'info'} pulse={!failed}>
            {failed ? 'Failed' : 'Deleting'}
          </StatusBadge>
        </CardTitle>
        <CardDescription>
          {failed
            ? `${c.error}. Retry once the problem is fixed, or keep the plan as a draft (data already deleted stays deleted).`
            : `Started ${formatRelative(c.startedAt)}${c.startedBy ? ` by ${c.startedBy}` : ''}. The plan is deleted once both hosts are cleaned up.`}
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-4">
        <CleanupSteps cleanup={c} />
        <CleanupActions cleanup={c} onChanged={() => void reload()} cancelLabel="Keep the plan" />
      </CardContent>
    </Card>
  )
}

// TakeoverCleanupCard offers, after a completed takeover, removing what the
// old jobs left, and shows the result.
export function TakeoverCleanupCard({ planId }: { planId: string }) {
  const [open, setOpen] = useState(false)
  const [running, setRunning] = useState(false)
  const { data, reload } = usePoll(async () => {
    const r = await planClient.getDataCleanup({ planId, kind: DataCleanupKind.TAKEOVER })
    setRunning(r.cleanup?.state === DataCleanupState.RUNNING)
    return r
  }, running ? 3000 : 30_000)
  const c = data?.cleanup
  const done = c?.state === DataCleanupState.COMPLETED
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          Old jobs' leftovers
          {c && (
            <StatusBadge
              tone={done ? 'success' : c.state === DataCleanupState.FAILED ? 'destructive' : c.state === DataCleanupState.RUNNING ? 'info' : 'neutral'}
              pulse={c.state === DataCleanupState.RUNNING}
            >
              {done ? 'Cleaned up' : c.state === DataCleanupState.FAILED ? 'Failed' : c.state === DataCleanupState.RUNNING ? 'Cleaning up' : 'Canceled'}
            </StatusBadge>
          )}
        </CardTitle>
        <CardDescription>
          The old jobs replicated some datasets the plan doesn't, such as the pool root or cloud-init volumes. Their snapshots on the primary
          and their stale replicas on the DR host stay until they're removed.
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-4">
        {c && (
          <>
            {done && (
              <p className="flex items-center gap-2 text-sm">
                <CheckCircle2 className="size-4 text-success" /> Cleaned up {formatRelative(c.completedAt ?? c.startedAt)}
                {c.startedBy && ` by ${c.startedBy}`}.
              </p>
            )}
            {c.state === DataCleanupState.FAILED && <ErrorAlert message={c.error} />}
            <CleanupSteps cleanup={c} />
            <CleanupActions cleanup={c} onChanged={() => void reload()} cancelLabel="Give up" />
          </>
        )}
        {(!c || c.state === DataCleanupState.CANCELED) && (
          <div>
            <Button variant="outline" onClick={() => setOpen(true)}>
              Clean up what the old jobs left
            </Button>
          </div>
        )}
      </CardContent>
      {open && (
        <TakeoverCleanupDialog
          planId={planId}
          onClose={() => setOpen(false)}
          onStarted={() => {
            setOpen(false)
            setRunning(true)
            void reload()
          }}
        />
      )}
    </Card>
  )
}

function TakeoverCleanupDialog({ planId, onClose, onStarted }: { planId: string; onClose: () => void; onStarted: () => void }) {
  const [preview, setPreview] = useState<DataCleanupPreview>()
  const [error, setError] = useState<string>()
  const [busy, setBusy] = useState(false)
  useEffect(() => {
    let active = true
    void planClient.previewTakeoverCleanup({ planId }).then(
      (r) => active && setPreview(r.preview),
      (e) => active && setError(errorMessage(e)),
    )
    return () => {
      active = false
    }
  }, [planId])

  async function start() {
    setBusy(true)
    setError(undefined)
    try {
      await planClient.startTakeoverCleanup({ planId })
      onStarted()
    } catch (err) {
      setError(errorMessage(err))
      setBusy(false)
    }
  }

  const nothing = preview && preview.hosts.every((h) => h.datasets.length === 0)
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-3xl">
        <DialogHeader>
          <DialogTitle>Clean up what the old jobs left</DialogTitle>
          <DialogDescription>
            Deletes the old jobs' snapshots on datasets the plan doesn't replicate, and their stale replicas on the DR host. Datasets on the
            primary are never destroyed. This can't be undone.
          </DialogDescription>
        </DialogHeader>
        {!preview && !error && (
          <div className="flex items-center gap-2 text-sm text-muted-foreground">
            <Loader2 className="size-4 animate-spin" /> Checking both hosts…
          </div>
        )}
        {preview && <CleanupPreview preview={preview} />}
        {nothing && <p className="text-sm text-muted-foreground">The old jobs left nothing behind.</p>}
        <ErrorAlert message={error} />
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="destructive" disabled={busy || !preview || nothing || preview.problems.length > 0} onClick={() => void start()}>
            Clean up
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
