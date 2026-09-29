import { ArrowRight, CheckCircle2, Circle, CircleAlert, FlaskConical, Loader2, MinusCircle, TriangleAlert, XCircle } from 'lucide-react'
import { useEffect, useState } from 'react'
import { Link, useNavigate } from 'react-router'

import { ErrorAlert } from '@/components/error-alert'
import { Alert, AlertDescription } from '@/components/ui/alert'
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
import { StatusBadge } from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Label } from '@/components/ui/label'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { NativeSelect } from '@/components/native-select'
import {
  type GetTestOptionsResponse,
  type Plan,
  PlanState,
  type TestRun,
  TestState,
  TestVerdict,
} from '@/gen/ezdr/portal/v1/portal_pb'
import { errorMessage, testClient } from '@/lib/api'
import { formatBytes, formatDateTime, formatRelative, toDate } from '@/lib/format'
import { operationPath, testStates, verdictLabel } from '@/lib/operations'
import { usePoll } from '@/lib/use-poll'

const activeStates = [TestState.STARTING, TestState.RUNNING, TestState.ENDING]

// TestFailoverCard shows an active or paused plan's current test failover,
// with a link to its page, and starts new tests. Past tests are in the
// plan's history. startSignal opens the start dialog each time it changes
// (from the plan page's header).
export function TestFailoverCard({ plan, startSignal = 0 }: { plan: Plan; startSignal?: number }) {
  const { data, error, reload } = usePoll(() => testClient.listTests({ planId: plan.id }), 5000)
  const [starting, setStarting] = useState(false)
  const [seenSignal, setSeenSignal] = useState(startSignal)
  if (startSignal !== seenSignal) {
    setSeenSignal(startSignal)
    setStarting(true)
  }
  if (plan.state !== PlanState.ACTIVE && plan.state !== PlanState.PAUSED) return null
  const tests = data?.tests ?? []
  const current = tests.find((t) => activeStates.includes(t.state))
  const last = tests.find((t) => t !== current)
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center justify-between gap-2">
          Test failover
          {!current && (
            <Button size="sm" onClick={() => setStarting(true)}>
              <FlaskConical /> Start test
            </Button>
          )}
        </CardTitle>
        <CardDescription>
          Starts copies of the protected guests on the DR host from replicated snapshots, on the isolated test bridge. Replication continues,
          and nothing on the primary changes.
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-3 text-sm">
        <ErrorAlert message={error} />
        {current && (
          <Link
            to={operationPath.test(plan.id, current.id)}
            className="flex flex-wrap items-center gap-x-3 gap-y-1 rounded-md border px-3 py-2 hover:bg-muted/50"
          >
            <StatusBadge tone={testStates[current.state].tone} pulse>
              {testStates[current.state].label}
            </StatusBadge>
            <span>from {formatDateTime(current.snapshotAt)}</span>
            <span className="text-muted-foreground">
              {current.guests.filter((g) => g.status === 'running').length} of {current.guests.length} guests running · ends by itself{' '}
              {formatRelative(current.deadline)}
            </span>
            <span className="ml-auto flex items-center gap-1 text-primary">
              Open test <ArrowRight className="size-4" />
            </span>
          </Link>
        )}
        {!current && last && (
          <p className="text-muted-foreground">
            Last test{' '}
            <Link to={operationPath.test(plan.id, last.id)} className="text-foreground underline-offset-2 hover:underline">
              {formatRelative(last.startedAt)}
            </Link>
            : {testStates[last.state].label.toLowerCase()}, {verdictLabel[last.verdict]}. All tests are in the history below.
          </p>
        )}
        {!current && !last && data && <p className="text-muted-foreground">No tests yet.</p>}
      </CardContent>
      {starting && (
        <StartTestDialog
          plan={plan}
          onClose={() => {
            setStarting(false)
            void reload()
          }}
        />
      )}
    </Card>
  )
}

function StartTestDialog({ plan, onClose }: { plan: Plan; onClose: () => void }) {
  const navigate = useNavigate()
  const [options, setOptions] = useState<GetTestOptionsResponse>()
  const [error, setError] = useState<string>()
  const [selected, setSelected] = useState<Set<number>>(new Set())
  const [snapshot, setSnapshot] = useState('')
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    let active = true
    testClient.getTestOptions({ planId: plan.id }).then(
      (r) => {
        if (!active) return
        setOptions(r)
        setSelected(new Set(r.guests.filter((g) => !g.problem).map((g) => g.vmid)))
        setSnapshot(r.pointsInTime[0]?.snapshot ?? '')
      },
      (e) => active && setError(errorMessage(e)),
    )
    return () => {
      active = false
    }
  }, [plan.id])

  async function start() {
    setBusy(true)
    setError(undefined)
    try {
      const r = await testClient.startTest({ planId: plan.id, vmids: [...selected], snapshot })
      navigate(operationPath.test(plan.id, r.test?.id ?? ''))
    } catch (err) {
      setError(errorMessage(err))
      setBusy(false)
    }
  }

  const memory = options?.guests.filter((g) => selected.has(g.vmid)).reduce((n, g) => n + g.memoryBytes, 0n) ?? 0n
  const short = options && memory > options.memoryAvailableBytes
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>Start a test failover</DialogTitle>
          <DialogDescription>
            Test guests use their ID plus {plan.spec?.testVmidOffset || 10000}, are tagged ezdr-test, and run on the test bridge{' '}
            <span className="font-mono">{plan.spec?.testBridge}</span>.
          </DialogDescription>
        </DialogHeader>
        <ErrorAlert message={error} />
        {!options && !error && (
          <div className="flex items-center gap-2 text-sm text-muted-foreground">
            <Loader2 className="size-4 animate-spin" /> Asking the DR host…
          </div>
        )}
        {options && (
          <div className="grid gap-4 text-sm">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead />
                  <TableHead>Guest</TableHead>
                  <TableHead>Test ID</TableHead>
                  <TableHead>Memory</TableHead>
                  <TableHead>Configuration</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {options.guests.map((g) => (
                  <TableRow key={g.vmid}>
                    <TableCell>
                      <input
                        type="checkbox"
                        aria-label={`Test ${g.vmid}`}
                        disabled={!!g.problem}
                        checked={selected.has(g.vmid)}
                        onChange={(e) => {
                          const next = new Set(selected)
                          if (e.target.checked) next.add(g.vmid)
                          else next.delete(g.vmid)
                          setSelected(next)
                        }}
                      />
                    </TableCell>
                    <TableCell>
                      <span className="font-mono text-xs">{g.vmid}</span> {g.name}
                      {g.problem && <div className="text-xs text-destructive">{g.problem}</div>}
                    </TableCell>
                    <TableCell className="font-mono text-xs">{g.testVmid}</TableCell>
                    <TableCell className="text-xs">{formatBytes(g.memoryBytes)}</TableCell>
                    <TableCell className="text-xs">{g.configChangedAt ? `changed ${formatRelative(g.configChangedAt)}` : '—'}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
            <div className="grid gap-2">
              <Label htmlFor="test-point">Point in time</Label>
              <NativeSelect id="test-point" value={snapshot} onChange={(e) => setSnapshot(e.target.value)}>
                {options.pointsInTime.map((p, i) => (
                  <option key={p.snapshot} value={p.snapshot}>
                    {formatDateTime(p.createdAt)}
                    {i === 0 ? ' (newest)' : ''}
                  </option>
                ))}
              </NativeSelect>
            </div>
            {short && (
              <Alert>
                <TriangleAlert />
                <AlertDescription>
                  The selected guests use {formatBytes(memory)} of memory, but the DR host has {formatBytes(options.memoryAvailableBytes)}{' '}
                  available. Some may not start.
                </AlertDescription>
              </Alert>
            )}
          </div>
        )}
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button onClick={() => void start()} disabled={busy || !options || selected.size === 0 || !snapshot}>
            Start test
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

const guestIcon: Record<string, React.ReactNode> = {
  pending: <Circle className="size-4 text-muted-foreground" />,
  starting: <Loader2 className="size-4 animate-spin" />,
  running: <CheckCircle2 className="size-4 text-success" />,
  failed: <XCircle className="size-4 text-destructive" />,
  stopped: <MinusCircle className="size-4 text-muted-foreground" />,
}

function GuestsTable({ test }: { test: TestRun }) {
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>Guest</TableHead>
          <TableHead>Test ID</TableHead>
          <TableHead>Result</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {test.guests.map((g) => (
          <TableRow key={g.vmid}>
            <TableCell>
              <span className="font-mono text-xs">{g.vmid}</span> {g.name}
            </TableCell>
            <TableCell className="font-mono text-xs">{g.testVmid}</TableCell>
            <TableCell className="text-xs">
              <span className="flex items-center gap-1">
                {guestIcon[g.status] ?? guestIcon.pending} {g.status}
                {g.agentEnabled && (g.agentOk ? ', guest agent answered' : '')}
              </span>
              {g.detail && <div className="text-destructive">{g.detail}</div>}
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  )
}

// TestDetails shows a test's steps and guests, extends or ends it while it
// runs, and records its verdict.
export function TestDetails({ test, onChanged }: { test: TestRun; onChanged: () => void }) {
  const [error, setError] = useState<string>()
  async function act(fn: () => Promise<unknown>) {
    setError(undefined)
    try {
      await fn()
      onChanged()
    } catch (err) {
      setError(errorMessage(err))
    }
  }
  const failedStep = test.steps.find((s) => s.status === 'failed')
  const active = activeStates.includes(test.state)
  return (
    <div className="grid gap-4 text-sm">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <span>Point in time: {formatDateTime(test.snapshotAt)}</span>
        {active && test.state !== TestState.ENDING && (
          <div className="flex gap-2">
            <Button size="sm" variant="outline" onClick={() => void act(() => testClient.extendTest({ id: test.id }))}>
              Extend
            </Button>
            <AlertDialog>
              <AlertDialogTrigger render={<Button size="sm" variant="outline" />}>End test</AlertDialogTrigger>
              <AlertDialogContent>
                <AlertDialogHeader>
                  <AlertDialogTitle>End the test?</AlertDialogTitle>
                  <AlertDialogDescription>
                    The test guests are stopped and destroyed with their clones. Replicas and snapshots aren't touched.
                  </AlertDialogDescription>
                </AlertDialogHeader>
                <AlertDialogFooter>
                  <AlertDialogCancel>Cancel</AlertDialogCancel>
                  <AlertDialogAction onClick={() => void act(() => testClient.endTest({ id: test.id }))}>End test</AlertDialogAction>
                </AlertDialogFooter>
              </AlertDialogContent>
            </AlertDialog>
          </div>
        )}
      </div>
      {active && test.state !== TestState.ENDING && (
        <div className="text-xs text-muted-foreground">
          Ends by itself {formatRelative(test.deadline)} ({toDate(test.deadline)?.toLocaleString()}).
        </div>
      )}
      {test.endedAt && (
        <div className="text-xs text-muted-foreground">
          Ended {formatRelative(test.endedAt)}
          {test.endedBy && ` by ${test.endedBy}`}.
        </div>
      )}
      <ErrorAlert message={error} />
      {(failedStep || test.error) && (
        <div className="flex gap-1 text-xs text-destructive">
          <CircleAlert className="mt-0.5 size-3 shrink-0" /> {test.error || `${failedStep?.name}: ${failedStep?.detail}`}
        </div>
      )}
      <ol className="grid gap-1 text-xs">
        {test.steps.map((s, i) => (
          <li key={i} className="flex gap-2">
            {guestIcon[s.status === 'done' ? 'running' : s.status === 'running' ? 'starting' : s.status] ?? guestIcon.pending}
            <span>
              {s.name}
              {s.detail && <span className="text-muted-foreground"> — {s.detail}</span>}
            </span>
          </li>
        ))}
      </ol>
      <GuestsTable test={test} />
      {test.notes.length > 0 && <Notes notes={test.notes} />}
      <VerdictForm test={test} onSaved={onChanged} />
    </div>
  )
}

function Notes({ notes }: { notes: string[] }) {
  return (
    <details className="text-xs">
      <summary className="cursor-pointer">Settings removed from test guests ({notes.length})</summary>
      <ul className="list-disc pl-5 font-mono text-muted-foreground">
        {notes.map((n) => (
          <li key={n}>{n}</li>
        ))}
      </ul>
    </details>
  )
}

function VerdictForm({ test, onSaved }: { test: TestRun; onSaved: () => void }) {
  const [verdict, setVerdict] = useState(test.verdict)
  const [notes, setNotes] = useState(test.verdictNotes)
  const [error, setError] = useState<string>()
  const [saved, setSaved] = useState(false)
  async function save() {
    setError(undefined)
    try {
      await testClient.setTestVerdict({ id: test.id, verdict, notes })
      setSaved(true)
      onSaved()
    } catch (err) {
      setError(errorMessage(err))
    }
  }
  return (
    <div className="grid gap-2 text-sm">
      <Label htmlFor={`verdict-${test.id}`}>Verdict</Label>
      <div className="flex flex-wrap items-center gap-4">
        {[
          { v: TestVerdict.PASSED, label: 'Passed' },
          { v: TestVerdict.FAILED, label: 'Failed' },
        ].map((o) => (
          <label key={o.v} className="flex items-center gap-2">
            <input
              type="radio"
              name={`verdict-${test.id}`}
              checked={verdict === o.v}
              onChange={() => {
                setVerdict(o.v)
                setSaved(false)
              }}
            />
            {o.label}
          </label>
        ))}
      </div>
      <textarea
        id={`verdict-${test.id}`}
        className="min-h-16 rounded-md border bg-transparent px-3 py-2 text-sm"
        placeholder="What was checked, and what didn't work"
        value={notes}
        onChange={(e) => {
          setNotes(e.target.value)
          setSaved(false)
        }}
      />
      <div className="flex items-center gap-2">
        <Button size="sm" variant="outline" onClick={() => void save()} disabled={verdict === TestVerdict.UNSPECIFIED}>
          Save verdict
        </Button>
        {saved && <span className="text-xs text-muted-foreground">Saved</span>}
        {test.verdictBy && !saved && (
          <span className="text-xs text-muted-foreground">
            by {test.verdictBy} {formatRelative(test.verdictAt)}
          </span>
        )}
      </div>
      <ErrorAlert message={error} />
    </div>
  )
}
