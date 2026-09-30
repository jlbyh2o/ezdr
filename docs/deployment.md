# Deploying EZDR

This guide covers running the portal, enrolling Proxmox VE hosts, and keeping
both up to date. See [Architecture](architecture.md) for how the pieces fit
together.

## Requirements

- A server for the portal **outside the sites it protects**, such as a small
  VPS: 1 vCPU, 1–2 GB of memory, Docker with the Compose plugin.
- A DNS name for the portal (for example, `portal.example.com`). Labs can use
  an IP address with the self-signed option below.
- Inbound firewall rules on the portal server:

  | Port | Protocol | Used for |
  | --- | --- | --- |
  | 80 | TCP | Let's Encrypt certificate issuance (Caddy option only) |
  | 443 | TCP | Web UI and host enrollment |
  | 51820 | UDP | WireGuard tunnels from hosts |

- Proxmox VE 9.x hosts with ZFS, able to reach the portal on TCP 443 and
  UDP 51820. Hosts need no inbound ports for the portal.
- For replication, either a network between the sites that the DR host can
  use to reach the primary (for example, a router site-to-site VPN; zrepl
  uses TCP 8888 by default), or an EZDR tunnel: forward one UDP port
  (default 51821) at one site to that site's host.

## Install the portal

Download the files from the [`deploy/`](../deploy) directory into a directory
on the server, for example `/opt/ezdr`.

### Option 1: Caddy with Let's Encrypt (recommended)

```sh
cp .env.example .env    # then set EZDR_DOMAIN=portal.example.com (and check EZDR_VERSION)
docker compose up -d
```

Caddy obtains and renews the certificate automatically.

### Option 2: Self-signed certificate

For labs and networks without a public DNS name. The portal serves HTTPS
itself, and enrollment tokens carry the certificate's pin, so hosts trust it
without further setup. Browsers show a certificate warning.

```sh
cp .env.example .env    # set EZDR_DOMAIN to the server's DNS name or IP address
docker compose -f compose.self-signed.yaml up -d
```

Browsers can't check a self-signed certificate, so before signing in, compare
the certificate's SHA-256 fingerprint your browser shows with the one in the
portal's log (`docker compose -f compose.self-signed.yaml logs portal | grep
sha256_fingerprint`). Otherwise someone who can intercept the connection
could capture your password and code. Prefer option 1 for production.

### Option 3 (optional): web UI only on a private network

For a portal whose web UI should be reachable only over a VPN (for example,
NetBird or Tailscale), while hosts' EZDR tunnels still use the server's
public address. Caddy gets a Let's Encrypt certificate through Cloudflare's
DNS, so the server needs no inbound web port. The files are in
[`deploy/private/`](../deploy/private); Caddy is built locally from the
official images with the Cloudflare DNS module.

Requirements:

- The portal's name in a Cloudflare zone, with an A record pointing to the
  server's VPN address.
- A Cloudflare API token with **Zone: DNS: Edit** on that zone, used only
  for the certificate (separate from the token for DNS failover).
- Hosts that can reach the VPN address while they enroll (for example, as
  VPN peers). After enrolling, they only use the WireGuard port.
- EZDR's tunnel ranges outside anything the hosts use, including the VPN's
  (NetBird and Tailscale use `100.64.0.0/10`, which EZDR's defaults are in).

```sh
cd deploy/private
cp .env.example .env    # set the domain, version, VPN address, public WireGuard endpoint, tunnel ranges, and token
docker compose -f compose.private.yaml up -d --build
```

Allow inbound UDP on the WireGuard port (`EZDR_WG_PORT`, 51820 unless the
VPN already uses it, as NetBird does) on the public address; the web UI
listens only on
`EZDR_PRIVATE_IP` (Docker's published ports bypass host firewall rules, so
the binding is what keeps it private). If the VPN interface can come up
after Docker at boot, let the server bind addresses that aren't up yet:
`echo net.ipv4.ip_nonlocal_bind=1 > /etc/sysctl.d/90-ezdr.conf && sysctl
--system`. The first sign-in and enrollment work as below, over the VPN.

## First sign-in

1. Find the one-time setup code in the portal's log:

   ```sh
   docker compose logs portal | grep setup_code
   ```

2. Open the portal, enter the setup code, and create the administrator.
3. Sign in. On first sign-in you add the portal to an authenticator app and
   receive recovery codes. Store the recovery codes somewhere safe.

## Secure the portal

Whoever controls the portal controls every enrolled host: the portal tells
the clients, which run as root on your hypervisors, what to replicate and
when to fail over. Treat the portal server as critical infrastructure:

- Run it on a dedicated server outside the protected sites, and allow only
  the ports above inbound. Restrict SSH to keys, and to known addresses if
  you can.
- Keep the server's operating system and Docker up to date, and upgrade the
  portal when a release fixes a security issue (watch the repository's
  releases).
- Keep the administrator's password and authenticator to the people who run
  failovers, and store the recovery codes offline.
- Set up email or webhook alerts (**Settings**), and review the **Audit**
  log: sign-ins, failed sign-ins, failovers, and configuration changes are
  recorded there.
- Protect backups of the portal as carefully as the portal itself (see
  below).

## Add a Proxmox VE host

1. In the portal, open **Hosts** and select **Add host**.
2. Copy the **New host** command and run it as root on the Proxmox VE host.
3. Review the list of changes and confirm.

The host appears as online within a few seconds. Each token works for one host
and expires (after one hour by default).

## Remove EZDR from a host

1. Fail back any failed-over plan that uses the host, and end running test
   failovers. (To also delete a plan's replicas, delete the plan with its
   data first.)
2. In the portal, select **Remove** on the host; it's disconnected
   immediately.
3. On the host, run `ezdr uninstall` (or `apt purge ezdr`).

Uninstalling removes EZDR's service, its wait before Proxmox starts guests
at boot, its zrepl jobs (releasing their holds, and removing their include
from `/etc/zrepl/zrepl.yml`), the test failover storage, the WireGuard
interfaces, the Proxmox VE API user, and `/etc/ezdr` and `/var/lib/ezdr`.
Replicas, snapshots, zrepl, and guest configurations stay. It refuses while
the host is part of a failover or test failover, and explains what to do;
`--force` proceeds anyway (a failed-over primary's guests are unlocked).
`ezdr unenroll` does the same but keeps the program installed, for enrolling
again.

## Guests at boot

On a plan's primary, Proxmox waits before starting guests at boot until the
EZDR client has heard from the portal (usually a few seconds). If the plan
was failed over while the primary was down, its guests stay stopped and
locked, so they don't run at both sites. If the portal can't be reached,
the guests start as usual after 5 minutes; set `boot_guard_timeout_seconds`
in `/etc/ezdr/config.json` to change that.

## Back up the portal

Back up the `portal-data` volume. It contains:

- `ezdr.db`: the database.
- `secret.key`: the key that encrypts the secrets in the database: the
  portal's WireGuard key, users' TOTP secrets, the DNS provider token, the
  SMTP password, and webhook secrets. Without it, a database backup can't be
  restored.
- `tls.crt` and `tls.key` (self-signed option only). Restoring these keeps the
  certificate pin that enrolled hosts expect.

A backup of the whole volume holds the database and its key together, so
anyone with the backup has the portal's secrets: encrypt backups and store
them securely. To keep the key out of volume backups, store it elsewhere
(for example, a file on the host mounted read-only into the container) and
point `EZDR_SECRET_KEY_FILE` at it; back that file up separately.

## Upgrade

Read the release notes first, and back up the portal.

Portal: set `EZDR_VERSION` in `.env` to the new version, then:

```sh
docker compose pull && docker compose up -d
```

Client: download `ezdr_linux_amd64.deb`, `checksums.txt`, and
`checksums.txt.sig` from the
[release](https://github.com/jlbyh2o/ezdr/releases), verify them (below), and
run `apt install ./ezdr_linux_amd64.deb`. The package restarts the client.
(Automatic updates through an apt repository are planned.)

## Verify a release

Each release's `checksums.txt` lists the SHA-256 checksums of its files and
is signed with EZDR's release key (fingerprint
`1EEE 2999 9590 0231 E3F0  2BF0 F248 3414 DBDD AB2B`, also in
[`packaging/release-signing-key.asc`](../packaging/release-signing-key.asc)).
The installer the portal shows checks this signature and the package's
checksum before installing anything. To verify files by hand:

```sh
gpg --import release-signing-key.asc
gpg --verify checksums.txt.sig checksums.txt    # "Good signature from EZDR release signing"
sha256sum -c --ignore-missing checksums.txt
```

The files and the portal image also carry GitHub build provenance, which
shows that they were built by this repository's release workflow:

```sh
gh attestation verify ezdr_linux_amd64.deb --repo jlbyh2o/ezdr
gh attestation verify oci://ghcr.io/jlbyh2o/ezdr-portal:0.1.0 --repo jlbyh2o/ezdr
```

## Configuration reference

The portal is configured with environment variables:

| Variable | Default | Purpose |
| --- | --- | --- |
| `EZDR_PUBLIC_URL` | (required) | Public URL, for example `https://portal.example.com`. Embedded in tokens; must use HTTPS to create tokens. The Compose files set it from `EZDR_DOMAIN`. |
| `EZDR_LISTEN` | `:8080` | Public listener address. |
| `EZDR_DATA_DIR` | `/data` in the container | Database, keys, and certificates. |
| `EZDR_TLS` | `off` | `off` (behind a reverse proxy) or `self-signed`. |
| `EZDR_TLS_PIN` | (none) | Pin for a private certificate presented by a proxy. See [Enrollment design](design/enrollment.md#3-enrollment-token). |
| `EZDR_WG_PORT` | `51820` | WireGuard UDP port. |
| `EZDR_WG_ENDPOINT` | public URL host and WireGuard port | WireGuard endpoint sent to hosts, as `host:port`. |
| `EZDR_TUNNEL_PREFIX` | `100.64.42.0/28` | Tunnel address range. Change it before enrolling hosts if it overlaps a network your hosts use. |
| `EZDR_SITE_TUNNEL_PREFIX` | `100.64.43.0/28` | Address range for EZDR tunnels between hosts (plans using the EZDR tunnel network path). Must not overlap `EZDR_TUNNEL_PREFIX`. |
| `EZDR_SECRET_KEY_FILE` | `<data dir>/secret.key` | Key that encrypts secrets in the database; generated if missing. |
| `EZDR_TRUSTED_PROXIES` | loopback and private ranges with `EZDR_TLS=off`; `none` with `self-signed` | Comma-separated ranges allowed to set `X-Forwarded-For`, or `none`. Only trust addresses that belong to your reverse proxy. |

## Troubleshooting

| Symptom | Likely cause |
| --- | --- |
| Enrollment warns "no WireGuard handshake" | Outbound UDP 51820 from the host, or inbound UDP 51820 to the portal, is blocked. |
| "tunnel range … overlaps" during enrollment | The host already uses addresses in `EZDR_TUNNEL_PREFIX` (for example, Tailscale or NetBird). Choose another range in the portal's configuration. |
| "portal certificate does not match the pin" | The portal's self-signed certificate changed (for example, the data volume was recreated). Create a new token after restoring `tls.crt`/`tls.key`, or re-enroll hosts. |
| Host shows offline | Check `ezdr status` and `journalctl -u ezdr` on the host. |
