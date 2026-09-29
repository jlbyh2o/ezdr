import { CheckCircle2, CircleAlert, Ellipsis, FlaskConical, Loader2, Pause, Play, Power, Rocket, ShieldAlert, Trash2, Undo2 } from 'lucide-react'
import { useEffect, useState } from 'react'
import { Link } from 'react-router'

import { ErrorAlert } from '@/components/error-alert'
import { StatusBadge } from '@/components/status-badge'
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from '@/components/ui/alert-dialog'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from '@/components/ui/dropdown-menu'
import { Separator } from '@/components/ui/separator'
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
import { operationPath } from '@/lib/operations'
import { planStates } from '@/lib/status'
import { FailbackDialog } from '@/pages/failback'
import { FailoverDialog } from '@/pages/failover'
import { TakeoverDialog } from '@/pages/takeover'

export function StateBadge({ state, pending }: { state: PlanState; pending?: boolean }) {
  const st = planStates[state]
  return (
    <span className="inline-flex items-center gap-1">
      <StatusBadge tone={st.tone} pulse={state === PlanState.FAILING_OVER || state === PlanState.FAILING_BACK}>
        {st.label}
      </StatusBadge>
      {pending && <Badge variant="warning">Pending changes</Badge>}
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

type Confirm = {
  title: string
  description: string
  label: string
  destructive?: boolean
  run: () => Promise<unknown>
}

// PlanActions shows the plan's actions for its state: the main ones as
// buttons, routine ones in a menu, and Fail over set apart.
export function PlanActions({
  plan,
  onChanged,
  dirty,
  onStartTest,
  onDeleted,
}: {
  plan: Plan
  onChanged: (p: Plan) => void
  dirty: boolean
  onStartTest: () => void
  onDeleted: () => void
}) {
  const [dialog, setDialog] = useState<'activate' | 'apply' | 'takeover' | 'failover' | 'failback'>()
  const [confirm, setConfirm] = useState<Confirm>()
  const failingBack = plan.state === PlanState.FAILING_BACK
  const failedOver = plan.state === PlanState.FAILING_OVER || plan.state === PlanState.FAILED_OVER || failingBack
  const adopted = !!plan.spec?.takeover
  const name = plan.spec?.name
  const [error, setError] = useState<string>()
  const saveFirst = dirty ? 'Save your changes first' : undefined

  async function act(fn: () => Promise<{ plan?: Plan }>) {
    setError(undefined)
    try {
      const r = await fn()
      if (r.plan) onChanged(r.plan)
    } catch (err) {
      setError(errorMessage(err))
    }
  }

  const pending = plan.state !== PlanState.DRAFT && !failedOver && plan.pendingChanges
  const menu: { label: string; icon: React.ReactNode; onClick: () => void; destructive?: boolean }[] = []
  if (pending) {
    menu.push({ label: 'Discard changes', icon: <Undo2 />, onClick: () => void act(() => planClient.discardPlanChanges({ id: plan.id })) })
  }
  if (plan.state === PlanState.ACTIVE) {
    menu.push({
      label: 'Pause',
      icon: <Pause />,
      onClick: () =>
        setConfirm({
          title: `Pause ${name}?`,
          description:
            "Snapshots and replication stop on both hosts. Replicas, snapshots, and zrepl's bookmarks are kept, so resuming continues incrementally.",
          label: 'Pause',
          run: () => act(() => planClient.pausePlan({ id: plan.id })),
        }),
    })
  }
  if (plan.state === PlanState.PAUSED) {
    menu.push({ label: 'Resume', icon: <Play />, onClick: () => void act(() => planClient.resumePlan({ id: plan.id })) })
  }
  if (plan.state !== PlanState.DRAFT && !failedOver) {
    menu.push({
      label: 'Deactivate',
      icon: <Power />,
      destructive: true,
      onClick: () =>
        setConfirm({
          title: `Deactivate ${name}?`,
          description:
            "EZDR's zrepl jobs for this plan are removed from both hosts and the plan returns to draft. Replicas and snapshots are never deleted.",
          label: 'Deactivate',
          destructive: true,
          run: () => act(() => planClient.deactivatePlan({ id: plan.id })),
        }),
    })
  }
  if (plan.state === PlanState.DRAFT) {
    menu.push({
      label: 'Delete plan',
      icon: <Trash2 />,
      destructive: true,
      onClick: () =>
        setConfirm({
          title: `Delete ${name}?`,
          description: "The plan's settings are removed. Its guests are no longer protected by it.",
          label: 'Delete plan',
          destructive: true,
          run: async () => {
            try {
              await planClient.deletePlan({ id: plan.id })
              onDeleted()
            } catch (err) {
              setError(errorMessage(err))
            }
          },
        }),
    })
  }

  return (
    <div className="grid justify-items-end gap-2">
      <div className="flex flex-wrap items-center justify-end gap-2">
        {plan.state === PlanState.DRAFT && (
          <Button onClick={() => setDialog(adopted ? 'takeover' : 'activate')} disabled={dirty} title={saveFirst}>
            <Rocket /> {adopted ? 'Take over' : 'Activate'}
          </Button>
        )}
        {pending && (
          <Button onClick={() => setDialog('apply')} disabled={dirty} title={saveFirst}>
            <CheckCircle2 /> Apply changes
          </Button>
        )}
        {(plan.state === PlanState.ACTIVE || plan.state === PlanState.PAUSED) && (
          <Button variant={pending ? 'outline' : 'default'} onClick={onStartTest}>
            <FlaskConical /> Test failover
          </Button>
        )}
        {plan.state === PlanState.FAILING_OVER && (
          <Button render={<Link to={operationPath.failover(plan.id)} />}>
            <Loader2 className="animate-spin" /> Failover status
          </Button>
        )}
        {plan.state === PlanState.FAILED_OVER && (
          <>
            <Button variant="outline" render={<Link to={operationPath.failover(plan.id)} />}>
              <ShieldAlert /> Failover details
            </Button>
            <Button onClick={() => setDialog('failback')}>
              <Undo2 /> Fail back
            </Button>
          </>
        )}
        {failingBack && (
          <Button render={<Link to={operationPath.failback(plan.id)} />}>
            <Loader2 className="animate-spin" /> Failback status
          </Button>
        )}
        {menu.length > 0 && (
          <DropdownMenu>
            <DropdownMenuTrigger render={<Button variant="outline" size="icon" aria-label="More actions" />}>
              <Ellipsis />
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end" className="min-w-44">
              {menu.map((m) => (
                <DropdownMenuItem key={m.label} variant={m.destructive ? 'destructive' : 'default'} onClick={m.onClick}>
                  {m.icon} {m.label}
                </DropdownMenuItem>
              ))}
            </DropdownMenuContent>
          </DropdownMenu>
        )}
        {(plan.state === PlanState.ACTIVE || plan.state === PlanState.PAUSED) && (
          <>
            <Separator orientation="vertical" className="mx-1 data-[orientation=vertical]:h-6" />
            <Button
              variant="outline"
              className="border-destructive/50 text-destructive hover:bg-destructive/10 hover:text-destructive"
              onClick={() => setDialog('failover')}
            >
              <ShieldAlert /> Fail over
            </Button>
          </>
        )}
      </div>
      <ErrorAlert message={error} />
      <AlertDialog open={!!confirm} onOpenChange={(o) => !o && setConfirm(undefined)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{confirm?.title}</AlertDialogTitle>
            <AlertDialogDescription>{confirm?.description}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              variant={confirm?.destructive ? 'destructive' : 'default'}
              onClick={() => {
                const c = confirm
                setConfirm(undefined)
                void c?.run()
              }}
            >
              {confirm?.label}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
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
      {dialog === 'failover' && (
        <FailoverDialog
          plan={plan}
          onClose={() => {
            setDialog(undefined)
            void act(() => planClient.getPlan({ id: plan.id }))
          }}
        />
      )}
      {dialog === 'failback' && (
        <FailbackDialog
          plan={plan}
          onClose={() => {
            setDialog(undefined)
            void act(() => planClient.getPlan({ id: plan.id }))
          }}
        />
      )}
      {dialog === 'takeover' && (
        <TakeoverDialog
          plan={plan}
          onClose={() => {
            setDialog(undefined)
            // A completed takeover activates the plan and clears its adoption.
            void act(() => planClient.getPlan({ id: plan.id }))
          }}
        />
      )}
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
                <Badge variant="destructive">offline</Badge>
              ) : h.applyError ? (
                <Badge variant="destructive">error</Badge>
              ) : h.applied ? (
                <span className="flex items-center gap-1 text-xs text-success">
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
