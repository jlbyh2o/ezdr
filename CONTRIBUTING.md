# Contributing to EZDR

Thanks for your interest in EZDR. The project is in its early stages, so the
design is still moving; opening an issue to discuss a change before starting
work is the best way to avoid wasted effort. For questions and open-ended
ideas, use [Discussions](https://github.com/jlbyh2o/ezdr/discussions).

Everyone taking part is expected to follow the
[code of conduct](CODE_OF_CONDUCT.md).

## Developer Certificate of Origin

All commits must be signed off under the
[Developer Certificate of Origin](https://developercertificate.org/) (DCO). By
signing off, you certify that you wrote the change or otherwise have the right
to submit it under the project's license (AGPL-3.0).

Add the sign-off with `git commit -s`, which appends a line like:

```text
Signed-off-by: Your Name <you@example.com>
```

The name and email must match the commit author.

## Commit messages

Commits follow [Conventional Commits](https://www.conventionalcommits.org/):

```text
<type>(<optional scope>): <summary>
```

Common types: `feat`, `fix`, `docs`, `refactor`, `test`, `build`, `ci`,
`chore`. Scopes are optional; useful ones include `client`, `portal`, `web`,
and `docs`. Write the summary in the imperative mood, for example
`feat(client): report ZFS pool inventory`.

## Development setup

The toolchain (Go, Node.js, pnpm, and golangci-lint) is pinned in
[`mise.toml`](mise.toml). With [mise](https://mise.jdx.dev/) installed, run:

```sh
mise install
```

CI uses the same file, so update versions there rather than in workflows.

Without mise, install the versions listed in `mise.toml` manually. Docker is
optional and only needed to build the portal image.

Common tasks:

| Command | Description |
| --- | --- |
| `make build` | Build the client and the portal (with the web UI) into `bin/` |
| `make test` | Run Go tests |
| `make lint` | Run Go and web linters |
| `make fmt` | Format Go code |

For web UI development, run the development portal and, in another terminal,
the Vite dev server:

```sh
go run ./cmd/ezdr-devportal   # -reset starts over, -fast adds activity, -empty seeds nothing
cd web && pnpm dev
```

Then open <http://localhost:5173/dev/signin>, which signs you in without a
password. The development portal keeps its data in `.devportal/` and
simulates hosts in-process: on first start it seeds six hosts and six plans,
then brings the plans into different states through the real services (active,
lagging, paused, draft, failed over, and one running a test failover).
Simulated hosts replicate every minute and answer every action, so tests,
failovers, and failbacks can be run from the UI. It never contacts a real
host. The Vite dev server forwards API requests (`/ezdr.*`) and `/dev/` to it
on port 8080.

The Go code builds and tests without Node.js: without the `webui` build tag,
the portal serves a placeholder page instead of the real UI.

## Repository layout

| Path | Contents |
| --- | --- |
| `cmd/ezdr/` | Client and command-line interface for Proxmox VE hosts |
| `cmd/ezdr-portal/` | Web portal server |
| `cmd/ezdr-devportal/` | Portal with simulated hosts and fake data, for UI development |
| `internal/` | Shared Go packages |
| `web/` | Portal web UI (React, TypeScript, Vite) |
| `proto/` | Protocol Buffers definitions for the portal's APIs |
| `deploy/` | Container image and Compose files for the portal |
| `packaging/` | Client `.deb` scripts, systemd unit, and install script |
| `docs/` | Architecture and development documentation |

## Testing against Proxmox VE

Never develop or test EZDR against production hosts. See
[docs/development/test-lab.md](docs/development/test-lab.md) for a
recommended nested test lab.

## Style

- Use American English in code, comments, and documentation.
- Go code is formatted with `gofmt` and `goimports`; `make lint` must pass.

## Reporting security issues

Please don't open public issues for security vulnerabilities. See
[SECURITY.md](SECURITY.md).
