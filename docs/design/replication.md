# Design: Replication

> **Status:** Draft for phase 4. Covers activating DR plans, configuring zrepl
> on both hosts, the network paths between them, taking over an existing zrepl
> setup, replication status, and alerts. See [DR plans](dr-plans.md) and
> [Architecture](../architecture.md).

## 1. Summary

- **Activating** a valid plan configures zrepl on both hosts: a `source` job
  on the primary (snapshots and serving) and a `pull` job on the DR host
  (replication and pruning).
- The portal sends each host a **structured description** of its jobs. The
  client renders zrepl configuration from it, checks it with
  `zrepl configcheck`, and restarts zrepl only when it changed. The portal
  never sends raw configuration files.
- EZDR's jobs live in their own file, **`/etc/zrepl/ezdr.d/ezdr.yml`**, which
  the main `zrepl.yml` includes (zrepl 0.7). Jobs that EZDR doesn't manage
  are left alone.
- Hosts replicate over either an **existing network** (such as a router
  site-to-site VPN) or an **EZDR-managed WireGuard tunnel** between them,
  chosen per plan.
- zrepl connections always use **TLS** with a self-signed certificate
  generated on each host. The portal only distributes public certificates.
- An **existing zrepl setup can be taken over**: a preflight confirms that
  the first sync will be incremental, then one confirmation switches both
  hosts, upgrades zrepl to 0.7 where needed, releases the old jobs' holds,
  and verifies the first sync.
- Changes to active plans are **reviewed, then applied**.
- The portal shows **replication status** per plan and sends **alerts** by
  email and webhook.

## 2. Plan lifecycle

| State | Meaning |
| --- | --- |
| **Draft** | Saved but not applied to hosts. May have validation errors. |
| **Active** | Applied to both hosts; replication runs. |
| **Paused** | EZDR's jobs for the plan are removed from both hosts. Snapshots, replicas, and zrepl's bookmarks remain, so resuming continues incrementally. |

- A plan can only be activated or resumed with **no validation errors**.
- Deactivating returns a plan to draft and, like pausing, removes its jobs
  but **never deletes replicas or snapshots**. Deleting data is always a
  separate, explicit action (not in phase 4).
- An active plan must be deactivated before it can be deleted.

### 2.1 Changing an active plan

The portal stores two versions of an active plan: the **applied**
specification and the **editing** specification.

- Saving an active plan updates only the editing specification. The plan
  shows "pending changes".
- **Apply** shows, per host, what will change (jobs, tunnel, listening
  ports), then pushes it. Replication continues on the applied settings
  until then.
- **Discard** reverts the editing specification to the applied one.

Changes that don't come from the plan itself apply automatically. For
example, adding a disk to a protected guest adds its dataset to replication
on the next inventory report; the plan is unchanged, so no review is needed.

## 3. Host configuration

### 3.1 Desired state

Each host's desired state (sent on the `Subscribe` stream, see
[Enrollment](enrollment.md#62-desired-state-and-actions)) now includes,
across all active plans the host belongs to:

- **Source jobs** (as primary): job name, the exact datasets to replicate,
  snapshot prefix and interval, listening address, and the peer certificate
  and name allowed to connect.
- **Pull jobs** (as DR host): job name, the primary's address, the peer
  certificate and expected name, receive dataset, interval, and primary and
  DR retention.
- **Tunnels** (section 4.2).

The datasets are derived from the plan's guests and the primary's latest
inventory: every replicable disk of every protected guest. They are listed
exactly; EZDR never replicates a whole pool with exclusions.

### 3.2 Rendering

The client turns desired state into zrepl configuration:

- **Job names:** `ezdr_<plan>` for the source job and `ezdr_<plan>_pull` for
  the pull job, where `<plan>` is a short, stable plan identifier. zrepl
  embeds job names in holds and bookmarks, so names never change.
- **Snapshots:** periodic snapshots with the plan's prefix and interval.
- **Sends:** compressed, large-block, and embedded-data sends, as raw
  (encrypted) sends for encrypted datasets. zrepl sets send options per job,
  so a plan with both encrypted and unencrypted datasets gets one job pair
  per kind.
- **Receives:** replicas are received with `readonly=on`, so nothing on the DR
  host changes them and breaks incremental receives. Failover (phase 6) lifts
  this for promoted datasets.
- **Pruning:** the plan's tiers as zrepl grids, restricted to the plan's
  prefix, plus the always-on rules: never prune the primary's unreplicated
  snapshots (`not_replicated`) and never prune snapshots without the prefix.

The rendered file is checked with `zrepl configcheck` before it replaces the
current one. If the check fails, the old file stays, zrepl isn't restarted,
and the error is reported to the portal.

### 3.3 The main zrepl.yml

- If `/etc/zrepl/zrepl.yml` doesn't exist, EZDR creates it with logging
  settings and `include: [./ezdr.d/]`.
- If it exists, EZDR adds the `include` entry, keeping a backup
  (`zrepl.yml.ezdr-backup-<timestamp>`), and changes nothing else, except
  during takeover (section 5).
- Job names are unique across included files, so a leftover job with an
  `ezdr_` name is reported as a conflict rather than overwritten.

### 3.4 Installing zrepl

- zrepl comes from zrepl's official apt repository, version 0.7.x. The
  repository's signing key fingerprint is built into the client, so a
  tampered key is rejected.
- Installation happens on activation, and is listed in the activation
  confirmation (section 6).

## 4. Network paths

Each plan chooses how the DR host reaches the primary's zrepl `source` job.

### 4.1 Existing network

The hosts can already reach each other, for example over a router
site-to-site VPN or a private link.

- **Plan settings:** the primary's address as seen from the DR host, and the
  port (default 8888).
- The primary's `source` job listens on that port on all addresses, or on a
  specific address if set. TLS authentication (section 4.3) rejects anyone
  but the plan's DR host.

### 4.2 EZDR tunnel

For sites without a link, EZDR creates a WireGuard tunnel between the two
hosts, separate from the control-plane tunnel to the portal.

- **One tunnel per host pair**, shared by all plans between those hosts.
- **Plan settings:** which host accepts the connection, its public endpoint
  (`host:port`, typically a port forward on that site's router), and the
  listen port (default 51821).
- **Addresses** come from a separate configurable range (default
  `100.64.43.0/28`). Each tunnel's peers allow only each other's single
  address, like the control-plane tunnel.
- **Keys** are generated on the hosts; the portal distributes only public
  keys. The connecting host sends keepalives.
- The overlap check from enrollment also runs for this range before a tunnel
  is created.
- The `source` job listens only on the primary's tunnel address.

### 4.3 zrepl TLS

- Each host generates a key pair and a long-lived self-signed certificate for
  zrepl (in `/etc/ezdr/zrepl/`), named after the host's EZDR host ID.
- The portal collects each host's public certificate and puts the peer's
  certificate in the desired state. Each side trusts exactly its peer's
  certificate: there is no certificate authority.
- Over an EZDR tunnel this doubles up on encryption. It's kept anyway, so
  zrepl's authentication doesn't depend on which network path is used.

> **To verify in the lab:** zrepl's TLS transport accepting a peer's
> self-signed certificate as its trust root. Fallback: a per-plan certificate
> authority whose key stays on the portal.

## 5. Taking over an existing zrepl setup

### 5.1 Detection

Inventory gains a **zrepl** section: installed version, whether the service
runs, and a summary of each existing job (name, type, listen or connect
address, `root_fs`, prefix, interval, filesystem filter, and pruning grids).

In the plan editor, **Adopt existing zrepl setup** fills in the receive
dataset, snapshot prefix, interval, retention, network path, and address from
the detected jobs. It also selects the guests whose disks the old filter
replicates.

### 5.2 Preflight

When an adopted plan is activated, both clients run a preflight and report:

- For each protected dataset: the newest snapshot common to both hosts. A
  dataset without one needs a **full send**, which is listed with its size.
- Datasets the old job replicates that the plan doesn't include. These stop
  being replicated.
- Old jobs that the takeover would remove, and any other jobs in the
  configuration (left in place).
- zrepl versions, and whether an upgrade to 0.7 is needed.

### 5.3 Switch-over

After one confirmation, the portal runs these steps:

1. Upgrade zrepl to 0.7 on both hosts if needed. 0.6.1 and 0.7 interoperate,
   so either host can go first.
2. **DR host:** remove the old `pull` job, so no replication runs during the
   switch.
3. **Primary:** replace the old `source` job with EZDR's.
4. **DR host:** add EZDR's `pull` job.
5. Wait for the first EZDR replication and check that every dataset
   replicated **incrementally** from the common snapshot found in preflight.
6. Release the old jobs' zrepl holds and bookmarks
   (`zrepl zfs-abstraction release-stale`) on both hosts.

The main `zrepl.yml` files are backed up before step 2. If any step before
step 6 fails, both hosts' configurations are restored and the old setup
resumes. Progress and results are shown in the portal and recorded in the
audit log.

## 6. Activation

For a plan without existing replication, activation shows a confirmation
listing, per host:

- installing zrepl (if needed)
- creating the zrepl TLS certificate
- creating the tunnel (EZDR tunnel plans)
- the zrepl jobs and listening port
- the first full send's size (from inventory), which may take a long time

After confirmation, both hosts apply their desired state, and the plan shows
the initial replication's progress.

## 7. Replication status

- Every 60 seconds, and on request, clients report zrepl's status
  (`zrepl status --mode raw`) for EZDR's jobs: each job's state and last run,
  and each dataset's newest replicated snapshot, bytes transferred, and
  errors.
- Per plan, the portal shows:
  - **Health:** healthy, lagging, failing, or unknown (a host is offline).
  - **RPO age:** the age of the oldest dataset's newest replicated snapshot.
  - **Per dataset:** the latest replicated snapshot, its age, and any error.
- The plans list and host pages show each plan's health.

## 8. Alerts

### 8.1 Conditions

| Alert | Fires when | Default |
| --- | --- | --- |
| RPO exceeded | The RPO age exceeds the plan's threshold | 3× the snapshot interval (configurable per plan) |
| Replication failing | A job reports errors for two consecutive runs | — |
| Host offline | A host with active plans has had no command stream for 5 minutes | — |
| Activation or takeover failed | A step fails | — |

- Alerts are sent when they **fire** and when they **resolve**, with a
  reminder every 12 hours while still firing.
- Alert history is shown in the portal.

### 8.2 Channels

A new **Settings** page configures:

- **Email (SMTP):** server, port, TLS mode, username, password, sender, and
  recipients.
- **Webhooks:** URLs, each with an optional secret. Requests are JSON
  (`event`, `severity`, `plan`, `host`, `message`, `time`), signed with an
  HMAC-SHA256 header when a secret is set.

Each channel has a **Send test** button. SMTP passwords and webhook secrets
are encrypted in the database like other secrets. Changes are recorded in the
audit log.

## 9. Security

- The portal describes jobs; it can't make a client run arbitrary zrepl
  configuration, commands, or files. Clients validate every field (dataset
  names, addresses, ports, prefixes) before rendering.
- The zrepl apt repository key is pinned in the client.
- zrepl's `source` job only accepts the plan's DR host (TLS peer
  certificate). Over an EZDR tunnel it listens only on the tunnel address.
- Takeover and activation are recorded in the audit log, including each
  step's outcome.

## 10. Phase 4 scope

In scope: everything above.

Out of scope: deleting replicas or snapshots, bandwidth limits, test failover
(phase 5), and failover (phase 6).
