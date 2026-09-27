# Test lab

EZDR reconfigures networking, ZFS, and workloads on Proxmox VE hosts. Develop
and test it only in a disposable lab, never against production hosts.

The recommended lab runs two nested Proxmox VE virtual machines (a "primary"
and a "DR" host) plus a small machine for the portal, all on an existing
Proxmox VE host.

## Prerequisites

Nested virtualization must be enabled on the outer Proxmox VE host:

```sh
cat /sys/module/kvm_intel/parameters/nested   # Intel
cat /sys/module/kvm_amd/parameters/nested     # AMD
```

The output should be `Y` or `1`. If not, add `options kvm_intel nested=1` (or
`options kvm_amd nested=1`) to a file in `/etc/modprobe.d/`, then reload the
module or reboot.

## Nested Proxmox VE hosts

Create two VMs with these settings:

| Setting | Recommendation | Notes |
| --- | --- | --- |
| OS | Latest Proxmox VE 9.x ISO | Same version on both. |
| CPU | 4 vCPUs, type `host` | `host` exposes virtualization extensions for nested guests. |
| Memory | 8 GB (6 GB minimum) | Cap the ZFS ARC (see below). |
| Disk controller | VirtIO SCSI single | Enable `iothread`, `discard`, and SSD emulation on each disk. |
| Boot disk | 32 GB, installed with ext4 | Keeps the OS separate from the replicated pool. |
| Data disks | 2 × 64 GB, thin-provisioned | Used for a ZFS mirror. |
| Network | 1 VirtIO NIC on a VLAN-aware bridge, firewall off | The outer firewall can drop nested guests' traffic. |
| Machine / BIOS | q35, OVMF (UEFI) | SeaBIOS also works. |

### Make the hosts intentionally different

Differences between the hosts exercise EZDR's storage and network mapping:

| | Primary | DR |
| --- | --- | --- |
| ZFS pool and Proxmox storage ID | `local-zfs` | `tank-dr` |
| Guest bridge (internal, VLAN-aware) | `vmbr1` | `vmbr2` |
| Isolated test-failover bridge | — | `vmbr99` |

Create the guest bridges **without a physical port** (`bridge-ports none`).
Test guests then stay off your real network: no DHCP leases or stray VLAN tags
on the LAN. Leave the management bridge (`vmbr0`) unchanged. Use the same VLAN
tags at both sites.

### After installation

1. Create the ZFS mirror from the two data disks and add it as Proxmox storage.
2. Cap the ZFS ARC at 2 GB by adding `options zfs zfs_arc_max=2147483648` to
   `/etc/modprobe.d/zfs.conf`, then run `update-initramfs -u` and reboot.
3. On the primary, create a few small Debian or Alpine LXC containers and one
   small VM on the guest bridge, with static addresses. Containers run well
   nested; the VM exercises zvol replication. A Debian cloud image
   (`debian-13-genericcloud-amd64.qcow2`) imported with `import-from` makes a
   small, quick-booting VM.
4. Snapshot both outer VMs after a clean install and again after the pool and
   workloads are set up, so you can roll back between experiments.

## Portal

A small Debian VM or LXC container with Docker: 1–2 vCPUs, 2 GB of memory, and
a 16 GB disk.

## Optional: simulated sites

To test hosts behind separate routers (NAT, port forwarding, one-sided
reachability for the site-to-site VPN), place each nested Proxmox VE VM behind
its own small router VM (for example, OPNsense) on separate bridges.
