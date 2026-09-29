import { clone, create } from '@bufbuild/protobuf'
import { ArrowLeft, ArrowRight, ChevronRight, CircleAlert, Plus, Trash2, TriangleAlert } from 'lucide-react'
import { createContext, useCallback, useContext, useEffect, useRef, useState } from 'react'
import { Link, useLocation, useNavigate, useParams } from 'react-router'

import { ErrorAlert } from '@/components/error-alert'
import { NativeSelect } from '@/components/native-select'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { GuestType, type Inventory } from '@/gen/ezdr/inventory/v1/inventory_pb'
import {
  DnsRecordSchema,
  type EzdrTunnel,
  EzdrTunnel_Listener,
  EzdrTunnelSchema,
  ExistingNetworkSchema,
  ReplicationNetworkSchema,
  DnsRecordType,
  type Issue,
  PlanGuestSchema,
  type PlanSpec,
  PlanSpecSchema,
  type RetentionTier,
  RetentionTierSchema,
  Severity,
} from '@/gen/ezdr/plan/v1/plan_pb'
import { type GetHostInventoryResponse, type GuestExclusion, type Host, type Plan, PlanState, type ZreplSetup } from '@/gen/ezdr/portal/v1/portal_pb'
import { HostStatusPanel, PlanActions, StateBadge } from '@/pages/plan-actions'
import { PlanDnsCard } from '@/pages/plan-dns'
import { PlanHistoryCard } from '@/pages/plan-history'
import { PlanStateCard, PlanStatusCard } from '@/pages/plan-status'
import { TestFailoverCard } from '@/pages/test-failover'
import { errorMessage, hostClient, planClient } from '@/lib/api'
import { formatBytes, formatDateTime, formatDuration } from '@/lib/format'
import { describeInterval, drPresets, grid, primaryPresets, splitPeriod, units } from '@/lib/retention'
import { Field } from '@/pages/setup'

type Update = (fn: (s: PlanSpec) => void) => void

// The page's sections, in order. Settings sections match the validation
// issues' sections (planv1.Issue.section).
type SectionId =
  | 'status'
  | 'tests'
  | 'history'
  | 'general'
  | 'takeover'
  | 'guests'
  | 'mappings'
  | 'network'
  | 'schedule'
  | 'startup'
  | 'dns'
  | 'advanced'

const sectionTitles: Record<SectionId, string> = {
  status: 'Status',
  tests: 'Test failover',
  history: 'History',
  general: 'General',
  takeover: 'Existing zrepl setup',
  guests: 'Guests',
  mappings: 'Mappings',
  network: 'Replication network',
  schedule: 'Schedule and retention',
  startup: 'Startup order',
  dns: 'DNS records',
  advanced: 'Advanced',
}

const sectionElementId = (id: string) => `section-${id}`

// Sections tells the settings cards whether they're open and which issues
// they have.
type SectionsState = {
  isOpen: (id: SectionId) => boolean
  toggle: (id: SectionId) => void
  issues: Issue[]
}

const Sections = createContext<SectionsState>({ isOpen: () => true, toggle: () => undefined, issues: [] })

function counts(issues: Issue[]) {
  return {
    errors: issues.filter((i) => i.severity === Severity.ERROR).length,
    warnings: issues.filter((i) => i.severity === Severity.WARNING).length,
  }
}

// chosen is the section last picked in the menu: it stays highlighted while
// the page scrolls to it (a section near the bottom may never reach the
// top), until the user scrolls by hand.
let chosen: string | undefined

function scrollToSection(id: string) {
  chosen = id
  // After the section has rendered open.
  requestAnimationFrame(() => document.getElementById(sectionElementId(id))?.scrollIntoView({ behavior: 'smooth', block: 'start' }))
}

// useActiveSection returns the section at the top of the viewport, or the
// one just chosen in the menu.
function useActiveSection(ids: string[]) {
  const [active, setActive] = useState<string>()
  // ids is a new array on every render; key tracks its content.
  const key = ids.join()
  useEffect(() => {
    const list = key.split(',')
    const update = () => {
      if (chosen && list.includes(chosen)) {
        setActive(chosen)
        return
      }
      let current = list[0]
      for (const id of list) {
        const el = document.getElementById(sectionElementId(id))
        if (el && el.getBoundingClientRect().top <= 120) current = id
      }
      setActive(current)
    }
    const byHand = () => {
      chosen = undefined
    }
    update()
    window.addEventListener('scroll', update, { passive: true })
    window.addEventListener('resize', update)
    for (const e of ['wheel', 'touchmove', 'keydown'] as const) window.addEventListener(e, byHand, { passive: true })
    return () => {
      window.removeEventListener('scroll', update)
      window.removeEventListener('resize', update)
      for (const e of ['wheel', 'touchmove', 'keydown'] as const) window.removeEventListener(e, byHand)
    }
  }, [key])
  return active
}

export function PlanEditorPage() {
  const { id } = useParams()
  const isNew = !id
  const navigate = useNavigate()
  const { hash } = useLocation()
  const [spec, setSpec] = useState<PlanSpec>()
  const [plan, setPlan] = useState<Plan>()
  const [dirty, setDirty] = useState(false)
  const [hosts, setHosts] = useState<Host[]>([])
  const [primary, setPrimary] = useState<GetHostInventoryResponse>()
  const [dr, setDr] = useState<GetHostInventoryResponse>()
  const [issues, setIssues] = useState<Issue[]>([])
  const [error, setError] = useState<string>()
  const [saving, setSaving] = useState(false)
  const [savedAt, setSavedAt] = useState<Date>()
  const [opened, setOpened] = useState<Partial<Record<SectionId, boolean>>>({})
  const [startTest, setStartTest] = useState(0)
  // Changes when something besides the plan affects validation.
  const [revalidate, setRevalidate] = useState(0)

  // Load the plan (or defaults for a new one) and the host list.
  useEffect(() => {
    let active = true
    void (async () => {
      try {
        const [list, loaded] = await Promise.all([
          hostClient.listHosts({}),
          isNew ? planClient.suggestPlan({}).then((r) => ({ spec: r.spec, plan: undefined })) : planClient.getPlan({ id }).then((r) => ({ spec: r.plan?.spec, plan: r.plan })),
        ])
        if (!active) return
        setHosts(list.hosts)
        setSpec(loaded.spec ?? create(PlanSpecSchema))
        setPlan(loaded.plan)
      } catch (err) {
        if (active) setError(errorMessage(err))
      }
    })()
    return () => {
      active = false
    }
  }, [id, isNew])

  // Refresh the stored plan (state and host progress) periodically.
  useEffect(() => {
    if (isNew) return
    const t = setInterval(() => {
      void planClient.getPlan({ id }).then((r) => setPlan(r.plan), () => undefined)
    }, 5000)
    return () => clearInterval(t)
  }, [id, isNew])

  // Load inventories when hosts change.
  const primaryId = spec?.primaryHostId ?? ''
  const drId = spec?.drHostId ?? ''
  useEffect(() => {
    if (!primaryId) return
    void hostClient.getHostInventory({ hostId: primaryId }).then(setPrimary, (e) => setError(errorMessage(e)))
  }, [primaryId])
  useEffect(() => {
    if (!drId) return
    void hostClient.getHostInventory({ hostId: drId }).then(setDr, (e) => setError(errorMessage(e)))
  }, [drId])

  // Validate as the plan changes (debounced).
  const validateTimer = useRef<ReturnType<typeof setTimeout>>(undefined)
  useEffect(() => {
    if (!spec) return
    clearTimeout(validateTimer.current)
    validateTimer.current = setTimeout(() => {
      void planClient.validatePlan({ spec, planId: id ?? '' }).then((r) => setIssues(r.issues), () => undefined)
    }, 300)
    return () => clearTimeout(validateTimer.current)
  }, [spec, id, revalidate])

  // Warn before leaving the page with unsaved changes.
  useEffect(() => {
    if (!dirty) return
    const warn = (e: BeforeUnloadEvent) => e.preventDefault()
    window.addEventListener('beforeunload', warn)
    return () => window.removeEventListener('beforeunload', warn)
  }, [dirty])

  const update: Update = (fn) => {
    setDirty(true)
    setSpec((prev) => {
      if (!prev) return prev
      const next = clone(PlanSpecSchema, prev)
      fn(next)
      return next
    })
  }

  // Changes to hosts or guests can introduce new storage, bridges, or
  // guests; ask the portal to fill in suggestions for them.
  async function updateAndSuggest(fn: (s: PlanSpec) => void) {
    if (!spec) return
    const next = clone(PlanSpecSchema, spec)
    fn(next)
    setSpec(next)
    setDirty(true)
    try {
      const r = await planClient.suggestPlan({ spec: next })
      if (r.spec) setSpec(r.spec)
    } catch (err) {
      setError(errorMessage(err))
    }
  }

  async function save() {
    if (!spec) return
    setSaving(true)
    setError(undefined)
    try {
      if (isNew) {
        const r = await planClient.createPlan({ spec })
        setDirty(false)
        navigate(`/plans/${r.plan?.id}`, { replace: true })
        return
      }
      const r = await planClient.updatePlan({ id, spec })
      setIssues(r.issues)
      setPlan(r.plan)
      setSavedAt(new Date())
      setDirty(false)
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setSaving(false)
    }
  }

  async function discard() {
    if (isNew) {
      navigate('/plans')
      return
    }
    try {
      const r = await planClient.getPlan({ id })
      setPlan(r.plan)
      if (r.plan?.spec) setSpec(r.plan.spec)
      setDirty(false)
    } catch (err) {
      setError(errorMessage(err))
    }
  }

  // Sections with errors open, and stay open once fixed.
  const errorSections = issues.filter((i) => i.severity === Severity.ERROR).map((i) => i.section as SectionId)
  const errorKey = [...new Set(errorSections)].sort().join()
  const [seenErrors, setSeenErrors] = useState('')
  if (errorKey !== seenErrors) {
    setSeenErrors(errorKey)
    if (errorKey) {
      setOpened((o) => {
        const next = { ...o }
        for (const s of errorKey.split(',') as SectionId[]) next[s] ??= true
        return next
      })
    }
  }

  const isOpen = useCallback((s: SectionId) => isNew || (opened[s] ?? false), [isNew, opened])
  const toggle = useCallback((s: SectionId) => setOpened((o) => ({ ...o, [s]: !(o[s] ?? false) })), [])
  const open = (s: SectionId) => {
    setOpened((o) => ({ ...o, [s]: true }))
    scrollToSection(s)
  }

  const primaryInv = primaryId ? primary?.inventory : undefined
  const drInv = drId ? dr?.inventory : undefined
  const guests = spec?.guests.length ?? 0
  const showStatus = !!plan && plan.state !== PlanState.DRAFT
  const showTests = plan?.state === PlanState.ACTIVE || plan?.state === PlanState.PAUSED
  const showTakeover = !!primaryInv && !!drInv && (!plan || plan.state === PlanState.DRAFT)
  const visible: SectionId[] = [
    ...(showStatus ? (['status'] as const) : []),
    ...(showTests ? (['tests'] as const) : []),
    ...(plan ? (['history'] as const) : []),
    'general',
    ...(showTakeover && spec?.takeover ? (['takeover'] as const) : []),
    ...(primaryInv ? (['guests'] as const) : []),
    ...(primaryInv && drInv && guests > 0 ? (['mappings'] as const) : []),
    ...(primaryInv ? (['network'] as const) : []),
    'schedule',
    ...(primaryInv && guests > 0 ? (['startup', 'dns'] as const) : []),
    'advanced',
  ]
  const active = useActiveSection(visible)

  // Open and show the section in the address (for example, #guests from
  // the overview's guest settings), once it exists.
  const target = hash.slice(1) as SectionId
  const targetVisible = visible.includes(target)
  const [seenTarget, setSeenTarget] = useState('')
  if (targetVisible && target !== seenTarget) {
    setSeenTarget(target)
    setOpened((o) => ({ ...o, [target]: true }))
  }
  useEffect(() => {
    if (targetVisible) scrollToSection(target)
  }, [target, targetVisible])

  if (!spec) return <ErrorAlert message={error} />
  const hostname = (hid: string) => hosts.find((h) => h.id === hid)?.hostname
  const { errors } = counts(issues)

  return (
    <Sections.Provider value={{ isOpen, toggle, issues }}>
      <div className="grid gap-3">
        <Link to="/plans" className="flex w-fit items-center gap-1 text-sm text-muted-foreground hover:text-foreground">
          <ArrowLeft className="size-4" /> DR plans
        </Link>
        <div className="flex flex-wrap items-start justify-between gap-4">
          <div className="grid gap-1">
            <div className="flex flex-wrap items-center gap-2">
              <h1 className="text-2xl font-semibold tracking-tight">{isNew ? 'New DR plan' : spec.name || 'DR plan'}</h1>
              {plan && <StateBadge state={plan.state} pending={plan.pendingChanges} />}
              {savedAt && !dirty && <span className="text-xs text-muted-foreground">Saved {savedAt.toLocaleTimeString()}</span>}
            </div>
            <p className="flex flex-wrap items-center gap-1.5 text-sm text-muted-foreground">
              {spec.primaryHostId && spec.drHostId ? (
                <>
                  {hostname(spec.primaryHostId)} <ArrowRight className="size-3.5" /> {hostname(spec.drHostId)}
                </>
              ) : (
                'Choose a primary host and a DR host.'
              )}
              {spec.description && <span>· {spec.description}</span>}
            </p>
          </div>
          {plan && (
            <PlanActions
              plan={plan}
              dirty={dirty}
              onChanged={(p) => {
                setPlan(p)
                if (p.spec) setSpec(p.spec)
              }}
              onStartTest={() => {
                open('tests')
                setStartTest((n) => n + 1)
              }}
              onDeleted={() => navigate('/plans')}
            />
          )}
        </div>
      </div>
      <ErrorAlert message={error} />
      <div className="grid items-start gap-6 lg:grid-cols-[230px_minmax(0,1fr)]">
        <aside className="hidden gap-4 lg:sticky lg:top-16 lg:grid lg:max-h-[calc(100svh-5rem)] lg:overflow-y-auto">
          <SectionNav ids={visible} active={active} issues={issues} onSelect={open} />
          {plan && <HostStatusPanel plan={plan} />}
          <ValidationPanel issues={issues} onSelect={open} />
        </aside>
        <div className="grid min-w-0 gap-4">
          {showStatus && plan && (
            <div id={sectionElementId('status')} className="grid scroll-mt-16 gap-4">
              <PlanStatusCard plan={plan} />
              <PlanStateCard plan={plan} drHostname={hostname(plan.spec?.drHostId ?? '')} />
              <PlanDnsCard plan={plan} />
            </div>
          )}
          {showTests && plan && (
            <div id={sectionElementId('tests')} className="scroll-mt-16">
              <TestFailoverCard plan={plan} startSignal={startTest} />
            </div>
          )}
          {plan && (
            <div id={sectionElementId('history')} className="scroll-mt-16">
              <PlanHistoryCard plan={plan} />
            </div>
          )}
          {plan && <h2 className="pt-2 text-lg font-semibold tracking-tight">Settings</h2>}
          <GeneralCard spec={spec} hosts={hosts} update={update} updateAndSuggest={updateAndSuggest} />
          {showTakeover && primaryInv && drInv && (
            <TakeoverCard
              spec={spec}
              inventoriesAt={`${primary?.receivedAt?.seconds}/${dr?.receivedAt?.seconds}`}
              update={update}
              replace={(s) => {
                setSpec(s)
                setDirty(true)
              }}
            />
          )}
          {primaryInv && (
            <GuestsCard
              spec={spec}
              inv={primaryInv}
              guestPlans={primary?.guestPlans ?? {}}
              excluded={primary?.excludedGuests ?? {}}
              planId={id}
              updateAndSuggest={updateAndSuggest}
              setExcluded={async (vmid, excluded) => {
                try {
                  await hostClient.setGuestExcluded({ hostId: primaryId, vmid, excluded })
                  setPrimary(await hostClient.getHostInventory({ hostId: primaryId }))
                  setRevalidate((n) => n + 1)
                } catch (err) {
                  setError(errorMessage(err))
                }
              }}
            />
          )}
          {primaryInv && drInv && guests > 0 && (
            <MappingsCard spec={spec} primaryInv={primaryInv} drInv={drInv} update={update} updateAndSuggest={updateAndSuggest} />
          )}
          {primaryInv && <NetworkCard spec={spec} update={update} />}
          <ScheduleCard spec={spec} update={update} />
          {primaryInv && guests > 0 && <StartupCard spec={spec} inv={primaryInv} update={update} />}
          {primaryInv && guests > 0 && <DnsCard spec={spec} inv={primaryInv} update={update} />}
          <AdvancedCard spec={spec} update={update} />
          <div className="lg:hidden">
            <ValidationPanel issues={issues} onSelect={open} />
          </div>
          {(dirty || isNew) && (
            <div className="sticky bottom-4 z-20 flex flex-wrap items-center gap-x-4 gap-y-2 rounded-lg border bg-card/95 px-4 py-3 shadow-lg backdrop-blur">
              <div className="grid">
                <span className="text-sm font-medium">{isNew ? 'New plan, not saved yet' : 'Unsaved changes'}</span>
                <span className="text-xs text-muted-foreground">
                  {errors > 0 && (
                    <span className="text-destructive">
                      {errors} error{errors === 1 ? '' : 's'} to fix before activation.{' '}
                    </span>
                  )}
                  {!plan || plan.state === PlanState.DRAFT
                    ? 'Drafts are saved without changing the hosts.'
                    : 'Saved changes stay pending until you apply them.'}
                </span>
              </div>
              <div className="ml-auto flex gap-2">
                <Button variant="ghost" onClick={() => void discard()}>
                  {isNew ? 'Cancel' : 'Discard'}
                </Button>
                <Button onClick={() => void save()} disabled={saving}>
                  {isNew ? 'Create plan' : 'Save'}
                </Button>
              </div>
            </div>
          )}
        </div>
      </div>
    </Sections.Provider>
  )
}

// SectionNav lists the page's sections, marks the one in view and those
// with issues, and jumps to one when chosen.
function SectionNav({
  ids,
  active,
  issues,
  onSelect,
}: {
  ids: SectionId[]
  active?: string
  issues: Issue[]
  onSelect: (id: SectionId) => void
}) {
  const settingsStart = ids.findIndex((i) => i !== 'status' && i !== 'tests' && i !== 'history')
  return (
    <nav className="grid gap-0.5 text-sm" aria-label="Plan sections">
      {ids.map((id, n) => {
        const { errors, warnings } = counts(issues.filter((i) => i.section === id))
        return (
          <div key={id}>
            {n === settingsStart && n > 0 && <div className="mt-3 mb-1 px-2 text-xs font-medium text-muted-foreground">Settings</div>}
            <button
              type="button"
              onClick={() => onSelect(id)}
              className={`flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left transition-colors ${
                active === id ? 'bg-muted font-medium text-foreground' : 'text-muted-foreground hover:bg-muted/60 hover:text-foreground'
              }`}
            >
              <span className="truncate">{sectionTitles[id]}</span>
              {errors > 0 ? (
                <CircleAlert className="ml-auto size-3.5 shrink-0 text-destructive" aria-label={`${errors} errors`} />
              ) : warnings > 0 ? (
                <TriangleAlert className="ml-auto size-3.5 shrink-0 text-warning-foreground" aria-label={`${warnings} warnings`} />
              ) : null}
            </button>
          </div>
        )
      })}
    </nav>
  )
}

// Section is a settings card that collapses to a one-line summary.
function Section({
  id,
  description,
  summary,
  children,
}: {
  id: SectionId
  description?: string
  summary: React.ReactNode
  children: React.ReactNode
}) {
  const { isOpen, toggle, issues } = useContext(Sections)
  const open = isOpen(id)
  const { errors, warnings } = counts(issues.filter((i) => i.section === id))
  return (
    <Card id={sectionElementId(id)} className="scroll-mt-16">
      <CardHeader>
        <button type="button" className="flex w-full items-start gap-2 text-left" aria-expanded={open} onClick={() => toggle(id)}>
          <ChevronRight className={`mt-0.5 size-4 shrink-0 text-muted-foreground transition-transform ${open ? 'rotate-90' : ''}`} />
          <div className="grid min-w-0 flex-1 gap-1">
            <CardTitle>{sectionTitles[id]}</CardTitle>
            <CardDescription className={open ? '' : 'truncate'}>{open ? description : summary}</CardDescription>
          </div>
          {errors > 0 && (
            <Badge variant="destructive">
              {errors} error{errors === 1 ? '' : 's'}
            </Badge>
          )}
          {warnings > 0 && (
            <Badge variant="warning">
              {warnings} warning{warnings === 1 ? '' : 's'}
            </Badge>
          )}
        </button>
      </CardHeader>
      {open && <CardContent className="grid gap-4">{children}</CardContent>}
    </Card>
  )
}

function GeneralCard({
  spec,
  hosts,
  update,
  updateAndSuggest,
}: {
  spec: PlanSpec
  hosts: Host[]
  update: Update
  updateAndSuggest: (fn: (s: PlanSpec) => void) => Promise<void>
}) {
  const clearMappings = (s: PlanSpec) => {
    s.storageMappings = []
    s.networkMappings = []
    s.testBridge = ''
  }
  const name = (hid: string) => hosts.find((h) => h.id === hid)?.hostname ?? 'not chosen'
  return (
    <Section
      id="general"
      summary={`${spec.name || 'Unnamed'} · ${name(spec.primaryHostId)} → ${name(spec.drHostId)}${spec.description ? ` · ${spec.description}` : ''}`}
    >
      <div className="grid gap-4 sm:grid-cols-2">
        <Field id="name" label="Name">
          <Input id="name" value={spec.name} onChange={(e) => update((s) => (s.name = e.target.value))} maxLength={100} />
        </Field>
        <Field id="description" label="Description (optional)">
          <Input id="description" value={spec.description} onChange={(e) => update((s) => (s.description = e.target.value))} />
        </Field>
        <Field id="primary" label="Primary host">
          <NativeSelect
            id="primary"
            value={spec.primaryHostId}
            onChange={(e) =>
              void updateAndSuggest((s) => {
                s.primaryHostId = e.target.value
                s.guests = []
                clearMappings(s)
              })
            }
          >
            <option value="">Choose…</option>
            {hosts.map((h) => (
              <option key={h.id} value={h.id}>
                {h.hostname}
              </option>
            ))}
          </NativeSelect>
        </Field>
        <Field id="dr" label="DR host">
          <NativeSelect
            id="dr"
            value={spec.drHostId}
            onChange={(e) =>
              void updateAndSuggest((s) => {
                s.drHostId = e.target.value
                clearMappings(s)
              })
            }
          >
            <option value="">Choose…</option>
            {hosts
              .filter((h) => h.id !== spec.primaryHostId)
              .map((h) => (
                <option key={h.id} value={h.id}>
                  {h.hostname}
                </option>
              ))}
          </NativeSelect>
        </Field>
      </div>
    </Section>
  )
}

// TakeoverCard offers existing hand-written zrepl setups between the plan's
// hosts for adoption, and shows the adopted one.
function TakeoverCard({
  spec,
  inventoriesAt,
  update,
  replace,
}: {
  spec: PlanSpec
  inventoriesAt: string
  update: Update
  replace: (s: PlanSpec) => void
}) {
  const [setups, setSetups] = useState<ZreplSetup[]>([])
  const [notes, setNotes] = useState<string[]>()
  const [error, setError] = useState<string>()
  const [busy, setBusy] = useState(false)
  const { primaryHostId, drHostId } = spec
  useEffect(() => {
    void planClient.listZreplSetups({ primaryHostId, drHostId }).then(
      (r) => setSetups(r.setups),
      (e) => setError(errorMessage(e)),
    )
  }, [primaryHostId, drHostId, inventoriesAt])

  async function adopt(setup: ZreplSetup) {
    setBusy(true)
    setError(undefined)
    try {
      const r = await planClient.adoptZreplSetup({ spec, sourceJob: setup.sourceJob?.name, pullJob: setup.pullJob?.name })
      if (r.spec) replace(r.spec)
      setNotes(r.notes)
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setBusy(false)
    }
  }

  const t = spec.takeover
  if (!t && setups.length === 0 && !error) return null
  return (
    <Section
      id="takeover"
      summary={t ? `Taking over ${t.sourceJob} → ${t.pullJob}` : `${setups.length} hand-written setup${setups.length === 1 ? '' : 's'} found`}
      description={
        t
          ? 'Activating this plan takes over the hand-written zrepl jobs below. A preflight checks that replication continues incrementally before anything changes.'
          : 'These hosts already replicate with hand-written zrepl jobs. Adopting them fills in the plan from their settings, so replication can continue incrementally instead of starting over.'
      }
    >
      <ErrorAlert message={error} />
      {t ? (
        <>
          <div className="grid gap-1 text-sm">
            <div>
              Primary: <span className="font-mono">{t.sourceJob}</span>
            </div>
            <div>
              DR host: <span className="font-mono">{t.pullJob}</span>
            </div>
          </div>
          {notes && notes.length > 0 && (
            <div className="grid gap-1 text-sm">
              <div className="font-medium">Differences from the old setup</div>
              <ul className="list-disc pl-5 text-muted-foreground">
                {notes.map((n) => (
                  <li key={n}>{n}</li>
                ))}
              </ul>
            </div>
          )}
          {notes && <p className="text-sm text-muted-foreground">Review the settings below, then save the plan.</p>}
          <div>
            <Button
              variant="outline"
              onClick={() => {
                update((s) => (s.takeover = undefined))
                setNotes(undefined)
              }}
            >
              Stop adopting
            </Button>
          </div>
        </>
      ) : (
        setups.map((c) => (
          <div key={`${c.sourceJob?.name}/${c.pullJob?.name}`} className="flex flex-wrap items-center justify-between gap-3 rounded-md border p-3 text-sm">
            <div className="grid gap-1">
              <div>
                <span className="font-mono">{c.sourceJob?.name}</span> on the primary →{' '}
                <span className="font-mono">{c.pullJob?.name}</span> on the DR host
              </div>
              <div className="text-xs text-muted-foreground">
                {c.pullJob?.connectAddress} · prefix <span className="font-mono">{c.sourceJob?.snapshotPrefix || '—'}</span> · receives into{' '}
                <span className="font-mono">{c.pullJob?.rootFs}</span>
              </div>
            </div>
            <Button onClick={() => void adopt(c)} disabled={busy}>
              Adopt
            </Button>
          </div>
        ))
      )}
    </Section>
  )
}

function GuestsCard({
  spec,
  inv,
  guestPlans,
  excluded,
  planId,
  updateAndSuggest,
  setExcluded,
}: {
  spec: PlanSpec
  inv: Inventory
  guestPlans: GetHostInventoryResponse['guestPlans']
  excluded: GetHostInventoryResponse['excludedGuests']
  planId?: string
  updateAndSuggest: (fn: (s: PlanSpec) => void) => Promise<void>
  // Records (or clears) the choice not to protect a guest.
  setExcluded: (vmid: number, excluded: boolean) => Promise<void>
}) {
  const selected = new Set(spec.guests.map((g) => g.vmid))
  const otherPlan = (vmid: number) => {
    const p = guestPlans[vmid]
    return p && p.id !== planId ? p : undefined
  }
  const selectable = inv.guests.filter((g) => !g.template && !otherPlan(g.vmid) && !excluded[g.vmid])
  const unconfigured = selectable.filter((g) => !selected.has(g.vmid)).length

  function toggle(vmid: number, on: boolean) {
    void updateAndSuggest((s) => {
      s.guests = on ? [...s.guests, create(PlanGuestSchema, { vmid })] : s.guests.filter((g) => g.vmid !== vmid)
    })
  }
  function selectAllReplicable() {
    void updateAndSuggest((s) => {
      const have = new Set(s.guests.map((g) => g.vmid))
      for (const g of selectable) {
        if (g.ready && !have.has(g.vmid)) s.guests.push(create(PlanGuestSchema, { vmid: g.vmid }))
      }
    })
  }

  return (
    <Section
      id="guests"
      description={`${spec.guests.length} of ${inv.guests.length} guests on ${inv.host?.hostname} selected.`}
      summary={[
        spec.guests.length === 0
          ? 'No guests selected'
          : `${spec.guests.length} guest${spec.guests.length === 1 ? '' : 's'}: ${spec.guests.map((g) => guestName(inv, g.vmid)).join(', ')}`,
        unconfigured > 0 && `${unconfigured} unconfigured on ${inv.host?.hostname}`,
      ]
        .filter(Boolean)
        .join(' · ')}
    >
      <div className="flex gap-2">
        <Button variant="outline" size="sm" onClick={selectAllReplicable}>
          Select all replicable
        </Button>
        <Button variant="ghost" size="sm" onClick={() => void updateAndSuggest((s) => (s.guests = []))}>
          Clear
        </Button>
      </div>
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead className="w-8" />
            <TableHead>ID</TableHead>
            <TableHead>Name</TableHead>
            <TableHead>Replication</TableHead>
            <TableHead className="text-right">Status</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {inv.guests.map((g) => {
            const other = otherPlan(g.vmid)
            return (
              <TableRow key={g.vmid}>
                <TableCell>
                  <input
                    type="checkbox"
                    aria-label={`Protect ${g.name}`}
                    checked={selected.has(g.vmid)}
                    disabled={!!other || g.template || !!excluded[g.vmid]}
                    onChange={(e) => toggle(g.vmid, e.target.checked)}
                  />
                </TableCell>
                <TableCell className="font-mono text-xs">{g.vmid}</TableCell>
                <TableCell>
                  {g.name}{' '}
                  <span className="text-xs text-muted-foreground">
                    {g.type === GuestType.VM ? 'VM' : 'container'}
                    {g.template && ' · template'}
                  </span>
                </TableCell>
                <TableCell className="text-xs">
                  {other ? (
                    <span className="text-muted-foreground">in plan {other.name}</span>
                  ) : g.ready ? (
                    <Badge variant="success">Ready</Badge>
                  ) : (
                    <Badge variant="destructive">Not ready</Badge>
                  )}
                </TableCell>
                <TableCell className="text-right text-xs">
                  <GuestStatus
                    protectedHere={selected.has(g.vmid)}
                    inOtherPlan={!!other}
                    template={g.template}
                    exclusion={excluded[g.vmid]}
                    setExcluded={(on) => setExcluded(g.vmid, on)}
                  />
                </TableCell>
              </TableRow>
            )
          })}
        </TableBody>
      </Table>
    </Section>
  )
}

// GuestStatus says whether a guest is protected, and lets a guest in no
// plan be marked unprotected (a deliberate choice, so plans stop warning)
// or back.
function GuestStatus({
  protectedHere,
  inOtherPlan,
  template,
  exclusion,
  setExcluded,
}: {
  protectedHere: boolean
  inOtherPlan: boolean
  template: boolean
  exclusion?: GuestExclusion
  setExcluded: (on: boolean) => Promise<void>
}) {
  const [busy, setBusy] = useState(false)
  const run = async (on: boolean) => {
    setBusy(true)
    await setExcluded(on)
    setBusy(false)
  }
  if (template) return <span className="text-muted-foreground">Template</span>
  if (protectedHere || inOtherPlan) return <span className="text-success">Protected</span>
  if (exclusion) {
    return (
      <span className="inline-flex items-center gap-2">
        <span className="text-muted-foreground" title={`Marked by ${exclusion.by}, ${formatDateTime(exclusion.at)}`}>
          Unprotected
        </span>
        <Button variant="ghost" size="xs" disabled={busy} onClick={() => void run(false)}>
          Undo
        </Button>
      </span>
    )
  }
  return (
    <span className="inline-flex items-center gap-2">
      <span className="text-warning-foreground">Unconfigured</span>
      <Button variant="outline" size="xs" disabled={busy} onClick={() => void run(true)} title="Stop warning about this guest">
        Mark unprotected
      </Button>
    </span>
  )
}

function MappingsCard({
  spec,
  primaryInv,
  drInv,
  update,
  updateAndSuggest,
}: {
  spec: PlanSpec
  primaryInv: Inventory
  drInv: Inventory
  update: Update
  updateAndSuggest: (fn: (s: PlanSpec) => void) => Promise<void>
}) {
  const pools = drInv.zfsPools
  const bridges = drInv.interfaces.filter((i) => i.type === 'bridge')
  const primaryName = primaryInv.host?.hostname ?? 'primary'
  return (
    <Section
      id="mappings"
      description="Where protected guests' disks and networks go on the DR host."
      summary={[
        ...spec.storageMappings.map((m) => `${m.sourceStorage} → ${m.receiveDataset || '?'}`),
        ...spec.networkMappings.map((m) => `${m.sourceBridge} → ${m.targetBridge || '?'}`),
        spec.testBridge ? `test bridge ${spec.testBridge}` : 'no test bridge',
      ].join(' · ')}
    >
      <div className="grid gap-2">
        <Label>Storage</Label>
        <p className="text-xs text-muted-foreground">
          Where each storage's disks are replicated on the DR host: a ZFS pool, and a dataset inside it that holds the replicas, each disk at
          its full path. Change the dataset to keep replicas apart from other data, or to match an existing zrepl setup (Adopt fills it
          in).
        </p>
        <div className="hidden gap-2 text-xs font-medium text-muted-foreground sm:grid sm:grid-cols-[160px_24px_minmax(0,220px)_12px_1fr]">
          <span>On {primaryName}</span>
          <span />
          <span>ZFS pool on the DR host</span>
          <span />
          <span>Dataset in the pool</span>
        </div>
        {spec.storageMappings.map((m, i) => {
          const example = exampleDisk(spec, primaryInv, m.sourceStorage)
          const [pool, ...rest] = m.receiveDataset.split('/')
          const path = rest.join('/')
          const setDataset = (p: string, sub: string) =>
            update((s) => (s.storageMappings[i].receiveDataset = sub.trim() ? `${p}/${sub.trim().replace(/^\/+/, '')}` : p))
          return (
            <div key={m.sourceStorage} className="grid gap-1">
              <div className="grid items-center gap-2 sm:grid-cols-[160px_24px_minmax(0,220px)_12px_1fr]">
                <span className="font-mono text-sm">{m.sourceStorage}</span>
                <span className="text-muted-foreground">→</span>
                <NativeSelect
                  aria-label={`ZFS pool for ${m.sourceStorage}`}
                  value={pool}
                  onChange={(e) => setDataset(e.target.value, path || `ezdr/${primaryName}`)}
                >
                  <option value="">Choose a pool…</option>
                  {pool && !pools.some((p) => p.name === pool) && <option value={pool}>{pool} (not on the DR host)</option>}
                  {pools.map((p) => (
                    <option key={p.name} value={p.name}>
                      {p.name} ({formatBytes(p.freeBytes)} free)
                    </option>
                  ))}
                </NativeSelect>
                <span className="text-center font-mono text-muted-foreground">/</span>
                <Input
                  aria-label={`Dataset for ${m.sourceStorage}'s replicas, in the pool`}
                  value={path}
                  disabled={!pool}
                  onChange={(e) => setDataset(pool, e.target.value)}
                  placeholder="replicated"
                  className="font-mono text-xs"
                />
              </div>
              {example && pool && (
                <p className="text-xs text-muted-foreground sm:pl-[184px]">
                  For example, <span className="font-mono">{example}</span> is copied to{' '}
                  <span className="font-mono">
                    {m.receiveDataset.replace(/\/+$/, '')}/{example}
                  </span>
                </p>
              )}
            </div>
          )
        })}
      </div>
      <div className="grid gap-2">
        <Label>Networks</Label>
        <p className="text-xs text-muted-foreground">
          The bridge each guest network connects to at the DR site after a failover. VLAN tags are kept, so a VLAN-aware bridge carries
          tagged networks.
        </p>
        <div className="hidden gap-2 text-xs font-medium text-muted-foreground sm:grid sm:grid-cols-[160px_24px_1fr]">
          <span>Bridge on {primaryName}</span>
          <span />
          <span>Bridge on the DR host</span>
        </div>
        {spec.networkMappings.map((m, i) => (
          <div key={m.sourceBridge} className="grid items-center gap-2 sm:grid-cols-[160px_24px_1fr]">
            <span className="font-mono text-sm">{m.sourceBridge}</span>
            <span className="text-muted-foreground">→</span>
            <NativeSelect
              aria-label={`DR bridge for ${m.sourceBridge}`}
              value={m.targetBridge}
              // Choosing a bridge can settle which bridge is left for test
              // failovers, so ask for suggestions again.
              onChange={(e) => void updateAndSuggest((s) => (s.networkMappings[i].targetBridge = e.target.value))}
            >
              <option value="">Choose DR bridge…</option>
              {bridges.map((b) => (
                <option key={b.name} value={b.name}>
                  {b.name}
                  {b.bridgePorts.length === 0 ? ' (no ports)' : ` (${b.bridgePorts.join(', ')})`}
                  {b.vlanAware ? ', VLAN-aware' : ''}
                </option>
              ))}
            </NativeSelect>
          </div>
        ))}
      </div>
      <div className="grid gap-2">
        <Label htmlFor="test-bridge">Test failover bridge</Label>
        <p className="text-xs text-muted-foreground">
          Test failovers start copies of the guests on this bridge instead, so it should have no physical ports: the test copies can't
          reach your network or clash with the guests still running on the primary.
        </p>
        <NativeSelect id="test-bridge" value={spec.testBridge} onChange={(e) => update((s) => (s.testBridge = e.target.value))}>
          <option value="">None</option>
          {bridges.map((b) => (
            <option key={b.name} value={b.name}>
              {b.name}
              {b.bridgePorts.length === 0 ? ' (no ports)' : ` (${b.bridgePorts.join(', ')})`}
            </option>
          ))}
        </NativeSelect>
      </div>
    </Section>
  )
}

// exampleDisk returns the dataset of a protected guest's disk on a storage,
// to show where replicas land.
function exampleDisk(spec: PlanSpec, inv: Inventory, storage: string): string | undefined {
  const vmids = new Set(spec.guests.map((g) => g.vmid))
  for (const g of inv.guests) {
    if (!vmids.has(g.vmid)) continue
    const d = g.disks.find((x) => x.storage === storage && x.zfsDataset)
    if (d) return d.zfsDataset
  }
  return undefined
}

function NetworkCard({ spec, update }: { spec: PlanSpec; update: Update }) {
  const path = spec.network?.path
  const existing = path?.case === 'existing' ? path.value : undefined
  const tunnel = path?.case === 'tunnel' ? path.value : undefined
  const setTunnel = (fn: (t: EzdrTunnel) => void) =>
    update((s) => {
      if (s.network?.path.case === 'tunnel') fn(s.network.path.value)
    })
  return (
    <Section
      id="network"
      description="How the DR host reaches the primary's zrepl jobs. zrepl connections always use TLS with each host's own certificate."
      summary={
        existing
          ? `Existing network · ${existing.primaryAddress || 'address not set'}:${existing.port || '?'}`
          : tunnel
            ? `EZDR tunnel · the ${tunnel.listener === EzdrTunnel_Listener.PRIMARY ? 'primary' : 'DR host'} listens on ${tunnel.endpoint || 'an endpoint not set'}`
            : 'Not chosen'
      }
    >
      <div className="grid gap-2 text-sm">
        <label className="flex items-center gap-2">
          <input
            type="radio"
            name="network"
            checked={!!existing}
            onChange={() =>
              update((s) => {
                s.network = create(ReplicationNetworkSchema, {
                  path: { case: 'existing', value: create(ExistingNetworkSchema, { port: 8888 }) },
                })
              })
            }
          />
          Existing network (for example, a router site-to-site VPN)
        </label>
        <label className="flex items-center gap-2">
          <input
            type="radio"
            name="network"
            checked={!!tunnel}
            onChange={() =>
              update((s) => {
                s.network = create(ReplicationNetworkSchema, {
                  path: {
                    case: 'tunnel',
                    value: create(EzdrTunnelSchema, { listener: EzdrTunnel_Listener.DR, listenPort: 51821, port: 8888 }),
                  },
                })
              })
            }
          />
          EZDR tunnel: a WireGuard tunnel between the two hosts
        </label>
      </div>
      {existing && (
        <div className="grid gap-4 sm:grid-cols-[1fr_140px]">
          <Field id="primary-address" label="Primary's address, as seen from the DR host">
            <Input
              id="primary-address"
              value={existing.primaryAddress}
              placeholder="192.0.2.10 or primary.example.com"
              onChange={(e) =>
                update((s) => {
                  if (s.network?.path.case === 'existing') s.network.path.value.primaryAddress = e.target.value
                })
              }
            />
          </Field>
          <Field id="zrepl-port" label="zrepl port">
            <Input
              id="zrepl-port"
              type="number"
              min={1024}
              max={65535}
              value={existing.port || ''}
              onChange={(e) =>
                update((s) => {
                  if (s.network?.path.case === 'existing') s.network.path.value.port = Math.round(Number(e.target.value))
                })
              }
            />
          </Field>
          <Field id="listen-address" label="Primary listens on (optional)">
            <Input
              id="listen-address"
              value={existing.listenAddress}
              placeholder="All addresses"
              onChange={(e) =>
                update((s) => {
                  if (s.network?.path.case === 'existing') s.network.path.value.listenAddress = e.target.value.trim()
                })
              }
            />
          </Field>
        </div>
      )}
      {tunnel && (
        <div className="grid gap-4">
          <div className="grid gap-2 text-sm">
            <Label>Which host accepts the connection?</Label>
            <div className="flex gap-4">
              {[
                { v: EzdrTunnel_Listener.DR, label: 'DR host' },
                { v: EzdrTunnel_Listener.PRIMARY, label: 'Primary' },
              ].map((o) => (
                <label key={o.v} className="flex items-center gap-2">
                  <input type="radio" name="listener" checked={tunnel.listener === o.v} onChange={() => setTunnel((t) => (t.listener = o.v))} />
                  {o.label}
                </label>
              ))}
            </div>
            <p className="text-xs text-muted-foreground">
              That host's site forwards the UDP listen port to it; the other host connects out. Replication then flows inside the tunnel.
            </p>
          </div>
          <div className="grid gap-4 sm:grid-cols-[1fr_140px_140px]">
            <Field id="tunnel-endpoint" label="Listening host's public endpoint (host:port)">
              <Input
                id="tunnel-endpoint"
                value={tunnel.endpoint}
                placeholder="dr.example.com:51821"
                onChange={(e) => setTunnel((t) => (t.endpoint = e.target.value))}
              />
            </Field>
            <Field id="tunnel-port" label="Listen port (UDP)">
              <Input
                id="tunnel-port"
                type="number"
                min={1}
                max={65535}
                value={tunnel.listenPort || ''}
                onChange={(e) => setTunnel((t) => (t.listenPort = Math.round(Number(e.target.value))))}
              />
            </Field>
            <Field id="tunnel-zrepl-port" label="zrepl port">
              <Input
                id="tunnel-zrepl-port"
                type="number"
                min={1024}
                max={65535}
                value={tunnel.port || ''}
                onChange={(e) => setTunnel((t) => (t.port = Math.round(Number(e.target.value))))}
              />
            </Field>
          </div>
        </div>
      )}
    </Section>
  )
}

function ScheduleCard({ spec, update }: { spec: PlanSpec; update: Update }) {
  const minutes = Math.round(spec.intervalSeconds / 60)
  return (
    <Section
      id="schedule"
      summary={`Snapshots ${describeInterval(spec.intervalSeconds || 60)} · alert after ${formatDuration(spec.rpoAlertSeconds || spec.intervalSeconds * 3)} · DR keeps ${grid(spec.drRetention) || 'nothing'} · primary keeps ${grid(spec.primaryRetention) || 'nothing'}`}
    >
      <div className="grid gap-2">
        <Label htmlFor="interval">Snapshot interval (minutes)</Label>
        <div className="flex flex-wrap items-center gap-2">
          <Input
            id="interval"
            type="number"
            min={1}
            max={1440}
            className="w-24"
            value={minutes || ''}
            onChange={(e) => update((s) => (s.intervalSeconds = Math.max(0, Math.round(Number(e.target.value) * 60))))}
          />
          {[5, 15, 60].map((m) => (
            <Button key={m} variant={minutes === m ? 'default' : 'outline'} size="sm" onClick={() => update((s) => (s.intervalSeconds = m * 60))}>
              {m === 60 ? '1 hour' : `${m} min`}
            </Button>
          ))}
        </div>
      </div>
      <div className="grid gap-2">
        <Label htmlFor="rpo-alert">RPO alert threshold (minutes)</Label>
        <div className="flex items-center gap-2">
          <Input
            id="rpo-alert"
            type="number"
            min={1}
            className="w-24"
            placeholder={String(Math.round((spec.intervalSeconds * 3) / 60))}
            value={spec.rpoAlertSeconds ? Math.round(spec.rpoAlertSeconds / 60) : ''}
            onChange={(e) => update((s) => (s.rpoAlertSeconds = Math.max(0, Math.round(Number(e.target.value) * 60))))}
          />
          <span className="text-xs text-muted-foreground">
            Alert when the newest replicated snapshot is older than this. Empty means 3× the snapshot interval.
          </span>
        </div>
      </div>
      <RetentionEditor
        label="Kept on the DR host"
        tiers={spec.drRetention}
        presets={drPresets}
        onChange={(tiers) => update((s) => (s.drRetention = tiers))}
      />
      <RetentionEditor
        label="Kept on the primary"
        tiers={spec.primaryRetention}
        presets={primaryPresets}
        onChange={(tiers) => update((s) => (s.primaryRetention = tiers))}
      />
      <p className="text-xs text-muted-foreground">
        Retention applies only to this plan's snapshots. Other snapshots, and snapshots not yet replicated, are never pruned.
      </p>
    </Section>
  )
}

function RetentionEditor({
  label,
  tiers,
  presets,
  onChange,
}: {
  label: string
  tiers: RetentionTier[]
  presets: { label: string; tiers: () => RetentionTier[] }[]
  onChange: (t: RetentionTier[]) => void
}) {
  const edit = (i: number, fn: (t: RetentionTier) => void) => {
    const next = tiers.map((t) => clone(RetentionTierSchema, t))
    fn(next[i])
    onChange(next)
  }
  return (
    <div className="grid gap-2 rounded-lg border p-3">
      <div className="flex flex-wrap items-center gap-2">
        <Label className="mr-2">{label}</Label>
        {presets.map((p) => (
          <Button key={p.label} variant="outline" size="xs" onClick={() => onChange(p.tiers())}>
            {p.label}
          </Button>
        ))}
      </div>
      {tiers.map((t, i) => {
        const { value, unit } = splitPeriod(t.periodSeconds)
        return (
          <div key={i} className="flex flex-wrap items-center gap-2 text-sm">
            {t.keepAll ? (
              <span>Every snapshot for</span>
            ) : (
              <>
                <span>One per</span>
              </>
            )}
            <Input
              type="number"
              min={1}
              className="w-20"
              aria-label="Period"
              value={value || ''}
              onChange={(e) => edit(i, (x) => (x.periodSeconds = Math.round(Number(e.target.value) * unit)))}
            />
            <NativeSelect
              className="w-28"
              aria-label="Unit"
              value={unit}
              onChange={(e) => edit(i, (x) => (x.periodSeconds = value * Number(e.target.value)))}
            >
              {units.map((u) => (
                <option key={u.seconds} value={u.seconds}>
                  {u.label}
                </option>
              ))}
            </NativeSelect>
            {!t.keepAll && (
              <>
                <span>for</span>
                <Input
                  type="number"
                  min={1}
                  className="w-20"
                  aria-label="Count"
                  value={t.count || ''}
                  onChange={(e) => edit(i, (x) => (x.count = Math.max(0, Math.round(Number(e.target.value)))))}
                />
                <span>{unitLabel(unit, t.count)}</span>
              </>
            )}
            <label className="flex items-center gap-1 text-xs text-muted-foreground">
              <input
                type="checkbox"
                checked={t.keepAll}
                onChange={(e) =>
                  edit(i, (x) => {
                    x.keepAll = e.target.checked
                    if (x.keepAll) x.count = 1
                  })
                }
              />
              keep all
            </label>
            <Button variant="ghost" size="icon-xs" aria-label="Remove tier" onClick={() => onChange(tiers.filter((_, j) => j !== i))}>
              <Trash2 />
            </Button>
          </div>
        )
      })}
      <div className="flex items-center justify-between gap-2">
        <Button
          variant="ghost"
          size="xs"
          onClick={() => onChange([...tiers, create(RetentionTierSchema, { count: 7, periodSeconds: 86400 })])}
        >
          <Plus /> Add tier
        </Button>
        <code className="text-xs text-muted-foreground">{grid(tiers)}</code>
      </div>
    </div>
  )
}

// unitLabel names a retention unit, singular for 1 ("1 hour", "24 hours").
function unitLabel(unitSeconds: number, n: number) {
  const label = units.find((u) => u.seconds === unitSeconds)?.label ?? 'periods'
  return n === 1 ? label.replace(/s$/, '') : label
}

function guestName(inv: Inventory, vmid: number) {
  return inv.guests.find((g) => g.vmid === vmid)?.name ?? `guest ${vmid}`
}

function StartupCard({ spec, inv, update }: { spec: PlanSpec; inv: Inventory; update: Update }) {
  const order = [...spec.guests].sort((a, b) => a.startupOrder - b.startupOrder || a.vmid - b.vmid)
  return (
    <Section
      id="startup"
      description="At failover, guests start in ascending order; equal orders start together."
      summary={order.map((g) => guestName(inv, g.vmid)).join(' → ')}
    >
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Guest</TableHead>
            <TableHead>Order</TableHead>
            <TableHead>Wait after start (seconds)</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {order.map((g) => {
            const i = spec.guests.findIndex((x) => x.vmid === g.vmid)
            return (
              <TableRow key={g.vmid}>
                <TableCell>
                  <span className="font-mono text-xs">{g.vmid}</span> {guestName(inv, g.vmid)}
                </TableCell>
                <TableCell>
                  <Input
                    type="number"
                    min={1}
                    className="w-20"
                    aria-label={`Startup order for ${g.vmid}`}
                    value={g.startupOrder || ''}
                    onChange={(e) => update((s) => (s.guests[i].startupOrder = Math.round(Number(e.target.value))))}
                  />
                </TableCell>
                <TableCell>
                  <Input
                    type="number"
                    min={0}
                    className="w-24"
                    aria-label={`Startup delay for ${g.vmid}`}
                    value={g.startupDelaySeconds}
                    onChange={(e) => update((s) => (s.guests[i].startupDelaySeconds = Math.max(0, Math.round(Number(e.target.value)))))}
                  />
                </TableCell>
              </TableRow>
            )
          })}
        </TableBody>
      </Table>
    </Section>
  )
}

const recordTypes = [
  { label: 'A', value: DnsRecordType.A },
  { label: 'AAAA', value: DnsRecordType.AAAA },
  { label: 'CNAME', value: DnsRecordType.CNAME },
  { label: 'TXT', value: DnsRecordType.TXT },
]

function DnsCard({ spec, inv, update }: { spec: PlanSpec; inv: Inventory; update: Update }) {
  return (
    <Section
      id="dns"
      description="Public records to switch at failover. When you confirm a failover or a failback, EZDR can switch them through the DNS provider set in Settings."
      summary={(() => {
        const n = spec.guests.reduce((sum, g) => sum + g.dnsRecords.length, 0)
        return n === 0 ? 'No records' : `${n} record${n === 1 ? '' : 's'}: ${spec.guests.flatMap((g) => g.dnsRecords.map((r) => r.name)).join(', ')}`
      })()}
    >
      {spec.guests.map((g, gi) => (
        <div key={g.vmid} className="grid gap-2 border-b pb-3 last:border-b-0">
          <div className="flex items-center justify-between">
            <span className="text-sm font-medium">
              <span className="font-mono text-xs">{g.vmid}</span> {guestName(inv, g.vmid)}
            </span>
            <Button
              variant="ghost"
              size="xs"
              onClick={() => update((s) => s.guests[gi].dnsRecords.push(create(DnsRecordSchema, { type: DnsRecordType.A })))}
            >
              <Plus /> Add record
            </Button>
          </div>
          {g.dnsRecords.map((r, ri) => (
            <div key={ri} className="grid items-center gap-2 sm:grid-cols-[1.5fr_90px_1fr_1fr_32px]">
              <Input
                aria-label="Record name"
                placeholder="app.example.com"
                value={r.name}
                onChange={(e) => update((s) => (s.guests[gi].dnsRecords[ri].name = e.target.value))}
              />
              <NativeSelect
                aria-label="Record type"
                value={r.type}
                onChange={(e) => update((s) => (s.guests[gi].dnsRecords[ri].type = Number(e.target.value)))}
              >
                {recordTypes.map((t) => (
                  <option key={t.value} value={t.value}>
                    {t.label}
                  </option>
                ))}
              </NativeSelect>
              <Input
                aria-label="Production value"
                placeholder="production value"
                value={r.productionValue}
                onChange={(e) => update((s) => (s.guests[gi].dnsRecords[ri].productionValue = e.target.value))}
              />
              <Input
                aria-label="Failover value"
                placeholder="failover value"
                value={r.failoverValue}
                onChange={(e) => update((s) => (s.guests[gi].dnsRecords[ri].failoverValue = e.target.value))}
              />
              <Button
                variant="ghost"
                size="icon-sm"
                aria-label="Remove record"
                onClick={() => update((s) => s.guests[gi].dnsRecords.splice(ri, 1))}
              >
                <Trash2 />
              </Button>
            </div>
          ))}
        </div>
      ))}
    </Section>
  )
}

function AdvancedCard({ spec, update }: { spec: PlanSpec; update: Update }) {
  return (
    <Section
      id="advanced"
      summary={`Prefix ${spec.snapshotPrefix || '?'} · test IDs +${spec.testVmidOffset || 10000} · test limit ${formatDuration(spec.testTimeLimitSeconds || 8 * 3600)} · shutdown timeout ${spec.shutdownTimeoutSeconds || 300}s`}
    >
      <Field id="prefix" label="Snapshot prefix">
        <Input
          id="prefix"
          className="w-48 font-mono"
          value={spec.snapshotPrefix}
          onChange={(e) => update((s) => (s.snapshotPrefix = e.target.value))}
        />
      </Field>
      <p className="-mt-2 text-xs text-muted-foreground">
        Snapshots this plan creates and prunes carry this prefix. To take over an existing zrepl setup, use Adopt instead of changing it by
        hand.
      </p>
      <div className="grid gap-4 sm:grid-cols-2">
        <Field id="test-offset" label="Test failover ID offset">
          <Input
            id="test-offset"
            type="number"
            min={1}
            className="w-48"
            placeholder="10000"
            value={spec.testVmidOffset || ''}
            onChange={(e) => update((s) => (s.testVmidOffset = Math.max(0, Math.round(Number(e.target.value)))))}
          />
        </Field>
        <Field id="test-limit" label="Test failover time limit (hours)">
          <Input
            id="test-limit"
            type="number"
            min={0.25}
            max={168}
            step={0.25}
            className="w-48"
            placeholder="8"
            value={spec.testTimeLimitSeconds ? spec.testTimeLimitSeconds / 3600 : ''}
            onChange={(e) => update((s) => (s.testTimeLimitSeconds = Math.max(0, Math.round(Number(e.target.value) * 3600))))}
          />
        </Field>
      </div>
      <Field id="shutdown-timeout" label="Failover shutdown timeout (seconds)">
        <Input
          id="shutdown-timeout"
          type="number"
          min={10}
          max={3600}
          className="w-48"
          placeholder="300"
          value={spec.shutdownTimeoutSeconds || ''}
          onChange={(e) => update((s) => (s.shutdownTimeoutSeconds = Math.max(0, Math.round(Number(e.target.value)))))}
        />
      </Field>
      <p className="-mt-2 text-xs text-muted-foreground">
        In a planned failover, guests on the primary that haven't shut down after this long are forced off.
      </p>
      <p className="-mt-2 text-xs text-muted-foreground">
        Test guests use their ID plus the offset (guest 201 becomes {201 + (spec.testVmidOffset || 10000)}). A test ends by itself after the
        time limit.
      </p>
    </Section>
  )
}

function ValidationPanel({ issues, onSelect }: { issues: Issue[]; onSelect: (id: SectionId) => void }) {
  const { errors } = counts(issues)
  const sorted = [...issues].sort((a, b) => a.severity - b.severity)
  return (
    <Card size="sm">
      <CardHeader>
        <CardTitle>Validation</CardTitle>
        <CardDescription>
          {errors === 0 ? 'No errors.' : `${errors} error${errors === 1 ? '' : 's'} to fix before activation.`}
        </CardDescription>
      </CardHeader>
      {sorted.length > 0 && (
        <CardContent className="grid gap-1 text-xs">
          {sorted.map((i, n) => {
            const Icon = i.severity === Severity.ERROR ? CircleAlert : TriangleAlert
            return (
              <button
                key={n}
                type="button"
                onClick={() => i.section && onSelect(i.section as SectionId)}
                className={`flex gap-1.5 rounded px-1 py-0.5 text-left hover:bg-muted ${
                  i.severity === Severity.ERROR ? 'text-destructive' : 'text-warning-foreground'
                }`}
              >
                <Icon className="mt-0.5 size-3.5 shrink-0" /> {i.message}
              </button>
            )
          })}
        </CardContent>
      )}
    </Card>
  )
}
