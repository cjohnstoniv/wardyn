# Contributing to Wardyn

Thank you for your interest in contributing to Wardyn, the open-source governance control plane for coding agents. This document outlines the process and requirements for contributing.

## Developer Certificate of Origin

Every commit must carry a sign-off certifying the
[Developer Certificate of Origin 1.1](https://developercertificate.org/) — that you
wrote the contribution or have the right to submit it, and that it is public and
kept indefinitely. CI enforces the line:

```
Signed-off-by: Your Name <your.email@example.com>
```

`git commit -s` adds it for you.

## Code of Conduct

See [CODE_OF_CONDUCT.md](./CODE_OF_CONDUCT.md).

## Licensing

All contributions to Wardyn are made under the Apache License 2.0. By contributing, you agree that your contributions will be licensed under this license. Inbound = outbound: Apache-2.0 in, Apache-2.0 out.

## Security Invariants

All contributors and subagents MUST preserve the six security invariants documented in [ARCHITECTURE.md](./ARCHITECTURE.md). These are non-negotiable and form the foundation of Wardyn's security model:

1. **Secrets never enter the sandbox** — Late binding via the broker; no secrets in env, disk, or args, except the named, bounded exceptions ARCHITECTURE.md invariant 1 enumerates (credentials that structurally cannot be proxy-injected). That enumeration is authoritative — do not restate a count here, it is what drifts.
2. **Approval mints the credential** — Credential scope is verified atomically in the same transaction.
3. **L0 structural egress** — Sandbox has no default route; only path out is wardyn-proxy.
4. **Per-run identity with full attribution** — Every token carries `sub`, `act`, and `sponsor`.
5. **Fail closed; never overclaim** — Drivers declare capabilities; policy refuses enforcement gaps.
6. **Audit is append-only and free** — Every event is recorded; SIEM export never paywalled.

## Conformance Gate

Features are not done until they pass the conformance suite (`test/conformance`) on the Docker target (the Kubernetes runner is **[v0.5+ — planned]** and has no conformance target yet; a driver-agnostic honesty stub keeps the contract enforced). Every pull request must pass these CI checks — they are a server-side merge block, not advice:

- `go build` and `go vet` — both plain and `-tags docker`
- Go unit suites with a coverage floor: `make cover-check` (enforces COVER_MIN=65 over the
  UNION of both shipped builds — tagless + `-tags docker`),
  `make test-report-docker` (fakeDocker), `make test-report-pg` (real Postgres)
- Conformance tests: Docker + the driver-agnostic stub (both blocking in CI)
- UI: `pnpm typecheck`, unit tests with coverage, `pnpm build`, and the Playwright e2e suite
- Docs: the mermaid diagram + label-truth gate (`make diagrams`)
- Deploy: `helm lint` + `helm template` render assertions, and
  `docker compose config` validation
- Supply chain: `govulncheck`, `staticcheck`, `gitleaks` (secret scan),
  `go-licenses` (dependency license check), and SPDX license headers
  (`make license-headers`)
- DCO: a `Signed-off-by` line on every commit (see Getting Started)

### Large files

`scripts/check-file-size.sh` (run by `make lint`) caps every new non-test `.go`
file, every `ui/src/**/*.{ts,tsx}` file, and every `scripts/*.sh` file at 1000
lines; `.golangci.yml` (funlen/gocyclo/gocognit/lll) caps function size and
complexity. Exceeding either needs an inline `//nolint` carrying a real reason —
or, for a whole file, an entry on the frozen allowlist inside
`scripts/check-file-size.sh`, which is the authoritative list. The allowlist is
frozen: listed files may shrink freely, but material growth fails the gate.

## Getting Started

1. Fork the repository
2. Create a feature branch: `git checkout -b feature/your-feature`
3. Make your changes, ensuring:
   - Code is idiomatic Go (sparse comments on constraints only)
   - Errors are wrapped with `%w`
   - No panics in library code
   - Security decisions fail closed
   - Audit events use dotted action names
4. Run the gate: `make ci` (the widest local gate — build, lint, Go tests, UI, diagrams, supply chain)
5. Commit with sign-off: `git commit -s`
6. Push and create a pull request

## Testing

`make ci` is the gate. `make test` alone runs just the Go suites:

```bash
make test
```

`make help` lists the common targets, including ones not covered here
(`make dev-pg`, the guided `make test-drive` walkthrough).

Docker-dependent tests live behind the `docker` build tag — plain `make test`
compiles them out entirely. To actually run them:

```bash
WARDYN_TEST_DOCKER=1 make test-docker   # go test -tags docker ./...
```

For Postgres-dependent tests, set `WARDYN_TEST_PG` to a valid DSN:

```bash
WARDYN_TEST_PG="postgres://user:pass@localhost/testdb" make test
```

## Web UI (`ui/`)

The UI is a React + Vite app with its own blocking CI jobs (typecheck, unit
tests with coverage, build, and a Playwright e2e suite) — a PR that touches
`ui/` must pass all of them. Locally:

```bash
cd ui && pnpm install --frozen-lockfile     # Node 22 + pnpm 9 (package.json pins packageManager)
make ui-typecheck         # tsc --noEmit
make ui-test              # vitest with coverage
make ui                   # production build (vite)
./scripts/run-ui-e2e.sh   # Playwright e2e (starts Postgres + wardynd, mocked model)
```

UI visibly changed? Run `make screenshots` and commit the updated `docs/img` PNGs.

## Questions?

Open an issue or reach out to the maintainers. We're here to help!
