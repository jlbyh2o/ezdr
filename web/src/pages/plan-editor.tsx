import { clone, create } from '@bufbuild/protobuf'
import { ArrowLeft, CircleAlert, Plus, Trash2, TriangleAlert } from 'lucide-react'
import { useEffect, useRef, useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router'

import { ErrorAlert } from '@/components/error-alert'
import { NativeSelect } from '@/components/native-select'
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
import { type GetHostInventoryResponse, type Host, type Plan, PlanState, type ZreplSetup } from '@/gen/ezdr/portal/v1/portal_pb'
import { HostStatusPanel, PlanActions, StateBadge } from '@/pages/plan-actions'
import { PlanStatusCard } from '@/pages/plan-status'
import { TestFailoverCard } from '@/pages/test-failover'
import { errorMessage, hostClient, planClient } from '@/lib/api'
import { drPresets, grid, primaryPresets, splitPeriod, units } from '@/lib/retention'
import { PageHeader } from '@/pages/layout'
import { Field } from '@/pages/setup'

type Update = (fn: (s: PlanSpec) => void) => void

export function PlanEditorPage() {
  const { id } = useParams()
  const isNew = !id
  const navigate = useNavigate()
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
  }, [spec, id])

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
        navigate(`/plans/${r.plan?.id}`, { replace: true })
      } else {
        const r = await planClient.updatePlan({ id, spec })
        setIssues(r.issues)
        setPlan(r.plan)
        setSavedAt(new Date())
      }
      setDirty(false)
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setSaving(false)
    }
  }

  async function remove() {
    try {
      await planClient.deletePlan({ id })
      navigate('/plans')
    } catch (err) {
      setError(errorMessage(err))
    }
  }

  if (!spec) return <ErrorAlert message={error} />
  const primaryInv = primaryId ? primary?.inventory : undefined
  const drInv = drId ? dr?.inventory : undefined

  return (
    <>
      <Link to="/plans" className="flex items-center gap-1 pt-4 text-sm text-muted-foreground hover:text-foreground">
        <ArrowLeft className="size-4" /> DR plans
      </Link>
      <PageHeader
        title={isNew ? 'New DR plan' : spec.name || 'DR plan'}
        description={
          !plan || plan.state === PlanState.DRAFT
            ? 'Drafts are saved without changing the hosts. Activate the plan to start replication.'
            : 'Edits to an active plan are saved as pending changes; replication keeps its applied settings until you apply them.'
        }
      >
        <div className="flex items-center gap-2">
          {plan && <StateBadge state={plan.state} pending={plan.pendingChanges} />}
          {savedAt && <span className="text-xs text-muted-foreground">Saved {savedAt.toLocaleTimeString()}</span>}
          {plan?.state === PlanState.DRAFT && <DeletePlanButton name={spec.name} onDelete={() => void remove()} />}
          <Button variant={plan ? 'outline' : 'default'} onClick={() => void save()} disabled={saving}>
            {isNew ? 'Create plan' : 'Save'}
          </Button>
        </div>
      </PageHeader>
      {plan && (
        <PlanActions
          plan={plan}
          dirty={dirty}
          onChanged={(p) => {
            setPlan(p)
            if (p.spec) setSpec(p.spec)
          }}
        />
      )}
      <ErrorAlert message={error} />
      <div className="grid items-start gap-4 lg:grid-cols-[1fr_320px]">
        <div className="grid gap-4">
          {plan && <PlanStatusCard plan={plan} />}
          {plan && <TestFailoverCard plan={plan} />}
          <GeneralCard spec={spec} hosts={hosts} update={update} updateAndSuggest={updateAndSuggest} />
          {primaryInv && drInv && (!plan || plan.state === PlanState.DRAFT) && (
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
            <GuestsCard spec={spec} inv={primaryInv} guestPlans={primary?.guestPlans ?? {}} planId={id} updateAndSuggest={updateAndSuggest} />
          )}
          {primaryInv && drInv && spec.guests.length > 0 && (
            <MappingsCard spec={spec} primaryInv={primaryInv} drInv={drInv} update={update} updateAndSuggest={updateAndSuggest} />
          )}
          {primaryInv && <NetworkCard spec={spec} update={update} />}
          <ScheduleCard spec={spec} update={update} />
          {primaryInv && spec.guests.length > 0 && <StartupCard spec={spec} inv={primaryInv} update={update} />}
          {primaryInv && spec.guests.length > 0 && <DnsCard spec={spec} inv={primaryInv} update={update} />}
          <AdvancedCard spec={spec} update={update} />
        </div>
        <div className="grid gap-4 lg:sticky lg:top-4">
          {plan && <HostStatusPanel plan={plan} />}
          <ValidationPanel issues={issues} />
        </div>
      </div>
    </>
  )
}

function DeletePlanButton({ name, onDelete }: { name: string; onDelete: () => void }) {
  return (
    <AlertDialog>
      <AlertDialogTrigger render={<Button variant="ghost" />}>
        <Trash2 /> Delete
      </AlertDialogTrigger>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Delete plan {name}?</AlertDialogTitle>
          <AlertDialogDescription>The plan's settings are removed. Its guests are no longer protected by it.</AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction variant="destructive" onClick={onDelete}>
            Delete plan
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}

function Section({ title, description, children }: { title: string; description?: string; children: React.ReactNode }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>{title}</CardTitle>
        {description && <CardDescription>{description}</CardDescription>}
      </CardHeader>
      <CardContent className="grid gap-4">{children}</CardContent>
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
  return (
    <Section title="General">
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
      title="Existing zrepl setup"
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
  planId,
  updateAndSuggest,
}: {
  spec: PlanSpec
  inv: Inventory
  guestPlans: GetHostInventoryResponse['guestPlans']
  planId?: string
  updateAndSuggest: (fn: (s: PlanSpec) => void) => Promise<void>
}) {
  const selected = new Set(spec.guests.map((g) => g.vmid))
  const otherPlan = (vmid: number) => {
    const p = guestPlans[vmid]
    return p && p.id !== planId ? p : undefined
  }
  const selectable = inv.guests.filter((g) => !g.template && !otherPlan(g.vmid))

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
    <Section title="Guests" description={`${spec.guests.length} of ${inv.guests.length} guests on ${inv.host?.hostname} selected.`}>
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
                    disabled={!!other || g.template}
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
                    <Badge>Ready</Badge>
                  ) : (
                    <Badge variant="destructive">Not ready</Badge>
                  )}
                </TableCell>
              </TableRow>
            )
          })}
        </TableBody>
      </Table>
    </Section>
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
  const zfsStorages = drInv.storages.filter((s) => s.type === 'zfspool')
  const bridges = drInv.interfaces.filter((i) => i.type === 'bridge')
  const primaryName = primaryInv.host?.hostname ?? 'primary'
  return (
    <Section title="Mappings" description="Where protected guests' disks and networks go on the DR host.">
      <div className="grid gap-2">
        <Label>Storage</Label>
        {spec.storageMappings.map((m, i) => (
          <div key={m.sourceStorage} className="grid items-center gap-2 sm:grid-cols-[140px_24px_1fr_1.5fr]">
            <span className="font-mono text-sm">{m.sourceStorage}</span>
            <span className="text-muted-foreground">→</span>
            <NativeSelect
              aria-label={`DR storage for ${m.sourceStorage}`}
              value={m.targetStorage}
              onChange={(e) =>
                update((s) => {
                  s.storageMappings[i].targetStorage = e.target.value
                  const pool = zfsStorages.find((z) => z.id === e.target.value)?.zfsPool
                  if (pool) s.storageMappings[i].receiveDataset = `${pool}/ezdr/${primaryName}`
                })
              }
            >
              <option value="">Choose DR storage…</option>
              {zfsStorages.map((z) => (
                <option key={z.id} value={z.id}>
                  {z.id}
                </option>
              ))}
            </NativeSelect>
            <Input
              aria-label={`Receive dataset for ${m.sourceStorage}`}
              value={m.receiveDataset}
              onChange={(e) => update((s) => (s.storageMappings[i].receiveDataset = e.target.value))}
              placeholder="receive dataset"
              className="font-mono text-xs"
            />
          </div>
        ))}
        <p className="text-xs text-muted-foreground">
          Replicas are received as &lt;receive dataset&gt;/&lt;source dataset&gt;.
        </p>
      </div>
      <div className="grid gap-2">
        <Label>Networks (VLAN tags are kept)</Label>
        {spec.networkMappings.map((m, i) => (
          <div key={m.sourceBridge} className="grid items-center gap-2 sm:grid-cols-[140px_24px_1fr]">
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
      <Field id="test-bridge" label="Test failover bridge (isolated, no physical ports)">
        <NativeSelect id="test-bridge" value={spec.testBridge} onChange={(e) => update((s) => (s.testBridge = e.target.value))}>
          <option value="">None</option>
          {bridges.map((b) => (
            <option key={b.name} value={b.name}>
              {b.name}
              {b.bridgePorts.length === 0 ? ' (no ports)' : ` (${b.bridgePorts.join(', ')})`}
            </option>
          ))}
        </NativeSelect>
      </Field>
    </Section>
  )
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
    <Section title="Replication network" description="How the DR host reaches the primary's zrepl jobs. zrepl connections always use TLS with each host's own certificate.">
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
    <Section title="Schedule and retention">
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
                <span>periods</span>
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

function guestName(inv: Inventory, vmid: number) {
  return inv.guests.find((g) => g.vmid === vmid)?.name ?? `guest ${vmid}`
}

function StartupCard({ spec, inv, update }: { spec: PlanSpec; inv: Inventory; update: Update }) {
  const order = [...spec.guests].sort((a, b) => a.startupOrder - b.startupOrder || a.vmid - b.vmid)
  return (
    <Section title="Startup order" description="At failover, guests start in ascending order; equal orders start together.">
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
    <Section title="DNS records" description="Public records to switch at failover. Switching them automatically comes with failover (phase 6).">
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
    <Section title="Advanced">
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

function ValidationPanel({ issues }: { issues: Issue[] }) {
  const errors = issues.filter((i) => i.severity === Severity.ERROR)
  const warnings = issues.filter((i) => i.severity === Severity.WARNING)
  return (
    <Card>
      <CardHeader>
        <CardTitle>Validation</CardTitle>
        <CardDescription>
          {errors.length === 0 ? 'No errors.' : `${errors.length} error${errors.length === 1 ? '' : 's'} to fix before activation.`}
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-2 text-sm">
        {errors.map((i, n) => (
          <div key={`e${n}`} className="flex gap-2 text-destructive">
            <CircleAlert className="mt-0.5 size-4 shrink-0" /> {i.message}
          </div>
        ))}
        {warnings.map((i, n) => (
          <div key={`w${n}`} className="flex gap-2 text-amber-700">
            <TriangleAlert className="mt-0.5 size-4 shrink-0" /> {i.message}
          </div>
        ))}
      </CardContent>
    </Card>
  )
}
