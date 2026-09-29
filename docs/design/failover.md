# Design: Failover

> **Status:** Approved for phase 6. Covers failing a plan over to its DR host
> (planned and unplanned), preventing split-brain, switching public DNS
> records through Cloudflare, detecting DNS drift, and break-glass failover
> from the DR host's command line when the portal is down. Failback is
> phase 7. See [Architecture](../architecture.md), [Replication](replication.md),
> and [Test failover](test-failover.md).

## 1. Summary

- **Failover** makes the DR host run a plan's guests from their replicas
  **in place**: EZDR adds a Proxmox storage over each receive dataset, so the
  replicas become the guests' disks with their original names and VMIDs. No
  data is copied, and the replicas keep their zrepl snapshots for failback.
- A **planned** failover (primary reachable) shuts the primary's guests down,
  forcing them off after a timeout, replicates their final state, and then
  starts them on the DR host. An **unplanned** failover uses the newest
  replicated snapshots.
- The primary's copies are **stopped and locked** so they can't start by
  accident: at once in a planned failover, and when the primary reconnects
  after an unplanned one.
- After the guests start, the operator **verifies** them, then confirms the
  **DNS switch**: each record is set to its failover value through Cloudflare
  and read back.
- The portal **checks DNS records** regularly and alerts when they don't
  match the plan's state (drift).
- **Break-glass:** `ezdr failover --plan <name>` on the DR host runs an
  unplanned failover without the portal and prints the DNS changes to make
  by hand. The portal records it when the DR host reconnects.

## 2. Plan states

| State | Meaning |
| --- | --- |
| Active, Paused | As before (phase 4). Either can fail over. |
| **Failing over** | The failover is running. Plan edits are refused. |
| **Failed over** | The guests run on the DR host. Replication for the plan is stopped, and the primary's copies are locked. Failback (phase 7) returns the plan to active. |

A running test failover is ended first: its clones depend on the replicas'
snapshots.

## 3. Replicas as the guests' disks

zrepl receives each source dataset as `<receive dataset>/<source dataset>`,
so every source storage's datasets sit together under
`<receive dataset>/<source storage's dataset>` (for example,
`tank-dr/ezdr/pve1/rpool/data/vm-201-disk-0`). For each source storage the
plan uses, failover adds a Proxmox ZFS storage over that dataset, named
`ezdr-<plan>-<source storage>` and restricted to the DR host. Proxmox then
lists the replicas under their original volume names and VMIDs (verified in
the lab).

Before registering guests, the DR client:

- aborts any partially received stream (`zfs receive -A`), so each replica
  is exactly its newest snapshot;
- makes the replicas writable (`readonly=off`);
- sets each container volume's `refquota` from the configuration's `size`:
  zrepl doesn't send properties, so container disk sizes aren't replicated
  (found in the lab).

The replicas keep their snapshots, bookmarks, and zrepl holds, so failback
can replicate the changes back incrementally.

## 4. Guest configurations

Configurations come from the DR host's stored copies (see
[Test failover](test-failover.md), section 2) and are rewritten with the
same code as for tests, with these differences:

| Setting | Failover |
| --- | --- |
| VMID | Unchanged |
| Disks | The same volume on the plan's `ezdr-` storage (such as `ezdr-abcd1234-local-zfs:vm-201-disk-0`) |
| NICs | The bridge from the plan's network mapping; VLAN tag and MAC address kept |
| Cloud-init drive, passthrough, ISO media, `vmgenid` | As for tests: a fresh drive, removed, removed if the storage is missing, a new generation ID |
| `onboot` | Kept, so the guests come back after the DR host restarts |
| Name, hostname | Kept |
| Tags | `ezdr-failover` added |
| Description | Notes the plan, the primary, and when the failover happened |

## 5. Failover steps

The portal runs these as a persisted, resumable workflow, like takeovers and
tests.

| Step | Planned | Unplanned |
| --- | --- | --- |
| 1. End a running test failover | yes | yes |
| 2. Shut down the guests on the primary, in reverse startup order; force off after the shutdown timeout | yes | — |
| 3. Take a final snapshot of every plan dataset on the primary (the plan's prefix and zrepl's name format), trigger the DR host's pull job (`zrepl signal wakeup`), and wait until the DR host has it for every dataset | yes | — |
| 4. Stop replication: the plan's jobs leave both hosts' desired state | yes | DR host only |
| 5. Lock the primary's copies: `onboot` off, `lock: migrate` (Proxmox refuses to start a locked guest), and the tag `ezdr-failed-over` | yes | when the primary reconnects |
| 6. Prepare the replicas and add the storages (section 3) | yes | yes |
| 7. Register the guests (section 4) | yes | yes |
| 8. Start them in startup order with their delays, and check them (running, and the guest agent when enabled) | yes | yes |
| 9. Wait for the operator to verify the guests, then switch DNS on confirmation | yes | yes |

- **Planned or unplanned:** the dialog offers a planned failover when the
  primary is online and an unplanned one otherwise; the operator can choose
  unplanned even if the primary is online (for example, if it's
  misbehaving).
- **Failures:** before step 4, a failed planned failover restarts the
  primary's guests and leaves the plan active. From step 4 on the plan is
  committed: failures are reported per step and the step can be retried.
  Guests that fail to start are reported, and the others continue.
- **Shutdown timeout:** a plan setting, default 5 minutes.
- **Confirmation:** starting a failover requires typing the plan's name. The
  dialog shows the newest replicated snapshot's age (the data that would be
  lost in an unplanned failover), the guests, and the DNS records to switch.

### 5.1 The returning primary

While a plan is failed over, the primary's desired state lists the plan's
guests as **locked**. Its client stops any that run (they may have kept
running if the primary was cut off rather than down) and applies the lock,
and it reports what it did. The portal alerts when it had to stop a guest.
Unlocking happens during failback.

## 6. DNS

### 6.1 Cloudflare

- **Settings:** a Cloudflare API token with "Zone: DNS: Edit" on the zones
  the plans use, stored encrypted like the other secrets. It's never sent to
  hosts. A test button lists the zones it can see.
- Each record's zone is the longest zone name that the record's name ends
  with. Records are identified by name and type.
- The plan page's DNS card shows each record as last checked, and warns
  when a record's zone isn't visible to the token, when the record doesn't
  exist, or when a DNS-only record's TTL is above 300 seconds (proxied
  records take effect almost immediately). These come from the regular
  checks (6.3) rather than validation, so validating a plan never waits on
  Cloudflare.

### 6.2 Switching

After the operator confirms (step 9), each record is set to its failover
value, then read back. The plan page shows each record's result; failed
records can be retried. The operator can also skip the switch (for example,
to switch by hand).

### 6.3 Drift detection

Every 5 minutes the portal reads each plan's records and compares them with
the values expected for the plan's state: production values while active or
paused, failover values once failed over and switched. A mismatch raises an
alert (for example, a manual edit, or a switch that only partly applied) and
shows on the plan page.

## 7. Break-glass failover

When the portal is unreachable, an operator with root on the DR host runs:

```
ezdr failover --plan <name>
```

- The DR host keeps what it needs locally: the guest configurations
  (phase 5), and, from its desired state, each plan's **recovery
  information**: receive datasets and source storages, network mappings,
  startup order and delays, and DNS records (under
  `/var/lib/ezdr/plans/<plan>/`).
- The CLI shows the plan, the newest local snapshot's age, and the guests,
  asks for the plan's name to confirm, then runs the unplanned failover's DR
  steps locally: remove the plan's pull job, prepare the replicas, register,
  start, and check. It prints the DNS records and values to change by hand.
- It records the failover locally. While that record exists, the DR client
  ignores the plan's jobs in its desired state, so the portal can't
  re-enable replication into the now-writable replicas.
- When the DR host reconnects, it reports the break-glass failover. The
  portal records it as awaiting confirmation, writes the audit record (with
  the local time and user), raises a critical alert, and runs the DNS drift
  check, which shows whether the records were changed. An administrator
  verifies the guests and confirms, optionally switching DNS: only then is
  the plan failed over and the primary locked (5.1). A report alone can't
  stop the primary's guests, so a compromised DR host can't use one to take
  production down; until the confirmation, the primary's guests could start
  too if it comes back.

## 8. Security

- New client actions are fixed and validated like the test actions. The DR
  host only makes writable and registers datasets under the plan's receive
  datasets; the primary only stops and locks the plan's guests.
- Only the portal holds the Cloudflare token.
- Failover requires typing the plan's name, in the portal and on the command
  line, and is recorded in the audit log.

## 9. Out of scope

Failback and reversing replication (phase 7); re-addressing guests; DNS
providers other than Cloudflare.
