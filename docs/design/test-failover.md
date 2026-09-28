# Design: Test failover

> **Status:** Draft for phase 5. Covers keeping protected guests'
> configuration on the DR host, and test failovers: starting copies of
> protected guests on the DR host from replicated snapshots, on an isolated
> network, without interrupting replication. See [Architecture](../architecture.md),
> [DR plans](dr-plans.md), and [Replication](replication.md).

## 1. Summary

- The primary's client reports each guest's **Proxmox configuration**. The
  portal passes the protected guests' configurations to the DR host, which
  keeps the latest copy locally, so failover (phase 6) can work without the
  portal.
- A **test failover** clones replicated snapshots into a dedicated Proxmox
  storage on the DR host (`ezdr-test-<pool>`), registers test guests from the
  stored configurations with the plan's mappings applied, attaches every NIC to
  the plan's isolated **test bridge**, and starts them in the plan's startup
  order.
- Test guests use **offset VMIDs** (default +10000, so VM 201 becomes 10201),
  are tagged `ezdr-test`, and VMs are named `test-<name>`. Real VMIDs stay free.
- A test starts from the **newest replicated snapshot** by default, or from any
  earlier point that all selected guests still have.
- **Replication continues** during a test.
- The operator **ends** the test; it also ends automatically after a **time
  limit** (default 8 hours, extendable). Ending stops and removes the test
  guests and their clones.
- Every test is kept in a **history** with each guest's result and the
  operator's **verdict** (passed or failed, with notes).
- Test failover never touches production DNS, the primary, or replicas.

## 2. Guest configuration on the DR host

### 2.1 Collecting

The inventory deliberately reads only listed fields, so notes and cloud-init
credentials never leave a host ([Inventory](inventory.md)). Configurations
therefore travel separately, and only for guests that need them:

- The primary's desired state lists the VMIDs protected by its active and
  paused plans.
- The primary's client reads those guests' configuration files from
  Proxmox's cluster file system (`/etc/pve/qemu-server/<vmid>.conf` and
  `/etc/pve/lxc/<vmid>.conf`), keeping only the current configuration:
  Proxmox snapshot sections and pending changes are dropped.
- It reports them to the portal when they change, and at least every 15
  minutes.

### 2.2 Relaying and storing

- The portal stores the latest configuration per guest and adds the
  configurations of each active or paused plan's guests to the DR host's
  desired state, with the time the primary reported them.
- The DR client stores them under `/var/lib/ezdr/plans/<plan>/guests/`
  (`<vmid>.conf` plus metadata: guest type, primary host name, reported
  time). Only the latest configuration is kept.
- Pausing a plan keeps them. Deactivating or deleting a plan, or removing a
  guest from it, removes them from the DR host (they come back on
  activation).
- The plan's status shows how old the DR host's copy of each configuration
  is.

Configurations can contain secrets that Proxmox stores in them, such as
cloud-init passwords (hashed) and SSH keys. They travel only over the
WireGuard tunnel and are stored on the portal and the DR host, both of which
already hold the plan.

## 3. Rewriting a configuration

The DR client turns a stored configuration into a test guest's configuration.
The same rewriting is used for failover (phase 6), so it lives in the client
and doesn't need the portal.

| Setting | Test failover |
| --- | --- |
| VMID | Original plus the plan's test VMID offset |
| Disks (replicated) | Clones on the test storage, named for the test VMID (`vm-10201-disk-0`, `subvol-10101-disk-0`); options such as `size` and `discard` are kept |
| Cloud-init drive | Not replicated; a fresh one is created on the test storage (`qm set --ide2 <storage>:cloudinit`). Proxmox doesn't create a missing cloud-init drive at start: it waits for it forever (found in the lab). |
| ISO media on storage the DR host doesn't have | Replaced by no media |
| NICs | Bridge replaced by the test bridge; VLAN tag and MAC address kept |
| PCI and USB passthrough, `lock`, `parent`, `unused*` | Removed (passthrough is reported) |
| `vmgenid` | A new UUID, written by EZDR (Proxmox only generates one for `vmgenid: 1` through `qm set`, not in the file) |
| `onboot` | Off |
| VM name | `test-<name>`; a container's hostname is kept, since it's the guest's own hostname |
| Tags | `ezdr-test` added |
| Description | Notes which plan and test the guest belongs to |

Everything else, including the SMBIOS UUID, is kept, so guests see the same
hardware.

## 4. Test storage and clones

- On first use, the DR client creates a dataset `<pool>/ezdr-test` on each
  pool that holds the plan's replicas, and a Proxmox ZFS storage
  `ezdr-test-<pool>` for it (content: disk images and container volumes,
  restricted to this node). It's kept between tests.
- ZFS clones must be in the same pool as their snapshot, which the receive
  datasets already are (plan validation requires the receive dataset to be on
  the target storage's pool).
- Each replicated disk is cloned from the chosen snapshot:
  `zfs clone <replica>@<snapshot> <pool>/ezdr-test/<volume>`. Clones take no
  space until the test guest writes.
- Clones don't inherit the replicas' `readonly=on` (they inherit from their
  parent dataset), so test guests can write.

### 4.1 Replication and pruning during a test

- zrepl keeps receiving into the replicas: an incremental receive isn't
  blocked by an older snapshot having clones (verified in the lab, including
  from the cloned snapshot itself).
- zrepl can't destroy a snapshot that has clones, so pruning that snapshot
  fails until the test ends ("snapshot has dependent clones"; verified in the
  lab, where it turned plan health to failing). Plan health and alerts
  ignore a pruning error when every failure in it is of that kind and every
  clone it names is on a test storage. zrepl removes the snapshots on its
  first pruning run after the test.

## 5. Running a test

### 5.1 Starting

The **Test failover** dialog on an active plan shows:

- the guests (all selected by default), in startup order;
- the point in time: newest by default, or any earlier snapshot all selected
  guests' datasets have (snapshots of one plan are taken together, so their
  names match);
- the DR host's free memory against the selected guests' memory, with a
  warning if it's short (the test can still start);
- the guests' configurations' age, and guests that can't be tested (for
  example, no stored configuration yet).

### 5.2 Steps

After confirmation, the portal runs these steps, recorded like a takeover
(each step can be repeated, and a portal restart resumes):

1. **Prepare** (DR host): create the test storage if needed, clone every
   selected guest's disks from the chosen snapshot, and register the test
   guests from their rewritten configurations. Nothing starts yet.
2. **Start** the guests in startup order, waiting each guest's delay before
   starting the next. A guest that fails to start is recorded and the rest
   continue.
3. **Check** each guest: running in Proxmox, and, for VMs whose
   configuration enables the QEMU guest agent, answering an agent ping within
   5 minutes.

The test is then **running** until it ends.

### 5.3 While running

- The plan page shows the test guests, their results, the time left, and
  **Extend** (adds the limit again) and **End test**.
- The operator can open the guests' consoles in Proxmox. Test guests are on
  the isolated bridge, so they only reach each other.

### 5.4 Ending

**End test**, or the time limit, runs the cleanup: stop and destroy the test
guests (with their disks on the test storage, which are the clones). If the
DR host is offline, the test stays "ending" and cleanup runs when it's back.

Cleanup only ever touches guests that have the `ezdr-test` tag, a VMID from
this test, and disks on the test storage. The DR client refuses anything
else, so a bug or a compromised portal can't use cleanup to remove other
guests.

### 5.5 Verdict and history

Each test is kept per plan: who started it, when, the point in time, the
selected guests with their start and check results, when and how it ended,
and the operator's **verdict** (passed or failed, with notes), which can be
set while the test runs or afterwards. The plan page lists recent tests.

## 6. Plan settings

- **Test VMID offset** (default 10000). Validation checks that the offset
  VMIDs of the plan's guests are free on the DR host, and that the result is a
  valid VMID.
- **Test time limit** (default 8 hours).
- The test bridge (already part of the plan).

## 7. Client actions

New fixed actions (the client rejects anything else):

| Action | Does |
| --- | --- |
| List snapshots | The replicas' snapshots, for choosing a point in time |
| Prepare test | Create the test storage, clone, and register test guests (idempotent) |
| Start guest | Start one test guest |
| Check guest | Report status and guest agent response |
| Clean up test | Stop and destroy the test's guests and clones (idempotent, with the safety checks in 5.4) |

Registration and cleanup use Proxmox's own tools as root (`qm`, `pct`,
`pvesm`, and the cluster file system), not the read-only API token.

## 8. Out of scope

Failover, promoting replicas, DNS changes (phase 6); failback (phase 7);
running scripts inside test guests; reaching test guests from outside the
isolated bridge.
