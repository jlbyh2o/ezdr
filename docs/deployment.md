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
cp .env.example .env    # then set EZDR_DOMAIN=portal.example.com
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

## First sign-in

1. Find the one-time setup code in the portal's log:

   ```sh
   docker compose logs portal | grep setup_code
   ```

2. Open the portal, enter the setup code, and create the administrator.
3. Sign in. On first sign-in you add the portal to an authenticator app and
   receive recovery codes. Store the recovery codes somewhere safe.

## Add a Proxmox VE host

1. In the portal, open **Hosts** and select **Add host**.
2. Copy the **New host** command and run it as root on the Proxmox VE host.
3. Review the list of changes and confirm.

The host appears as online within a few seconds. Each token works for one host
and expires (after one hour by default).

To remove a host, select **Remove** in the portal; it's disconnected
immediately. Then run `ezdr unenroll` on the host to remove the client's
service, interface, and configuration.

## Back up the portal

Back up the `portal-data` volume. It contains:

- `ezdr.db`: the database.
- `secret.key`: the key that encrypts secrets in the database (the portal's
  WireGuard key and users' TOTP secrets). Without it, a database backup can't
  be restored. Keep a copy of this file separately from database backups.
- `tls.crt` and `tls.key` (self-signed option only). Restoring these keeps the
  certificate pin that enrolled hosts expect.

## Upgrade

Portal:

```sh
docker compose pull && docker compose up -d
```

Client: download `ezdr_linux_amd64.deb` from the
[latest release](https://github.com/jlbyh2o/ezdr/releases/latest) and run
`apt install ./ezdr_linux_amd64.deb`. The package restarts the client.
(Automatic updates through an apt repository are planned.)

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
