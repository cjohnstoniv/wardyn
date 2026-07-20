# Wardyn roadmap

Wardyn is **pre-alpha**. Interfaces are not stable, there is no semantic-versioning
promise, and nothing here is a date commitment — the planned rows are ordered by
intent, not by schedule.

This file is the single forward-looking source of truth. Per-release detail lives in
[CHANGELOG.md](CHANGELOG.md); per-seam detail (which pluggable implementations exist
versus which are only an interface) lives in [docs/PLUGGABILITY.md](docs/PLUGGABILITY.md).

## Shipped

| Milestone | Highlights | Status |
|---|---|---|
| **v0.1** | Per-run identity (embedded provider), approval FSM, credential broker, L2 egress proxy, append-only Postgres audit + PTY replay, CC1/CC2 confinement gating, Compose deploy | **Shipped (pre-alpha)** |
| **v0.2** | Open-source pilot bar (Docker-only): secret-output masking, eBPF/Tetragon ground-truth audit stream, pinned seccomp + AppArmor, interactive attach sessions, policy CRUD, run-completion state, control-plane TLS, real conformance gate + supply-chain CI | **Shipped (pre-alpha)** |
| **v0.3** | CI mode (BYOA): headless pipeline launches with no pre-running control plane — `wardyn run --wait` (outcome exit codes), `--image` (bring-your-own container, wrapped + governed), `task_mode: exec` (plain commands, no agent/LLM), one-shot `scripts/ci-run.sh`, GitHub Actions / Azure DevOps examples ([docs/CI.md](docs/CI.md)) | **Shipped (pre-alpha)** |
| **v0.3.1** | Repo-scoped git egress via the proxy-side git-broker (`/wardyn/gh/<org>/<repo>`; `github.com` leaves the allowlist), Getting Started demos, container login for a Claude subscription (`claude setup-token` captured in a sandbox), paginated list endpoints (`limit`/`offset` + `X-Wardyn-Truncated`), SDK route-family coverage, mobile console navigation, [docs/ENV.md](docs/ENV.md) | **Shipped (pre-alpha)** |
| **v0.4** | Containerized setup as the default, credential CLI, YAML policies, container workspaces with their own model credentials, Bedrock SSO, and the corporate-network build/egress lanes (below) | **Shipped (pre-alpha)** |

### What v0.4 shipped

- **Containerized setup is the default.** `make setup` brings up the compose stack;
  host mode is an advanced escape hatch (`WARDYN_SETUP_MODE=local`). The console
  gates a *new* install behind Getting Started, and the model/harness step is
  harness-first and skippable (only the sandbox barrier is required).
- **Credentials are first-class at the CLI.** `wardyn subscription
  connect|status|disconnect` (stdin only, age-encrypted, injected proxy-side) and
  `wardyn setup status`, which prints the exact next command per unmet check.
  `WARDYN_SUBSCRIPTION_TOKEN` seeds a subscription headlessly.
- **YAML policies.** `--policy-file` and `policy create|update -f` accept YAML or
  JSON; `wardyn policy render -f <file>` converts and strictly validates. See
  [`examples/policies/sandbox.yaml`](examples/policies/sandbox.yaml) and
  [`examples/policies/sandbox-workspace.yaml`](examples/policies/sandbox-workspace.yaml).
- **A container can be a workspace.** A workspace may be a container image, and any
  workspace can carry an operator-owned model/harness credential binding (managed,
  API key, or Bedrock — names and refs only) via `PUT /workspaces/{id}/llm-cred`.
  A run inherits the binding of the workspace it picks.
- **Bedrock via AWS SSO.** A containerized device-code login (`deploy/images/aws-sso`,
  `cmd/wardyn-aws-sso`) captures the SSO token and materializes a minimal synthetic
  `~/.aws`, so an SSO-only org needs neither a bearer key nor a host `~/.aws` mount.
  `WARDYN_BEDROCK_REGION`/`_AWS_PROFILE` fall back to `AWS_REGION`/`AWS_DEFAULT_REGION`/
  `AWS_PROFILE`. **Limitation:** the SSO token and the derived role credentials are
  **resident** in the sandbox — see the planned proxy-side injection below.
- **Corporate networks.** Corp CA + `NPM_REGISTRY`/`HTTP(S)_PROXY` threaded through
  every image build (plus a `WARDYN_UI_STAGE=ui-prebuilt` escape hatch for a mirror
  that cannot serve pnpm), an opt-in native agent-CLI install (`CLAUDE_INSTALL=native`,
  `CODEX_INSTALL=native`), a corp-aware `make doctor` preflight, and Rancher Desktop
  support.
- **Concurrent CI jobs on one host.** Every named compose object is scoped by
  `WARDYN_NS` and each `ci-run.sh` invocation gets its own `COMPOSE_PROJECT_NAME`,
  so one job's teardown no longer wipes another's stack (`make test-e2e-concurrent`).
  Bounded to one trusted operator (e.g. a CI fleet under one service account).
- **The resource-cap gate is authoritative and pre-launch.** A host that cannot
  enforce CPU/memory/pid caps never starts the sandbox — the gate reads the
  `ContainerCreate` warnings between create and start, which also removed a
  false-positive refusal on rootless Podman.
- **Rootless Podman is probed, not assumed.** `scripts/test-podman.sh` was run
  against rootless Podman 4.9.3: the runner-critical primitives hold at CC1;
  CC2/CC3 are refused fail-closed (gVisor and Kata need what rootless does not give).

## Planned

Everything below is **planned, unbuilt, and undated**. Where a seam exists but no
implementation does, [docs/PLUGGABILITY.md](docs/PLUGGABILITY.md) says so per row.

| Milestone | Scope |
|---|---|
| **v0.5** | SPIRE identity provider (the `identity.Provider` seam ships; the SPIRE impl does not) · OpenBao secret store (same, for `secretstore.Store`) · L1 default-deny (nftables / NetworkPolicy, blocking `169.254.169.254`) · L3 MCP/tool gateway · arbitrary-domain L2 TLS interception (targeted LLM/registry MITM already ships, opt-in) · Kubernetes runner driver + the Helm chart (`deploy/helm/wardyn/` is render-checked only today — it deploys the control plane but cannot create sandboxes) · cloud STS federation · OTLP/OCSF SIEM sinks (file/webhook/syslog sinks already ship) · signed image publishing, which turns CI-mode source builds into pulls and enables a reusable one-line GitHub Action ([docs/CI.md](docs/CI.md)) · branch-namespace enforcement on minted git tokens — today the broker only *records* `wardyn/<run-id>/*` as advisory metadata and the token can push to any branch within its granted repos (`threatmodel/THREAT-MODEL.md` asset #4) |
| **v1.0** | CC3/Vault (Kata) packaged and GA — experimental today · Cilium `toFQDNs` · hash-chained audit + signed action receipts · separation of duty on the control plane · the conformance suite green on both the Docker and Kubernetes targets |

### Named gaps without a milestone

These are known, documented ceilings. They are listed so they are not mistaken for
shipped behavior; none is scheduled.

- **Never-resident Azure DevOps git egress.** Designed, not built:
  [docs/ADO-GIT-BROKER.md](docs/ADO-GIT-BROKER.md). ADO works today through the
  `git_pat` grant, on which the PAT *is* resident in the sandbox. The ceiling is
  stated in that design: ADO has no token-minting API, so the operator PAT's scope
  is the boundary — never-resident is achievable, per-repo auto-expiring scoping is not.
- **Proxy-side injection of the Bedrock SSO bearer.** Would make the SSO token
  never-resident; the derived role credentials stay resident regardless, because
  SigV4 signs in-process.
- **Team mode (multi-user SSO/RBAC).** Speculative — no design in the tree. The
  console's "Sign in with SSO" button is disabled; single-operator admin-token auth
  is what exists. The `/auth/login` flow works server-side.

## What is not on the roadmap

- **Bring-your-own arbitrary Kubernetes manifests.** One blessed Helm chart, or nothing.
- **A feature that passes on only one target.** The parity rule: a feature is not
  done until it passes the conformance suite on both Docker and kind. Today only the
  Docker target runs functionally.
- **An `enterprise/` directory.** Apache-2.0 everything.
