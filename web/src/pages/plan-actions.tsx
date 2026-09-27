import { CheckCircle2, CircleAlert, Loader2, Pause, Play, Power, Rocket } from 'lucide-react'
import { useEffect, useState } from 'react'

import { ErrorAlert } from '@/components/error-alert'
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from '@/components/ui/alert-dialog'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Severity } from '@/gen/ezdr/plan/v1/plan_pb'
import { type Plan, PlanState, type PreviewPlanChangesResponse } from '@/gen/ezdr/portal/v1/portal_pb'
import { errorMessage, planClient } from '@/lib/api'

const stateLabel: Record<PlanState, string> = {
  [PlanState.UNSPECIFIED]: 'unknown',
  [PlanState.DRAFT]: 'Draft',
  [PlanState.ACTIVE]: 'Active',
  [PlanState.PAUSED]: 'Paused',
}

export function StateBadge({ state, pending }: { state: PlanState; pending?: boolean }) {
  return (
    <span className="inline-flex items-center gap-1">
      <Badge variant={state === PlanState.ACTIVE ? 'default' : 'secondary'}>{stateLabel[state]}</Badge>
      {pending && <Badge variant="outline">pending changes</Badge>}
    </span>
  )
}

// ChangesDialog previews what applying the plan will change on each host,
// then runs confirm.
function ChangesDialog({
  plan,
  title,
  confirmLabel,
  open,
  onOpenChange,
  confirm,
}: {
  plan: Plan
  title: string
  confirmLabel: string
  open: boolean
  onOpenChange: (o: boolean) => void
  confirm: () => Promise<void>
}) {
  const [preview, setPreview] = useState<PreviewPlanChangesResponse>()
  const [error, setError] = useState<string>()
  const [busy, setBusy] = useState(false)

  // Load the preview each time the dialog opens.
  useEffect(() => {
    if (!open) return
    let active = true
    void planClient.previewPlanChanges({ id: plan.id }).then(
      (r) => active && setPreview(r),
      (e) => active && setError(errorMessage(e)),
    )
    return () => {
      active = false
    }
  }, [open, plan.id])

  function changeOpen(o: boolean) {
    if (!o) {
      setPreview(undefined)
      setError(undefined)
    }
    onOpenChange(o)
  }
  const errors = preview?.issues.filter((i) => i.severity === Severity.ERROR) ?? []

  async function run() {
    setBusy(true)
    setError(undefined)
    try {
      await confirm()
      changeOpen(false)
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog open={open} onOpenChange={changeOpen}>
      <DialogContent className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
          <DialogDescription>These changes will be made on each host.</DialogDescription>
        </DialogHeader>
        <ErrorAlert message={error} />
        {!preview && !error && <Loader2 className="animate-spin" />}
        {errors.length > 0 && (
          <ErrorAlert message={`Fix ${errors.length} validation error(s) first: ${errors.map((e) => e.message).join('; ')}`} />
        )}
        {preview?.hosts.map((h) => (
          <div key={h.hostId} className="grid gap-1">
            <div className="font-medium">{h.hostname}</div>
            <ul className="list-disc space-y-1 pl-5 text-sm text-muted-foreground">
              {h.changes.map((c) => (
                <li key={c} className="break-words">
                  {c}
                </li>
              ))}
            </ul>
          </div>
        ))}
        <DialogFooter>
          <Button onClick={() => void run()} disabled={busy || !preview || errors.length > 0}>
            {confirmLabel}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function ConfirmButton({
  label,
  icon,
  title,
  description,
  onConfirm,
  destructive,
}: {
  label: string
  icon: React.ReactNode
  title: string
  description: string
  onConfirm: () => void
  destructive?: boolean
}) {
  return (
    <AlertDialog>
      <AlertDialogTrigger render={<Button variant="outline" />}>
        {icon} {label}
      </AlertDialogTrigger>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{title}</AlertDialogTitle>
          <AlertDialogDescription>{description}</AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction variant={destructive ? 'destructive' : 'default'} onClick={onConfirm}>
            {label}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}

// PlanActions shows the lifecycle actions for the plan's state.
export function PlanActions({ plan, onChanged, dirty }: { plan: Plan; onChanged: (p: Plan) => void; dirty: boolean }) {
  const [dialog, setDialog] = useState<'activate' | 'apply'>()
  const [error, setError] = useState<string>()

  async function act(fn: () => Promise<{ plan?: Plan }>) {
    setError(undefined)
    try {
      const r = await fn()
      if (r.plan) onChanged(r.plan)
    } catch (err) {
      setError(errorMessage(err))
    }
  }

  return (
    <div className="grid justify-items-end gap-2">
      <div className="flex flex-wrap items-center justify-end gap-2">
        {plan.state === PlanState.DRAFT && (
          <Button onClick={() => setDialog('activate')} disabled={dirty} title={dirty ? 'Save first' : undefined}>
            <Rocket /> Activate
          </Button>
        )}
        {plan.state !== PlanState.DRAFT && plan.pendingChanges && (
          <>
            <Button onClick={() => setDialog('apply')} disabled={dirty} title={dirty ? 'Save first' : undefined}>
              Apply changes
            </Button>
            <Button variant="ghost" onClick={() => void act(() => planClient.discardPlanChanges({ id: plan.id }))} disabled={dirty}>
              Discard changes
            </Button>
          </>
        )}
        {plan.state === PlanState.ACTIVE && (
          <ConfirmButton
            label="Pause"
            icon={<Pause />}
            title={`Pause ${plan.spec?.name}?`}
            description="Snapshots and replication stop on both hosts. Replicas, snapshots, and zrepl's bookmarks are kept, so resuming continues incrementally."
            onConfirm={() => void act(() => planClient.pausePlan({ id: plan.id }))}
          />
        )}
        {plan.state === PlanState.PAUSED && (
          <Button variant="outline" onClick={() => void act(() => planClient.resumePlan({ id: plan.id }))}>
            <Play /> Resume
          </Button>
        )}
        {plan.state !== PlanState.DRAFT && (
          <ConfirmButton
            label="Deactivate"
            icon={<Power />}
            destructive
            title={`Deactivate ${plan.spec?.name}?`}
            description="EZDR's zrepl jobs for this plan are removed from both hosts and the plan returns to draft. Replicas and snapshots are never deleted."
            onConfirm={() => void act(() => planClient.deactivatePlan({ id: plan.id }))}
          />
        )}
      </div>
      <ErrorAlert message={error} />
      <ChangesDialog
        plan={plan}
        title={`Activate ${plan.spec?.name}`}
        confirmLabel="Activate plan"
        open={dialog === 'activate'}
        onOpenChange={(o) => setDialog(o ? 'activate' : undefined)}
        confirm={async () => {
          const r = await planClient.activatePlan({ id: plan.id })
          if (r.plan) onChanged(r.plan)
        }}
      />
      <ChangesDialog
        plan={plan}
        title={`Apply changes to ${plan.spec?.name}`}
        confirmLabel="Apply changes"
        open={dialog === 'apply'}
        onOpenChange={(o) => setDialog(o ? 'apply' : undefined)}
        confirm={async () => {
          const r = await planClient.applyPlanChanges({ id: plan.id })
          if (r.plan) onChanged(r.plan)
        }}
      />
    </div>
  )
}

// HostStatusPanel shows each host's progress applying the plan.
export function HostStatusPanel({ plan }: { plan: Plan }) {
  if (plan.state === PlanState.DRAFT) return null
  return (
    <Card>
      <CardHeader>
        <CardTitle>Hosts</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-3 text-sm">
        {plan.hosts.map((h) => (
          <div key={h.hostId} className="grid gap-0.5">
            <div className="flex items-center justify-between gap-2">
              <span className="font-medium">{h.hostname}</span>
              {!h.online ? (
                <Badge variant="secondary">offline</Badge>
              ) : h.applyError ? (
                <Badge variant="destructive">error</Badge>
              ) : h.applied ? (
                <span className="flex items-center gap-1 text-xs text-emerald-700">
                  <CheckCircle2 className="size-4" /> applied
                </span>
              ) : (
                <span className="flex items-center gap-1 text-xs text-muted-foreground">
                  <Loader2 className="size-4 animate-spin" /> applying
                </span>
              )}
            </div>
            <div className="text-xs text-muted-foreground">
              zrepl {h.zreplVersion || 'not installed'}
              {!h.hasCertificate && ' · waiting for certificate'}
            </div>
            {h.applyError && (
              <div className="flex gap-1 text-xs break-words text-destructive">
                <CircleAlert className="mt-0.5 size-3 shrink-0" /> {h.applyError}
              </div>
            )}
          </div>
        ))}
      </CardContent>
    </Card>
  )
}
