import { ArrowLeft } from 'lucide-react'
import { useState } from 'react'
import { Link, useParams } from 'react-router'

import { ErrorAlert } from '@/components/error-alert'
import { StatusBadge } from '@/components/status-badge'
import { Card, CardContent } from '@/components/ui/card'
import { type Failback, type Failover, type Takeover, TakeoverState, type TestRun } from '@/gen/ezdr/portal/v1/portal_pb'
import { failoverClient, planClient, testClient } from '@/lib/api'
import { formatDateTime, formatRelative } from '@/lib/format'
import { failbackStates, failoverStates, takeoverStates, testStates } from '@/lib/operations'
import type { Tone } from '@/lib/status'
import { usePoll } from '@/lib/use-poll'
import { FailbackActions, FailbackProgress } from '@/pages/failback'
import { FailoverActions, FailoverProgress } from '@/pages/failover'
import { TakeoverCleanupCard } from '@/pages/data-cleanup'
import { TakeoverActions, TakeoverProgress } from '@/pages/takeover'
import { TestDetails } from '@/pages/test-failover'

// An operation page follows one takeover, test failover, failover, or
// failback while it runs, and keeps it as a record afterwards.

// OperationShell lays out an operation page: a link back to the plan, the
// title and state, who started it and when, and the actions that apply.
function OperationShell({
  planId,
  title,
  look,
  started,
  actions,
  error,
  children,
}: {
  planId: string
  title: string
  look?: { label: string; tone: Tone; active?: boolean }
  started?: React.ReactNode
  actions?: React.ReactNode
  error?: string
  children?: React.ReactNode
}) {
  const { data: plan } = usePoll(() => planClient.getPlan({ id: planId }))
  const name = plan?.plan?.spec?.name
  return (
    <>
      <div className="grid gap-3">
        <Link to={`/plans/${planId}`} className="flex w-fit items-center gap-1 text-sm text-muted-foreground hover:text-foreground">
          <ArrowLeft className="size-4" /> {name ?? 'Plan'}
        </Link>
        <div className="flex flex-wrap items-start justify-between gap-4">
          <div className="grid gap-1">
            <div className="flex flex-wrap items-center gap-2">
              <h1 className="text-2xl font-semibold tracking-tight">
                {title}
                {name && <span className="text-muted-foreground"> · {name}</span>}
              </h1>
              {look && (
                <StatusBadge tone={look.tone} pulse={look.tone === 'info'}>
                  {look.label}
                </StatusBadge>
              )}
            </div>
            {started && <p className="text-sm text-muted-foreground">{started}</p>}
          </div>
          {actions}
        </div>
      </div>
      <ErrorAlert message={error} />
      {children && (
        <Card>
          <CardContent>{children}</CardContent>
        </Card>
      )}
    </>
  )
}

function startedLine(at?: Parameters<typeof formatDateTime>[0], by?: string) {
  if (!at) return undefined
  return (
    <>
      Started <span title={formatDateTime(at)}>{formatRelative(at)}</span>
      {by && ` by ${by}`}
    </>
  )
}

// Operations are polled quickly while they run.
const poll = (active?: boolean) => (active ? 2000 : 15000)

export function FailoverPage() {
  const { id = '', opId = '' } = useParams()
  const [override, setOverride] = useState<Failover>()
  const [active, setActive] = useState(true)
  const { data, error } = usePoll(async () => {
    const r = await failoverClient.getFailover({ planId: id, id: opId === 'latest' ? '' : opId })
    setOverride(undefined)
    setActive(!!r.failover && !!failoverStates[r.failover.state].active)
    return r
  }, poll(active))
  const f = override ?? data?.failover
  const kind = !f ? 'Failover' : f.breakGlass ? 'Break-glass failover' : f.planned ? 'Planned failover' : 'Unplanned failover'
  return (
    <OperationShell
      planId={id}
      title={kind}
      look={f && failoverStates[f.state]}
      started={f && startedLine(f.startedAt, f.startedBy)}
      actions={f && <FailoverActions failover={f} onChanged={setOverride} />}
      error={error ?? (data && !f ? "This plan hasn't failed over." : undefined)}
    >
      {f && <FailoverProgress failover={f} />}
    </OperationShell>
  )
}

export function FailbackPage() {
  const { id = '', opId = '' } = useParams()
  const [override, setOverride] = useState<Failback>()
  const [active, setActive] = useState(true)
  const { data, error } = usePoll(async () => {
    const r = await failoverClient.getFailback({ planId: id, id: opId === 'latest' ? '' : opId })
    setOverride(undefined)
    setActive(!!r.failback && !!failbackStates[r.failback.state].active)
    return r
  }, poll(active))
  const f = override ?? data?.failback
  return (
    <OperationShell
      planId={id}
      title="Failback"
      look={f && failbackStates[f.state]}
      started={f && startedLine(f.startedAt, f.startedBy)}
      actions={f && <FailbackActions failback={f} onChanged={setOverride} />}
      error={error ?? (data && !f ? "This plan hasn't failed back." : undefined)}
    >
      {f && <FailbackProgress failback={f} />}
    </OperationShell>
  )
}

export function TestPage() {
  const { id = '', opId = '' } = useParams()
  const [active, setActive] = useState(true)
  const { data, error, reload } = usePoll(async () => {
    const r = await testClient.getTest({ id: opId })
    setActive(!!r.test && !!testStates[r.test.state].active)
    return r.test as TestRun | undefined
  }, poll(active))
  return (
    <OperationShell
      planId={id}
      title="Test failover"
      look={data && testStates[data.state]}
      started={data && startedLine(data.startedAt, data.startedBy)}
      error={error}
    >
      {data && <TestDetails test={data} onChanged={() => void reload()} />}
    </OperationShell>
  )
}

export function TakeoverPage() {
  const { id = '' } = useParams()
  const [override, setOverride] = useState<Takeover>()
  const [active, setActive] = useState(true)
  const { data, error } = usePoll(async () => {
    const r = await planClient.getTakeover({ id })
    setOverride(undefined)
    setActive(!!r.takeover && !!takeoverStates[r.takeover.state].active)
    return r
  }, poll(active))
  const t = override ?? data?.takeover
  const first = t?.steps.find((s) => s.status !== 'pending' && s.updatedAt)?.updatedAt
  return (
    <OperationShell
      planId={id}
      title="zrepl takeover"
      look={t && takeoverStates[t.state]}
      started={startedLine(first)}
      actions={t && <TakeoverActions planId={id} takeover={t} onChanged={setOverride} />}
      error={error ?? (data && !t ? 'This plan has no takeover.' : undefined)}
    >
      {t && <TakeoverProgress takeover={t} />}
      {t?.state === TakeoverState.COMPLETED && <TakeoverCleanupCard planId={id} />}
    </OperationShell>
  )
}
