# Contributing to Wardyn

Thank you for your interest in contributing to Wardyn, the open-source governance control plane for coding agents. This document outlines the process and requirements for contributing.

## Developer Certificate of Origin

By contributing to Wardyn, you certify that:

1. The contribution was created in whole or in part by you and you have the right to submit it under the open source license indicated in the file; or
2. The contribution is based upon previous work that, to the best of your knowledge, is covered under an appropriate open source license and I have the right under that license to submit that work with modifications, whether created in whole or in part by you, under the same open source license (unless you are permitted to submit under a different license), as indicated in the file; or
3. The contribution was provided directly to you by some other person who certified (1), (2) or (3) and you have not modified it.
4. You understand and agree that this project and the contribution are public and that a record of the contribution (including all personal information you submit with it) is maintained indefinitely and may be redistributed consistent with this project's license(s) or the open source licenses it includes.

You must sign off every commit with:

```
Signed-off-by: Your Name <your.email@example.com>
```

Using the `-s` flag with `git commit` will automatically add this line.

### Full DCO 1.1 Text

```
Developer's Certificate of Origin 1.1

By making a contribution to this project, I certify that:

(a) The contribution was created in whole or in part by me and I
    have the right to submit it under the open source license
    indicated in the file; or

(b) The contribution is based upon previous work that, to the best
    of my knowledge, is covered under an appropriate open source
    license and I have the right under that license to submit that
    work with modifications, whether created in whole or in part
    by me, under the same open source license (unless I am
    permitted to submit under a different license), as indicated
    in the file; or

(c) The contribution was provided directly to me by some other
    person who certified (a), (b) or (c) and I have not modified
    it.

(d) I understand and agree that this project and the contribution
    are public and that a record of the contribution (including all
    personal information I submit with it) is maintained indefinitely
    and may be redistributed consistent with this project's license(s)
    or the open source licenses it includes.
```

## Code of Conduct

Contributors are expected to behave professionally and respectfully. Harassment and discriminatory behavior will not be tolerated.

## Licensing

All contributions to Wardyn are made under the Apache License 2.0. By contributing, you agree that your contributions will be licensed under this license. Inbound = outbound: Apache-2.0 in, Apache-2.0 out.

## Security Invariants

All contributors and subagents MUST preserve the six security invariants documented in [ARCHITECTURE.md](./ARCHITECTURE.md). These are non-negotiable and form the foundation of Wardyn's security model:

1. **Secrets never enter the sandbox** — Late binding via the broker; no secrets in env, disk, or args, with two named, bounded exceptions: a resident `ssh_key` grant (wiped after clone) and Bedrock access-key mode's `aws-*` env vars (SigV4 signing can't be proxy-injected) — see ARCHITECTURE.md invariant 1.
2. **Approval mints the credential** — Credential scope is verified atomically in the same transaction.
3. **L0 structural egress** — Sandbox has no default route; only path out is wardyn-proxy.
4. **Per-run identity with full attribution** — Every token carries `sub`, `act`, and `sponsor`.
5. **Fail closed; never overclaim** — Drivers declare capabilities; policy refuses enforcement gaps.
6. **Audit is append-only and free** — Every event is recorded; SIEM export never paywalled.

## Conformance Gate

Features are not done until they pass the conformance suite (`test/conformance`) on the Docker target (the Kubernetes runner is **[v0.5 — planned]** and has no conformance target yet; a driver-agnostic honesty stub keeps the contract enforced). All pull requests must pass CI checks — these **block merge**:

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

## Getting Started

1. Fork the repository
2. Create a feature branch: `git checkout -b feature/your-feature`
3. Make your changes, ensuring:
   - Code is idiomatic Go (sparse comments on constraints only)
   - Errors are wrapped with `%w`
   - No panics in library code
   - Security decisions fail closed
   - Audit events use dotted action names
4. Run tests: `make test`
5. Commit with sign-off: `git commit -s`
6. Push and create a pull request

## Testing

Run the full test suite locally:

```bash
make test
```

`make help` lists every target, including the ones not covered here (the
live e2e harnesses, `make dev-pg`, the guided `make test-drive` walkthrough).

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
