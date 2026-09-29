import {
  ArrowLeftRight,
  Bell,
  CheckCircle2,
  Circle,
  FlaskConical,
  Loader2,
  MoveRight,
  RotateCcw,
  Server,
  ShieldCheck,
  Timer,
  X,
} from 'lucide-react'
import { lazy, Suspense, useState } from 'react'
import { Link } from 'react-router'

import { ErrorAlert } from '@/components/error-alert'
import { RPO } from '@/components/rpo'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import {
  type GetOverviewResponse,
  HealthState,
  OperationKind,
  type OverviewOperation,
  PlanState,
} from '@/gen/ezdr/portal/v1/portal_pb'
import { overviewClient } from '@/lib/api'
import { formatDuration, formatRelative } from '@/lib/format'
import { operationPath } from '@/lib/operations'
import { healthStates, type Tone } from '@/lib/status'
import { usePoll } from '@/lib/use-poll'

// React Flow is large; load it with the chart.
const ReplicationChart = lazy(() =>
  import('@/components/flow/replication-chart').then((m) => ({ default: m.ReplicationChart })),
)

export function OverviewPage() {
  const [busy, setBusy] = useState(false)
  // Poll faster while data moves or an operation runs.
  const { data, error } = usePoll(async () => {
    const ov = await overviewClient.getOverview({})
    setBusy(ov.operations.length > 0 || ov.plans.some((p) => p.transfer))
    return ov
  }, busy ? 2000 : 5000)

  return (
    <>
      <ErrorAlert message={error} />
      {data && (
        <>
          <GettingStarted ov={data} />
          <Tiles ov={data} />
          <Operations ops={data.operations} />
          <Card>
            <CardHeader className="flex flex-wrap items-center justify-between gap-2">
              <div>
                <CardTitle>Replication</CardTitle>
                <CardDescription>Primary hosts on the left, DR hosts on the right.</CardDescription>
              </div>
              <Legend />
            </CardHeader>
            <CardContent>
              {data.hosts.length === 0 ? (
                <div className="grid justify-items-center gap-2 py-16 text-center text-sm text-muted-foreground">
                  <Server className="size-8" />
                  No hosts yet. Enroll a primary host and a DR host to get started.
                  <Button size="sm" render={<Link to="/hosts" />}>
                    Add a host
                  </Button>
                </div>
              ) : (
                <Suspense fallback={<div className="h-96" />}>
                  <ReplicationChart overview={data} />
                </Suspense>
              )}
            </CardContent>
          </Card>
        </>
      )}
    </>
  )
}

function Legend() {
  const item = 'flex items-center gap-1.5'
  return (
    <div className="flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted-foreground">
      <span className={item}>
        <span className="h-3 w-5 rounded border-[1.5px] border-success/70 bg-success/5" /> Running
      </span>
      <span className={item}>
        <span className="h-3 w-5 rounded border-[1.5px] border-muted-foreground/35 bg-muted/60" /> Standby
      </span>
      <span className={item}>
        <span className="h-3 w-5 rounded border-[1.5px] border-dashed border-warning/70" /> Unconfigured
      </span>
      <span className={item}>
        <span className="h-3 w-5 rounded border-[1.5px] border-border opacity-60" /> Unprotected
      </span>
      <span className={item}>
        <span className="h-3 w-5 rounded border-[1.5px] border-dashed border-info/60 bg-info/5" /> Test copy
      </span>
      <span className={item}>
        <span className="flex items-center">
          <span className="h-0.5 w-5 bg-success" />
          <span className="-ml-3 size-2 animate-pulse rounded-full bg-success" />
        </span>
        Copying data
      </span>
      <span className={item}>
        <MoveRight className="size-4" /> Replication direction
      </span>
    </div>
  )
}

const toneText: Record<Tone, string> = {
  success: 'text-success',
  warning: 'text-warning-foreground',
  destructive: 'text-destructive',
  info: 'text-info',
  neutral: 'text-muted-foreground',
}

function Tile({
  to,
  icon: Icon,
  label,
  value,
  detail,
  tone = 'neutral',
}: {
  to: string
  icon: React.ComponentType<{ className?: string }>
  label: React.ReactNode
  value: React.ReactNode
  detail?: React.ReactNode
  tone?: Tone
}) {
  return (
    <Link to={to} className="rounded-lg border bg-card p-4 shadow-xs transition-colors hover:bg-muted/50">
      <div className="flex items-center gap-2 text-sm text-muted-foreground">
        <Icon className="size-4" /> {label}
      </div>
      <div className={`mt-1 text-2xl font-semibold tabular-nums ${toneText[tone]}`}>{value}</div>
      {detail && <div className="mt-0.5 truncate text-xs text-muted-foreground">{detail}</div>}
    </Link>
  )
}

function Tiles({ ov }: { ov: GetOverviewResponse }) {
  const active = ov.plans.filter((p) => p.state === PlanState.ACTIVE)
  const healthy = active.filter((p) => p.health?.state === HealthState.HEALTHY)
  const troubled = active.filter((p) => p.health?.state === HealthState.LAGGING || p.health?.state === HealthState.FAILING)
  const failedOver = ov.plans.filter((p) => p.state === PlanState.FAILED_OVER)
  const worst = [...active].sort((a, b) => Number((b.health?.rpoAgeSeconds ?? 0n) - (a.health?.rpoAgeSeconds ?? 0n)))[0]
  const online = ov.hosts.filter((h) => h.online)
  const offline = ov.hosts.filter((h) => !h.online)

  let plansDetail = `${ov.plans.length - active.length} not active`
  if (troubled.length > 0) plansDetail = `${troubled.map((p) => p.name).join(', ')} need attention`
  else if (failedOver.length > 0) plansDetail = `${failedOver.map((p) => p.name).join(', ')} failed over`

  return (
    <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
      <Tile
        to="/plans"
        icon={ShieldCheck}
        label="Plans healthy"
        value={active.length > 0 ? `${healthy.length} of ${active.length}` : '—'}
        detail={ov.plans.length > 0 ? plansDetail : 'No plans yet'}
        tone={active.length === 0 ? 'neutral' : troubled.length > 0 ? 'warning' : healthy.length === active.length ? 'success' : 'neutral'}
      />
      <Tile
        to={worst ? `/plans/${worst.id}` : '/plans'}
        icon={Timer}
        label={
          <>
            Worst <RPO />
          </>
        }
        value={worst?.health?.rpoAgeSeconds ? formatDuration(worst.health.rpoAgeSeconds) : '—'}
        detail={worst ? `${worst.name} · alert after ${formatDuration(worst.health?.rpoAlertSeconds ?? 0n)}` : 'No active plans'}
        tone={worst ? healthStates[worst.health?.state ?? HealthState.UNSPECIFIED].tone : 'neutral'}
      />
      <Tile
        to="/hosts"
        icon={Server}
        label="Hosts online"
        value={ov.hosts.length > 0 ? `${online.length} of ${ov.hosts.length}` : '—'}
        detail={
          offline.length > 0
            ? `Offline: ${offline.map((h) => h.hostname).join(', ')}`
            : ov.hosts.length > 0
              ? 'All hosts connected'
              : 'No hosts yet'
        }
        tone={offline.length > 0 ? 'destructive' : ov.hosts.length > 0 ? 'success' : 'neutral'}
      />
      <Tile
        to="/alerts"
        icon={Bell}
        label="Open alerts"
        value={ov.firingAlerts}
        detail={ov.firingAlerts > 0 ? 'See the alerts page' : 'Nothing firing'}
        tone={ov.firingAlerts > 0 ? 'destructive' : 'success'}
      />
    </div>
  )
}

const opKinds: Record<OperationKind, { label: string; icon: React.ComponentType<{ className?: string }> }> = {
  [OperationKind.UNSPECIFIED]: { label: 'Operation', icon: Loader2 },
  [OperationKind.TAKEOVER]: { label: 'zrepl takeover', icon: ArrowLeftRight },
  [OperationKind.TEST]: { label: 'Test failover', icon: FlaskConical },
  [OperationKind.FAILOVER]: { label: 'Failover', icon: ArrowLeftRight },
  [OperationKind.FAILBACK]: { label: 'Failback', icon: RotateCcw },
}

function opLink(o: OverviewOperation): string {
  switch (o.kind) {
    case OperationKind.TEST:
      return operationPath.test(o.planId, o.id)
    case OperationKind.FAILOVER:
      return operationPath.failover(o.planId, o.id)
    case OperationKind.FAILBACK:
      return operationPath.failback(o.planId, o.id)
    case OperationKind.TAKEOVER:
      return operationPath.takeover(o.planId)
    default:
      return `/plans/${o.planId}`
  }
}

function Operations({ ops }: { ops: OverviewOperation[] }) {
  if (ops.length === 0) return null
  return (
    <Card>
      <CardHeader>
        <CardTitle>Running operations</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-2">
        {ops.map((o) => {
          const k = opKinds[o.kind]
          return (
            <Link
              key={`${o.kind}:${o.id || o.planId}`}
              to={opLink(o)}
              className="flex flex-wrap items-center gap-x-3 gap-y-1 rounded-md border px-3 py-2 text-sm hover:bg-muted/50"
            >
              <k.icon className="size-4 text-info" />
              <span className="font-medium">
                {k.label} · {o.planName}
              </span>
              <span className={o.awaitingConfirmation ? 'text-warning-foreground' : 'text-info'}>{o.state}</span>
              {o.step && <span className="text-muted-foreground">{o.step}</span>}
              <span className="ml-auto text-xs text-muted-foreground">
                started {formatRelative(o.startedAt)}
                {o.startedBy && ` by ${o.startedBy}`}
              </span>
            </Link>
          )
        })}
      </CardContent>
    </Card>
  )
}

const dismissKey = 'ezdr-getting-started-dismissed'

function GettingStarted({ ov }: { ov: GetOverviewResponse }) {
  const [dismissed, setDismissed] = useState(() => localStorage.getItem(dismissKey) === '1')
  const steps = [
    { done: ov.hosts.length >= 1, label: 'Enroll a primary host', to: '/hosts' },
    { done: ov.hosts.length >= 2, label: 'Enroll a DR host', to: '/hosts' },
    { done: ov.plans.length >= 1, label: 'Create a DR plan', to: '/plans/new' },
    { done: ov.plans.some((p) => p.state !== PlanState.DRAFT), label: 'Activate it to start replication', to: '/plans' },
    { done: ov.plans.some((p) => p.lastTestAt), label: 'Run a test failover', to: '/plans' },
  ]
  if (dismissed || steps.every((s) => s.done)) return null
  const next = steps.findIndex((s) => !s.done)
  return (
    <Card>
      <CardHeader className="flex items-start justify-between gap-2">
        <div>
          <CardTitle>Getting started</CardTitle>
          <CardDescription>
            {steps.filter((s) => s.done).length} of {steps.length} done
          </CardDescription>
        </div>
        <Button
          variant="ghost"
          size="icon-sm"
          aria-label="Dismiss"
          onClick={() => {
            localStorage.setItem(dismissKey, '1')
            setDismissed(true)
          }}
        >
          <X />
        </Button>
      </CardHeader>
      <CardContent>
        <ol className="grid gap-2 sm:grid-cols-5">
          {steps.map((s, i) => (
            <li key={s.label}>
              <Link
                to={s.to}
                className={`flex h-full items-start gap-2 rounded-md border p-2.5 text-sm hover:bg-muted/50 ${i === next ? 'border-primary' : ''}`}
              >
                {s.done ? (
                  <CheckCircle2 className="mt-0.5 size-4 shrink-0 text-success" />
                ) : (
                  <Circle className={`mt-0.5 size-4 shrink-0 ${i === next ? 'text-primary' : 'text-muted-foreground'}`} />
                )}
                <span className={s.done ? 'text-muted-foreground line-through' : ''}>{s.label}</span>
              </Link>
            </li>
          ))}
        </ol>
      </CardContent>
    </Card>
  )
}
