# Design: Failback

> **Status:** Approved for phase 7. Covers returning a failed-over plan's guests
> to the original primary: copying the DR host's changes back, a short
> cutover, and resuming normal replication. See [Failover](failover.md) and
> [Replication](replication.md).

## 1. Summary

- Failback copies the DR host's changes back **incrementally** from the last
  snapshot both hosts have, using **EZDR's own transfer**: zrepl can't
  replicate back into the guests' original datasets (it receives under
  `<root_fs>/<source path>`).
- While the guests keep running on the DR host, changes are copied back in
  **rounds** until the difference is small. Then the **cutover** stops the
  guests on the DR host, copies the last changes, and starts them on the
  primary. Downtime is about one small incremental copy.
- A **preflight** shows what failing back would do, including primary
  datasets with changes newer than the common snapshot (possible after an
  unplanned failover). Those changes are **discarded** on confirmation.
- Afterwards the DR host's guest registrations are removed (never their
  disks: those are the replicas), the replicas become read-only again, and
  **replication resumes** incrementally in the original direction. DNS is
  switched back on confirmation.

Making the DR host the permanent primary (swapping roles) is out of scope.

## 2. The transfer

- The **primary listens** on the plan's zrepl address and port, which are
  free while the plan is failed over (its source job is removed). The DR
  host connects over the same network path replication uses.
- Both sides authenticate with their existing zrepl certificates: the
  primary only accepts the DR host's certificate, and the DR host only
  accepts the primary's (mutual TLS, pinned certificates as in section 4.3
  of [Replication](replication.md)).
- For each dataset the DR host sends a header (target dataset, from and to
  snapshots) followed by `zfs send -I @<from> <replica>@<to>` (compressed,
  large blocks, embedded data; raw for encrypted datasets), so intermediate
  snapshots arrive too and zrepl's history stays consistent. The primary
  runs `zfs receive -F` into the original dataset and reports the result.
- The primary only receives into the plan's datasets it was told about, and
  only while a failback runs; the listener stops when the failback ends or
  after a time limit.

## 3. Preflight

Before the failback starts, both hosts list the plan's datasets' snapshots
(matched by GUID, as for takeovers) and the portal reports:

- each dataset's **common snapshot**. A dataset without one blocks the
  failback (a full copy isn't supported in phase 7);
- **diverged datasets** on the primary: snapshots newer than the common one,
  or data written after it, with the amount. Failing back discards them;
- guests whose configuration on the DR host has a **disk the primary's
  configuration doesn't** (added at the DR site). This blocks the failback:
  its data would be lost;
- other configuration changes made at the DR site (memory, cores, NICs).
  These are reported; the primary keeps its own configuration;
- guests created on the DR host outside the plan (not failed back).

The operator confirms by typing the plan's name, and separately confirms
discarding diverged data if there is any.

## 4. Steps

| Step | Guests run on | Undone on failure? |
| --- | --- | --- |
| 1. Start the primary's receiver | DR host | yes |
| 2. Copy changes back in rounds: snapshot the replicas on the DR host (the plan's prefix), send the changes since the last round. Repeat until a round copies less than 256 MiB, at most 5 rounds. The first round rolls diverged primary datasets back. | DR host | yes |
| 3. Shut down the guests on the DR host (reverse startup order, forced off after the shutdown timeout) | — | yes: they're started again |
| 4. Take a final snapshot and send the last changes | — | yes: the DR guests are started again |
| 5. Remove the DR host's guest registrations (configuration files only), remove the plan's `ezdr-` storages, and make the replicas read-only | — | no (retry) |
| 6. Resume replication: the plan becomes active; the primary's source job and the DR host's pull job return | — | no (retry) |
| 7. Unlock the primary's guests (restoring onboot) and start them in startup order with their delays; check them | primary | no (retry) |
| 8. Wait for the operator to verify the guests, then switch DNS back to production values on confirmation | primary | — |

- Before step 5, a failure leaves the plan failed over with the guests
  running on the DR host (restarted if they were stopped), and the failback
  can be run again. Data already copied to the primary is simply superseded
  by the next attempt.
- From step 5 on, the failback is committed: failed steps are retried.
- Plans can't be changed while failing back.
- Replication resumes incrementally: after the cutover the primary's
  datasets and the replicas both end at the final snapshot.

## 5. Security

- The receiver is a fixed client action with an allowlist of datasets and a
  pinned peer certificate, open only during a failback.
- Removing DR registrations checks that each guest is this plan's
  failed-over guest and is stopped, and deletes only its configuration file:
  never `qm destroy`, which would destroy the replicas.

## 6. Out of scope

Full resynchronization when no common snapshot exists; carrying disks added
at the DR site back to the primary; swapping the plan's roles.
