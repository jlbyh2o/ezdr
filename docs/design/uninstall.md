# Uninstalling the client

> **Status:** Approved. Covers removing EZDR from a Proxmox VE host so that the
> host, its guests, and zrepl work normally afterward.

## 1. Commands

- `ezdr uninstall [--yes] [--force]` removes everything EZDR set up on the
  host (section 3), then purges the `ezdr` package.
- `ezdr unenroll [--yes] [--force]` does the same but keeps the package, for
  enrolling again (for example, with another portal).
- `apt remove ezdr` and `apt purge ezdr` run the same cleanup
  non-interactively before removing the files (upgrades don't).

Like enrollment, the commands first list what they'll change and ask for
confirmation (`--yes` skips it). The host must also be removed in the
portal; the commands say so.

## 2. Unsafe states

The cleanup refuses, and says what to do instead, while the host is part of
a DR operation:

| State | Found by | Instead | With `--force` |
| --- | --- | --- | --- |
| Primary: guests locked because their plan is failed over | the `ezdr-failed-over` tag | fail back first | the guests are unlocked and their start-at-boot setting restored (as failback does), with a warning that the DR copies may still run |
| DR host: failed-over guests registered here | the failover marker in their notes, or a break-glass record | fail back first | the guests and their storages stay (they're production now); only EZDR is removed |
| DR host: a test failover's guests or clones exist | the test marker, and clones under the test dataset | end the test | the test guests, clones, and storage are removed as ending a test does |

Package removal follows the same rules: in an unsafe state it aborts the
removal (the package's pre-removal script fails) with a message pointing to
`ezdr uninstall --force`, so nobody is left with locked guests and no tool
to unlock them.

## 3. What is removed

In this order, each step safe to repeat:

1. Stop and disable the `ezdr` service.
2. Remove the boot guard's drop-in for `pve-guests.service`
   (failover design, 5.2) and reload systemd.
3. zrepl: release the holds and step bookmarks of EZDR's jobs (`zrepl
   zfs-abstraction release-all --job <job>` for each job in EZDR's jobs
   file), then remove the jobs file, the `include` of EZDR's jobs directory
   in `/etc/zrepl/zrepl.yml` (keeping everything else, with a backup), and
   the directory, and restart zrepl if it runs.
4. The test storage (`ezdr-test-<pool>`) and its dataset, if empty.
5. The WireGuard interfaces (`ezdr0`, and `ezdr1` if present).
6. The Proxmox VE API user EZDR created, and its token.
7. The service unit, if enrollment wrote one.
8. `/etc/ezdr` (keys, certificates, configuration) and `/var/lib/ezdr`
   (guest configurations and recovery information kept on DR hosts).

## 4. What stays

- Replicas and their snapshots on DR hosts, and the plan's snapshots on
  primaries: they're data. Delete a plan with its data in the portal first
  to remove them (cleanup design).
- zrepl, its apt source, and its configuration apart from EZDR's jobs. If
  EZDR took over a hand-written setup, the original configuration is kept
  as `/etc/zrepl/zrepl.yml.ezdr-takeover-<id>`; the command points to it.
- Guest configurations (apart from `--force` unlocking, above).

## 5. Verification

On lab hosts: uninstall a primary and a DR host of an active plan (zrepl
restarts without EZDR's jobs, no holds left, guests start at boot, the boot
guard is gone), refusal and `--force` on a failed-over primary, `apt purge`
on an enrolled host, and enrolling again afterward.
