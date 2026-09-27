# Design: Replication

> **Status:** Approved for phase 4. Covers activating DR plans, configuring zrepl
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
  this for promoted datasets. The placeholder datasets zrepl creates to mirror
  the source path use `recv.placeholder.encryption: inherit`, which zrepl
  requires to be set.
- **Pruning:** the plan's tiers as zrepl grids, restricted to the plan's
  prefix, plus the always-on rules: never prune the primary's unreplicated
  snapshots (`not_replicated`) and never prune snapshots without the prefix.

The rendered file is checked with `zrepl configcheck` before it replaces the
current one. If the check fails, the old file stays, zrepl isn't restarted,
and the error is reported to the portal.

### 3.3 The main zrepl.yml

- If `/etc/zrepl/zrepl.yml` doesn't exist, EZDR creates it with logging
  settings and `include: [/etc/zrepl/ezdr.d/]`. The path is always absolute:
  zrepl 0.7.0 resolves relative include paths against its working directory,
  not the configuration file's directory.
- If it exists, EZDR adds the `include` entry, keeping a backup
  (`zrepl.yml.ezdr-backup-<timestamp>`), and changes nothing else, except
  during takeover (section 5).
- Job names are unique across included files, so a leftover job with an
  `ezdr_` name is reported as a conflict rather than overwritten.

### 3.4 Installing zrepl

- zrepl comes from zrepl's official apt repository, version 0.7.x. The
  repository's signing key fingerprint
  (`E101 418F D3D6 FBCB 9D65 A62D 7086 99FC 5F2E BF16`) is built into the
  client, so a tampered key is rejected.
- The package is held (`apt-mark hold zrepl`). Until zrepl 1.0, its repository
  publishes new releases, including breaking ones, immediately. EZDR upgrades
  zrepl deliberately, on both hosts of a plan together.
- Installation happens on activation, and is listed in the activation
  confirmation (section 6).

## 4. Network paths

Each plan chooses how the DR host reaches the primary's zrepl `source` job.

### 4.1 Existing network

The hosts can already reach each other, for example over a router
site-to-site VPN or a private link.

- **Plan settings:** the primary's address as seen from the DR host, the
  port (default 8888), and optionally the address the primary listens on.
- The primary's `source` job listens on that port on all addresses, or only
  on the listen address if set (with `listen_freebind`, so zrepl can bind
  before the address is up). TLS authentication (section 4.3) rejects anyone
  but the plan's DR host.

### 4.2 EZDR tunnel

For sites without a link, EZDR creates a WireGuard tunnel between the two
hosts, separate from the control-plane tunnel to the portal.

- **One tunnel per host pair**, shared by all plans between those hosts, so
  their tunnel settings must match (validation reports conflicts). Each host
  has a single site tunnel interface, `ezdr1`, with one peer per replication
  partner, and a stable address from the site range.
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
- The `source` job listens only on the primary's tunnel address, with
  `listen_freebind` so zrepl can bind before the interface is up. The client
  brings the tunnel up before applying zrepl jobs.
- A host accepts tunnels on one UDP port for all its partners; plans that
  would need different ports on the same host are rejected.
- Switching a plan between network paths is an ordinary change: the preview
  shows the tunnel being created or removed, and replication continues
  incrementally.

### 4.3 zrepl TLS

- Each host generates a key pair and a long-lived self-signed certificate for
  zrepl (in `/etc/ezdr/zrepl/`), named after the host's EZDR host ID.
- The portal collects each host's public certificate and puts the peer's
  certificate in the desired state. Each side trusts exactly its peer's
  certificate: there is no certificate authority.
- Certificates are leaf certificates (not certificate authorities) with the
  host name as both common name and DNS subject alternative name. zrepl
  accepts a peer's self-signed certificate as its trust root. Verified in the
  lab: a peer with the wrong name, and an impostor with the right name but a
  different certificate, are both rejected.
- Over an EZDR tunnel this doubles up on encryption. It's kept anyway, so
  zrepl's authentication doesn't depend on which network path is used.

## 5. Taking over an existing zrepl setup

### 5.1 Detection

Inventory gains a **zrepl** section: installed version, whether the service
runs, and a summary of each existing job (name, type, listen or connect
address, `root_fs`, prefix, interval, filesystem filter, and pruning grids).

On a draft plan, **Adopt existing zrepl setup** picks a detected source job
on the primary and the pull job on the DR host that connects to it. It fills
in the receive dataset, snapshot prefix, interval, retention, and the
existing-network settings (address, port, and listen address) from those
jobs, and selects the guests whose disks the old filter replicates. The plan
records the adopted job names, so activating it runs a takeover instead of a
plain activation. Validation reports an error if a setting that incremental
sends depend on (receive dataset or prefix) no longer matches the adopted
jobs, or if those jobs disappear. It warns about protected datasets the old
job doesn't replicate (they need a full send) and datasets it replicates
that the plan doesn't (they stop being replicated). **Stop adopting** turns
the plan back into an ordinary one.

### 5.2 Preflight

When an adopted plan is activated, both clients run a preflight and report:

- For each protected dataset: the newest snapshot common to both hosts,
  matched by GUID. A bookmark on the primary counts as a common base too,
  since zrepl can send incrementally from it. A dataset without one needs a
  **full send**, which is listed with its size.
- Datasets the old job replicates that the plan doesn't include. These stop
  being replicated.
- The old jobs' holds and bookmarks that step 6 will release (from
  `zrepl zfs-abstraction release-all --job <old job> --dry-run`).
- Old jobs that the takeover would remove, and any other jobs in the
  configuration (left in place).
- zrepl versions, and whether an upgrade to 0.7 is needed.

The takeover can't start while the preflight reports a problem:

- a replica exists on the DR host but shares no snapshot with the primary
  (zrepl can't replicate into it, and EZDR never destroys replicas);
- a replica has a snapshot newer than the newest common one that the primary
  doesn't have (diverged);
- another hand-written job on the primary listens on the plan's port;
- zrepl isn't installed on a host.

Confirmation needs a preflight from the last 15 minutes; otherwise it runs
again.

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
6. Release the old jobs' zrepl holds and bookmarks by job name on both hosts
   (`zrepl zfs-abstraction release-all --job <old job>`): the old source
   job's on the primary and the old pull job's on the DR host (zrepl names
   the DR host's last-received hold after the pull job). `release-stale`
   doesn't cover this: it only releases markers superseded by newer ones,
   not those of removed jobs.

Steps 2 and 3 edit the main `zrepl.yml` structurally: they remove the adopted
jobs, add the `include` entry, and keep everything else (global settings,
other jobs, comments). The old jobs' certificate and key files are left in
place.

The main `zrepl.yml` files are backed up before step 2. If any step before
step 6 fails, both hosts' configurations are restored and the old setup
resumes. An upgrade from step 1 is kept: zrepl 0.7 runs 0.6 configurations
unchanged, and downgrading during a failure would add risk. The plan's alerts
are muted during the switch-over. Progress and results are shown in the
portal and recorded in the audit log.

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
| RPO exceeded | The RPO age exceeds the plan's threshold | 3× the snapshot interval (configurable per plan). Snapshots and pulls run on independent schedules, so replicated data is normally up to about 2× the interval old; lower thresholds get a validation warning. |
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
