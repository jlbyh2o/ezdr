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
| [Test lab](development/test-lab.md) | Recommended nested Proxmox VE lab for development and testing. |

See also [CONTRIBUTING.md](../CONTRIBUTING.md) for development setup and
conventions.

## Status

EZDR is in the early design phase. The documents here describe intended
behavior and will change as the design is refined.
