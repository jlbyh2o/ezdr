# Design: Enrollment and control plane

> **Status:** Approved for phase 1. Covers how hosts join a portal, how they
> authenticate, how the client and portal communicate, and how people sign in
> to the portal. See [Architecture](../architecture.md) for the overall design.

## 1. Summary

- An admin creates a **single-use enrollment token** in the portal and runs
  `ezdr enroll <token>` on a Proxmox VE host.
- The client generates a **WireGuard key pair locally** and exchanges its
  public key for a tunnel address over a one-time HTTPS request.
- From then on, the client talks to the portal **only through the WireGuard
  tunnel**. The tunnel is the client's identity: WireGuard cryptographically
  binds each tunnel address to one host's key, so the portal needs no internal
  certificate authority.
- Client, portal, and web UI communicate with **ConnectRPC** using **protobuf**
  schemas shared across Go and TypeScript.
- The client keeps a long-lived stream open to the portal to receive desired
  state and actions. **The portal never connects to clients.**
- People sign in with a **password plus TOTP**. The portal sits behind
  **Caddy**, which provides HTTPS.

## 2. Components and listeners

```text
                      Internet
                         │
          TCP 80/443     │        UDP 51820
     ┌───────────────────┼───────────────────────┐
     │  ┌────────────────▼─────┐                  │
     │  │ Caddy (HTTPS, ACME)  │                  │
     │  └────────────────┬─────┘                  │
     │                   │ HTTP                   │
     │  ┌────────────────▼──────────────────────┐ │
     │  │ ezdr-portal                           │ │
     │  │                                       │ │
     │  │  Public listener                      │ │
     │  │   • Web UI and user API               │ │
     │  │   • Enrollment endpoint               │ │
     │  │                                       │ │
     │  │  Tunnel listener (userspace           │◄┘
     │  │  WireGuard, inside the process)       │
     │  │   • Client API only                   │
     │  └───────────────────────────────────────┘
     │                 Portal host (VPS)
     └────────────────────────────────────────────
```

The portal has two separate listeners:

| Listener | Reachable from | Serves |
| --- | --- | --- |
| Public (plain HTTP behind Caddy) | The internet, through Caddy | Web UI, user API, enrollment endpoint |
| Tunnel (inside userspace WireGuard) | Enrolled clients only | Client API |

The client API is **never** served on the public listener. A request to the
client API can only arrive through an authenticated WireGuard peer.

The portal runs WireGuard in userspace (`wireguard-go` with an in-process
network stack), so the container needs no extra privileges or kernel modules;
it only needs UDP 51820 published. Caddy does not proxy UDP, so that port is
published directly from the portal container.

Clients use the kernel's built-in WireGuard, configured through netlink by the
`ezdr` service. The interface is named `ezdr0`.

## 3. Enrollment token

Tokens are created in the portal by an admin.

- **Single use** and **expiring**: 1 hour by default, configurable per token.
- The portal stores only a **hash** of the secret, never the secret itself.
- Tokens can be revoked before use. Creation, use, and revocation are recorded
  in the audit log.
- Both enrollment endpoints (`CheckToken` and `Enroll`) require the full
  token, including its secret, and are rate-limited per source address.

The token is a single string the admin copies to the host. It encodes:

| Field | Purpose |
| --- | --- |
| Portal URL | Where to send the enrollment request, for example `https://portal.example.com`. |
| Token ID | Lets the portal look up the token record. |
| Secret | 32 random bytes proving possession of the token. |
| TLS pin (optional) | SHA-256 hash of a trusted certificate's public key. |

Format: `ezdr1_` followed by the base64url-encoded fields. The prefix makes
tokens easy to recognize (and to detect with secret scanners) and allows the
format to change later.

### TLS trust during enrollment

Enrollment is the only request sent over the internet, so the client must be
sure it's talking to the real portal:

- **Default:** the client verifies the portal's certificate against the system
  trust store. This works with Caddy's automatic Let's Encrypt certificates.
- **Private certificates:** if the portal uses a self-signed or internal CA
  certificate (common in labs), the admin sets a TLS pin in the portal settings.
  It's embedded in every token, and the client accepts only a certificate chain
  containing that public key.

## 4. Enrollment flow

```text
Admin                     Client (Proxmox host)                  Portal
  │                              │                                  │
  │ 1. Create token ───────────────────────────────────────────────►│
  │◄──────────────────────────────────────────────── token string ──│
  │ 2. ezdr enroll <token> ─────►│                                  │
  │                              │ 3. HTTPS CheckToken(token) ─────►│ validate only;
  │                              │◄──── tunnel range, endpoint ─────│ token not used
  │                   4. Check prerequisites                        │
  │                   5. Show planned changes; confirm              │
  │                   6. Generate WireGuard key pair                │
  │                              │ 7. HTTPS Enroll(token, pubkey,   │
  │                              │    host facts) ─────────────────►│ 8. Validate token,
  │                              │                                  │    register host,
  │                              │◄──────── 9. Peer config ─────────│    assign tunnel IP
  │                  10. Configure ezdr0, start service             │
  │                              │ 11. Stream opens through ───────►│
  │                              │     the tunnel                   │ host shown online
```

1. The admin creates a token in the portal.
2. The admin runs `ezdr enroll <token>` as root on the host.
3. The client calls `CheckToken` over HTTPS. The portal confirms the token is
   valid **without using it** and returns the tunnel range and the portal's
   WireGuard endpoint. This lets the client run all checks before anything is
   committed on either side.
4. The client checks prerequisites (section 4.2).
5. The client lists the changes it will make (interface, files, service) and
   asks for confirmation. `--yes` skips the prompt for automation.
6. The client generates a WireGuard key pair. The private key is written to
   `/etc/ezdr/` (mode `0600`) and never leaves the host.
7. The client calls `Enroll` over HTTPS with the token ID and secret, its
   WireGuard public key, and host facts: hostname, `/etc/machine-id`, Proxmox
   VE version, and client version.
8. The portal validates the token again (exists, unused, unexpired, secret
   matches), marks it used, registers the host, and assigns a tunnel address.
   These steps happen in one database transaction, so a token can never enroll
   two hosts.
9. The portal returns the peer configuration: host ID, the client's tunnel
   address, the portal's WireGuard public key and endpoint, and the client API
   address inside the tunnel.
10. The client writes its configuration, creates `ezdr0`, and enables and
    starts the `ezdr` systemd service.
11. The service opens the command stream through the tunnel (section 6). The
    portal marks the host online.

If a prerequisite check fails, nothing has changed and the token is still
valid: fix the problem and run `ezdr enroll` again. If any step after 8 fails,
the admin deletes the half-enrolled host in the portal and enrolls again with a
new token. The client command reports clearly which step failed.

### 4.1 Install and enroll in one command

The portal's **Add host** dialog creates a token and shows two ready-to-copy
commands:

- **New host (default):** installs the client and enrolls in one step.

  ```sh
  curl -fsSL https://github.com/jlbyh2o/ezdr/releases/latest/download/install.sh | sh -s -- ezdr1_…
  ```

- **Client already installed:**

  ```sh
  ezdr enroll ezdr1_…
  ```

The install script:

1. Checks that it's running as root on a supported Proxmox VE host.
2. Installs the `ezdr` package. Until the apt repository exists, it downloads
   the `.deb` from the matching GitHub release and verifies it against the
   release's published SHA-256 checksums. Once the signed apt repository is
   available, the script adds it instead, and apt verifies package signatures
   and provides updates.
3. Runs `ezdr enroll` with the token. The usual checks and confirmation prompt
   still apply.

The script is served from the project's GitHub releases, **never from the
portal**, so a compromised portal can't hand hosts a malicious installer.
Tokens are single-use and short-lived, so a token left in shell history or
briefly visible in the process list is not useful to anyone else.

### 4.2 Prerequisite checks

Before making changes, `ezdr enroll` verifies:

- It is running as root.
- The host runs Proxmox VE 9.x.
- The WireGuard kernel module can be loaded.
- The system clock is synchronized.
- The tunnel range (from `CheckToken`) doesn't overlap any existing address or
  route on the host.
- The host is not already enrolled (unless `--force` is given, which removes
  the old local configuration first).

After the tunnel comes up, the client waits for a WireGuard handshake. If none
completes within 30 seconds, it reports that outbound UDP to the portal's
WireGuard port may be blocked.

Tools needed only in later phases, such as zrepl, are not installed at
enrollment. They are installed when a DR plan first needs them, with the same
list-and-confirm approach.

## 5. Tunnel addressing and isolation

- Tunnel addresses come from a configurable IPv4 range. The default is the
  small `100.64.42.0/28`, inside `100.64.0.0/10` (shared address space):
  - It's unlikely to overlap typical LAN ranges such as `10.0.0.0/8` or
    `192.168.0.0/16`.
  - It deliberately avoids the start of `100.64.0.0/10`. Other overlay tools
    that allocate addresses in order, such as Headscale, begin at
    `100.64.0.1`.
  - A small range keeps the footprint on the host's routing table minimal.
- The portal takes the first usable address (`100.64.42.1`); each host gets the
  next free one. The default range leaves 13 addresses for hosts. Larger
  deployments can configure a bigger range before enrolling hosts.
- Hosts that also run Tailscale, NetBird, or another tool using
  `100.64.0.0/10` may still conflict. Enrollment refuses to proceed if the
  tunnel range overlaps an existing route on the host (see section 4.2). The
  admin then chooses a different range in the portal settings.
- On the portal, each peer's allowed IPs are exactly that host's `/32`. On each
  client, the portal peer's allowed IPs are exactly the portal's `/32`.
- The portal does not forward packets between peers. **Clients cannot reach
  each other through the portal.** Site-to-site traffic uses its own separate
  tunnel (phase 4).
- Clients send a persistent keepalive every 25 seconds so NAT mappings stay
  open.

## 6. Client identity and the client API

When a request arrives on the tunnel listener, the portal identifies the host
by the request's source tunnel address. WireGuard guarantees that only the
holder of that host's private key can send packets from that address, so no
further credentials are needed.

Traffic inside the tunnel uses HTTP/2 without TLS (h2c); WireGuard already
encrypts and authenticates it.

### 6.1 Client API (ConnectRPC)

| RPC | Type | Purpose |
| --- | --- | --- |
| `Subscribe` | Server stream | Portal pushes desired state and actions to the client. |
| `ReportStatus` | Unary | Client reports health and, in later phases, replication status. |
| `ReportInventory` | Unary | Client reports inventory (phase 2). |
| `AckAction` | Unary | Client reports progress and the result of an action. |

Server streaming plus unary calls avoids needing bidirectional streaming, which
keeps the protocol simple and works over HTTP/1.1 as well as HTTP/2.

### 6.2 Desired state and actions

The portal sends two kinds of messages on the `Subscribe` stream:

- **Desired state:** the full configuration the host should have, with an
  increasing generation number. The client applies it, then reports the
  generation it has applied. Sending the full state (not a diff) means a
  client that reconnects after any outage converges correctly.
- **Actions:** named, one-off operations such as "start test failover for
  plan X". Each has an ID and an expiry. The client runs an action at most once,
  reports progress through `AckAction`, and ignores expired actions.

The client accepts only message types it knows. There is no generic "run
command" action (see [Security model](../architecture.md#7-security-model)).

### 6.3 Connection health

- The stream carries a heartbeat every 15 seconds.
- The portal marks a host offline if it has no open stream for 60 seconds.
- The client reconnects with exponential backoff (1 second up to 60 seconds,
  with jitter).

## 7. Revocation and re-enrollment

- **Removing a host** in the portal deletes its WireGuard peer immediately. The
  host can no longer reach the client API, even though it still has its keys.
- **Reinstalled hosts** enroll again with a new token. The portal flags a new
  enrollment with the same `machine-id` as an existing host, so the admin can
  remove the old record.
- `ezdr unenroll` on the host removes `ezdr0`, the service, and `/etc/ezdr/`.
- WireGuard key rotation is deferred to a later phase.

## 8. User authentication

- **Local accounts** with passwords hashed using Argon2id.
- **TOTP (authenticator app) is required.** Users set it up at first sign-in
  and receive one-time recovery codes.
- **Sessions** use a server-side session with an `HttpOnly`, `Secure`,
  `SameSite=Strict` cookie. Because ConnectRPC calls are `POST` requests with a
  protobuf or JSON content type, a SameSite cookie plus a content-type check
  protects against cross-site request forgery.
- **Rate limiting** on sign-in attempts, per account and per source address.
- **Audit log** entries for sign-ins (successful and failed), token creation
  and use, host enrollment, and host removal.
- In phase 1, all users are administrators. Roles are deferred.
- Passkeys (WebAuthn) and single sign-on (OIDC) are planned for later phases.

### 8.1 First-run setup

A newly deployed portal has no users. To prevent someone else from claiming an
internet-facing portal first:

1. On startup with no users, the portal generates a one-time setup code and
   prints it to its log.
2. The web UI shows a setup page that requires this code to create the first
   administrator.
3. The code is invalidated once the first administrator exists.

## 9. Portal storage

SQLite, in the portal's data directory:

| Table | Contents |
| --- | --- |
| `users` | Username, password hash, TOTP secret (encrypted), recovery code hashes |
| `sessions` | Session ID hash, user, expiry |
| `enrollment_tokens` | Token ID, secret hash, expiry, used/revoked timestamps, creator |
| `hosts` | Host ID, name, machine ID, WireGuard public key, tunnel address, enrolled and last-seen timestamps |
| `audit_log` | Timestamp, actor, action, target, source address |

Secrets that must be recoverable (the portal's WireGuard private key, TOTP
secrets, and later DNS provider tokens) are encrypted with a key supplied by
file or environment variable and kept outside the database.

## 10. Deployment

The shipped Compose file runs two containers:

| Service | Published ports | Notes |
| --- | --- | --- |
| `caddy` | TCP 80, 443 | Automatic HTTPS; proxies to the portal's public listener. |
| `portal` | UDP 51820 | WireGuard; data directory on a volume. |

Portal settings needed for enrollment:

- Public URL (for example, `https://portal.example.com`), embedded in tokens.
- Public WireGuard endpoint (for example, `portal.example.com:51820`), sent to
  clients at enrollment.
- Tunnel address range (optional).
- TLS pin (optional; see section 3).

Users with an existing reverse proxy can remove Caddy and proxy to the portal's
public listener themselves.

## 11. Tooling

- Protobuf schemas live in `proto/` and are managed with
  [Buf](https://buf.build/).
- Code generation: `protoc-gen-go` and `protoc-gen-connect-go` for Go, and
  `protoc-gen-es` for the TypeScript web UI.
- Generated code is committed so builds don't require the generators.
- Generator versions are pinned in `mise.toml` alongside the rest of the
  toolchain.

## 12. Phase 1 scope

In scope:

- First-run setup, local accounts with TOTP, sessions, and the audit log.
- Enrollment tokens: create, list, revoke.
- `ezdr enroll`, `ezdr unenroll`, and `ezdr status`.
- The install script and `.deb` packages published in GitHub releases.
- The **Add host** dialog with both install commands.
- The portal's userspace WireGuard, the client's `ezdr0` interface, and the
  `ezdr` systemd service.
- The `Subscribe` stream with heartbeats, `ReportStatus`, and online/offline
  status in the web UI.
- Host list and host removal in the web UI.
- The Compose file with Caddy.

Out of scope (later phases): inventory, desired state beyond an empty
configuration, actions, WireGuard key rotation, roles, passkeys, and single
sign-on.
