# EZDR Documentation

EZDR (Easy Disaster Recovery) is a self-hosted, open source tool for setting up
and managing disaster recovery between Proxmox VE hosts that use ZFS storage.

## Contents

| Document | Description |
| --- | --- |
| [Deploying EZDR](deployment.md) | Install the portal, enroll hosts, back up, and upgrade. |
| [Architecture](architecture.md) | High-level design: components, data flows, security model, and roadmap. |
| [Enrollment and control plane](design/enrollment.md) | Design for host enrollment, client identity, the client API, and user sign-in (phase 1). |
| [Inventory](design/inventory.md) | Design for collecting and reporting host inventory and replication readiness (phase 2). |
| [DR plans](design/dr-plans.md) | Design for DR plans: guests, mappings, schedule, retention, startup order, DNS records, and validation (phase 3). |
| [Replication](design/replication.md) | Design for activating plans, zrepl configuration, network paths, zrepl takeover, status, and alerts (phase 4). |
| [Test failover](design/test-failover.md) | Design for test failovers on isolated clones (phase 5). |
| [Failover](design/failover.md) | Design for planned, unplanned, and break-glass failovers, split-brain locks, and DNS switching (phase 6). |
| [Failback](design/failback.md) | Design for returning workloads to the primary (phase 7). |
| [Cleanup](design/cleanup.md) | Design for deleting a plan's replicated data and cleaning up after a takeover. |
| [Guest storage figures](design/guest-storage.md) | Design for showing each guest's allocated and used storage. |
| [Uninstalling the client](design/uninstall.md) | Design for removing EZDR from a host cleanly. |
| [Web UI](design/ui.md) | Design for the portal's web interface. |
| [Test lab](development/test-lab.md) | Recommended nested Proxmox VE lab for development and testing. |

See also [CONTRIBUTING.md](../CONTRIBUTING.md) for development setup and
conventions.

## Status

The design documents describe how each part was built. They're updated when
the behavior changes, and the code is the final reference.
