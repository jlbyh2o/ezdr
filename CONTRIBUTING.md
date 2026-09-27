# Contributing to EZDR

Thanks for your interest in EZDR. The project is in its early stages, so the
design is still moving; opening an issue to discuss a change before starting
work is the best way to avoid wasted effort.

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

For web UI development, run the portal (`go run ./cmd/ezdr-portal`) and, in
another terminal, `pnpm dev` in `web/`. The Vite dev server forwards `/api`
requests to the portal on port 8080.

The Go code builds and tests without Node.js: without the `webui` build tag,
the portal serves a placeholder page instead of the real UI.

## Repository layout

| Path | Contents |
| --- | --- |
| `cmd/ezdr/` | Client and command-line interface for Proxmox VE hosts |
| `cmd/ezdr-portal/` | Web portal server |
| `internal/` | Shared Go packages |
| `web/` | Portal web UI (React, TypeScript, Vite) |
| `deploy/` | Container image and Compose file for the portal |
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
