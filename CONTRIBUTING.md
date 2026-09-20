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

Features are not done until they pass the conformance suite (`test/conformance`) on the Docker target and, for anything the Kubernetes runner supports, the `conformance-k8s` CI job (needs a local `kind` cluster to run outside CI — see RELEASING.md; a driver-agnostic honesty stub keeps the contract enforced everywhere else). Every pull request runs these CI checks. A **subset** of them is a server-side merge block on `main`; branch protection is the source of truth, not this list — read it back with `gh api repos/cjohnstoniv/wardyn/branches/main/protection --jq .required_status_checks.contexts`. The rest are the review bar, and a red one is still a red one:

- `go build` and `go vet` — both plain and `-tags docker`
- Go unit suites with a coverage floor: `make cover-check` (enforces COVER_MIN=78 over the
  UNION of all three shipped builds — tagless + `-tags docker` + `-tags k8s`),
  `make test-report-docker` (fakeDocker), `make test-report-k8s`,
  `make test-report-pg` (real Postgres), and `make test-race-pg` — the race
  pass over the Postgres-gated concurrency proofs, which `make test-race` cannot
  reach because it strips `WARDYN_TEST_PG`
- Conformance tests: Docker + the driver-agnostic stub (blocking in CI), plus
  `conformance-k8s` for the Kubernetes runner
- UI: `pnpm typecheck`, unit tests with coverage, `pnpm build`, and the Playwright e2e suite
- Docs: the mermaid diagram + label-truth gate (`make diagrams`)
- Deploy: `helm lint` + `helm template` render assertions over the default AND
  `ci/all-on-values.yaml` value sets, `docker compose config` validation, and
  `helm-install-test` (kind cluster: postgres + helm install + `/healthz`)
- Supply chain: `govulncheck`, `staticcheck`, `gitleaks` (secret scan),
  `go-licenses` (dependency license check), and SPDX license headers
  (`make license-headers`)
- DCO: a `Signed-off-by` line on every commit (CI's `dco` job checks every commit in the pushed range; a new branch or a force-push is checked at its head only, so sign locally rather than relying on the gate) (see Getting Started)

### Large files

`scripts/check-file-size.sh` (run by `make lint`) caps every new non-test `.go`
file, every `ui/src/**/*.{ts,tsx}` file, and every `scripts/*.sh` file at 1000
lines; `.golangci.yml` (funlen/gocyclo/gocognit/lll) caps function size and
complexity. Exceeding either needs an inline `//nolint` carrying a real reason —
or, for a whole file, an entry on the frozen allowlist inside
`scripts/check-file-size.sh`, which is the authoritative list. The allowlist is
frozen: listed files may shrink freely, but material growth fails the gate.

## Building from source

This is the CONTRIBUTOR path. If you only want to *run* Wardyn, don't build it —
`install.sh` and the Helm chart both pull published, signed images and need no
clone at all. See the README's Install section.

```sh
git clone https://github.com/cjohnstoniv/wardyn && cd wardyn
make setup                     # asks containerized vs host; PULLS published images
WARDYN_BUILD_LOCAL=1 make setup  # builds every image from THIS tree instead
```

`WARDYN_BUILD_LOCAL=1` is what you want while working on the daemon, the proxy or
the UI — without it `make setup` pulls the released images and you would be
running someone else's binary against your own source.

Individual pieces: `make build` (Go binaries), `make ui` (the console bundle),
`make agent-images` (the agent images, including `agent-claude-code`, which is
deliberately never published because it bundles a proprietary vendor CLI — see
`deploy/images/THIRD-PARTY-TERMS.md`).

## Branching, issues and pull requests

Wardyn has users. Every change is planned as an issue, delivered as a pull
request, and reviewed before it reaches `main`.

### Issues first

- Every change starts as an issue on a milestone (`0.8.0`, `0.8.1`, …). A large
  feature is an **epic** issue whose task list links the issues that deliver it.
- A maintainer approves an issue by adding the `approved` label. Work on an
  unapproved issue is not merged.
- Questions about scope or design are asked on the epic, so the ruling is
  recorded where the work is.
- Labels: `kind/*` (`feature`, `bug`, `docs`, `chore`, `security`, `test`) ·
  `area/*` · `epic` · `approved` · `needs-mock` · `needs-decision` ·
  `security-review` · `blocked` · `deferred`.

### Branches

- Branch from `main`, keep it short-lived, name it `<kind>/<issue#>-<slug>`:
  `feat/57-push-content-rules-deny`, `fix/123-comparable-mount`,
  `docs/73-working-practice`.
- `main` is always releasable and is protected: required CI contexts plus a
  review. Nobody pushes to it directly except the maintainer's release commit
  (see [RELEASING.md](./RELEASING.md)).
- Dependent work stacks: branch from the previous PR's branch, write
  `Depends on #N` in the PR body, retarget to `main` after #N merges.
- A database migration takes the next free number at rebase time, never a
  number reserved in advance.

### Pull requests

- **One issue per PR.** Mixing is allowed only for tightly coupled changes that
  cannot be reviewed apart — at most three issues, each named in the body.
- Title in conventional-commit form (`feat(broker): …`, `fix(console): …`,
  `docs: …`, `chore(deps): …`). The body links the issue: `Closes #N` on the PR
  that finishes it, `Refs #N` on the others.
- Every commit is DCO-signed (`git commit -s`) and authored by the person who
  submits it.
- Docs land in the same PR as the code they describe: the CHANGELOG
  `[Unreleased]` entry, `docs/AUDIT-ACTIONS.md` rows for new audit actions,
  `docs/ENV.md` rows for new variables.
- Console changes: a mock or canon round precedes implementation unless a
  maintainer waives it on the issue (`needs-mock`), and every solidified UI path
  gets a Playwright pin (`scripts/run-ui-e2e.sh <spec>`).
- Quote the check you ran in the PR body — the command and the test names it
  executed (`go test -count=1 -v …`), not a summary line.
- Merge when the required CI contexts are green and a maintainer has approved;
  a `security-review` PR gets that review before approval. Multi-commit feature
  PRs merge with a merge commit so the reviewed commits stay in history;
  single-commit chores squash.

### What "done" means

An issue closes when its PR merged with the check it promised, its docs in the
same PR, and — for anything a live gate covers (conformance on both substrates,
the kind SSO walk, the Playwright suite) — that gate quoted in the PR body.

## Getting Started

1. Fork the repository
2. Pick an `approved` issue, or file one and wait for the label
3. Create a branch: `git checkout -b <kind>/<issue#>-<slug>`
4. Make your changes, ensuring:
   - Code is idiomatic Go (sparse comments on constraints only)
   - Errors are wrapped with `%w`
   - No panics in library code
   - Security decisions fail closed
   - Audit events use dotted action names
5. Run the gate: `make ci` (the daemon-free merge gate, and the widest gate a contributor needs — build, lint, Go tests, UI, diagrams, supply chain). Maintainers run the strict superset `make release-check` before a tag.
6. Commit with sign-off: `git commit -s`
7. Push and open a pull request that closes the issue

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

For Postgres-dependent tests, set `WARDYN_TEST_PG` to a valid DSN and use the
PG-lane targets — `test`, `test-docker` and `test-race` all strip
`WARDYN_TEST_PG` on their own recipe line, so passing it there runs the PG
lane disabled and green rather than failing loudly:

```bash
WARDYN_TEST_PG="postgres://user:pass@localhost/testdb" make test-report-pg
WARDYN_TEST_PG="postgres://user:pass@localhost/testdb" make test-race-pg
```

Kubernetes conformance runs in `.github/workflows/ci.yml`'s `conformance-k8s`
job against kind with Calico; see that job's setup for a local run. The additional
cluster walks, including `scripts/run-e2e-ssh-k8s.sh` and the **AWS SSO walk**,
are manual proofs against `make kind-quickstart` and self-skip without
`WARDYN_TEST_K8S=1`. No workflow runs the AWS SSO walk,
`scripts/kind-sso-walk.sh`: with `make kind-sso` it stands up Dex with two
principals and an on-cluster fake of AWS IAM Identity Center, then proves that a
MEMBER signing in from their own seat gets a credential that is theirs and not
the admin's, and that their Bedrock run's role credentials were minted for their
own pinned account and role. It needs no AWS account. The fake is reached
through the gated `WARDYN_AWS_SSO_ENDPOINT_OVERRIDE` test hatch — read
`threatmodel/THREAT-MODEL.md` residual #45 before setting it anywhere real, and
see docs/OPERATIONS.md, "Testing AWS SSO without an AWS tenant", for the recipe
and the one precondition (`internal_hosts`) that fails first if you skip it.

## Web UI (`ui/`)

The UI is a React + Vite app with its own blocking CI jobs (typecheck, unit
tests with coverage, build, and a Playwright e2e suite) — a PR that touches
`ui/` must pass all of them. Run these commands from the repository root:

```bash
(cd ui && pnpm install --frozen-lockfile)   # Node 22 + pnpm 9 (package.json pins packageManager)
(cd ui && pnpm exec playwright install chromium) # once; run-ui-e2e.sh also needs jq on PATH
make ui-typecheck         # tsc --noEmit
make ui-test              # vitest with coverage
make ui                   # production build (vite)
./scripts/run-ui-e2e.sh   # Playwright e2e (starts Postgres + wardynd; seeds no model credential)
```

UI visibly changed? Run `make screenshots` and commit the updated `docs/img` PNGs.

## Questions?

Open an issue or reach out to the maintainers. We're here to help!
