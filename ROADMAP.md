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
| **v0.5** | Kubernetes runner substrate + the Helm chart's first sandbox-capable deploy, conformance green on a real cluster, native SSH access into a run, real admin/member RBAC with owner scoping, signed+published release images (below) | **Merged, unreleased (pre-alpha)** — code-complete and CI-green on this branch; no `vX.Y.Z` tag cut yet (0.4.5 is the last version with a written CHANGELOG entry, itself still untagged — see [CHANGELOG.md](CHANGELOG.md)) |

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
  enforce CPU/memory/pid caps does not start the sandbox — the gate reads the
  `ContainerCreate` warnings between create and start, which also removed a
  false-positive refusal on rootless Podman. It fails closed by default; the one
  way past it is the explicit `WARDYN_ALLOW_UNENFORCEABLE_CAPS=1` opt-out, which
  downgrades the refusal to a warning on a trusted host.
- **Rootless Podman is probed, not assumed.** `scripts/test-podman.sh` was run
  against rootless Podman 4.9.3: the runner-critical primitives hold at CC1;
  CC2/CC3 are refused fail-closed (gVisor and Kata need what rootless does not give).

### What v0.5 shipped

- **Kubernetes runner substrate.** `internal/runner/k8s` (`-tags k8s`,
  `WARDYN_RUNNER=k8s`) creates/manages sandboxes as pods instead of Docker
  containers — a second, independent confinement substrate behind the same
  `substrate.Substrate` seam the Docker driver uses (see
  [docs/PLUGGABILITY.md](docs/PLUGGABILITY.md)). It is **L1** (NetworkPolicy-
  enforced), not L0 (structural/gatewayless) like Docker: a boot-time
  two-phase egress canary proves the cluster's CNI actually enforces
  `NetworkPolicy` before wardynd will start the substrate at all, and refuses
  to boot otherwise (`WARDYN_K8S_ALLOW_UNENFORCED_NETPOL=1` is the documented,
  loud opt-out — every sandbox then has unconfined egress). CC1-only until an
  operator pins `WARDYN_CONFINEMENT_MAP`/the chart's `k8s.runtimeClasses` to a
  registered RuntimeClass for CC2/CC3. The substrate is **not** at parity
  with Docker yet — see [deploy/helm/wardyn/README.md](deploy/helm/wardyn/README.md)'s
  and [docs/OPERATIONS.md](docs/OPERATIONS.md)'s "Kubernetes: known gaps"
  sections for the honest, code-checked list (no BYOI/devcontainer builds, no
  `local_dir` mounts, no per-pod PIDs/disk enforcement, no ground-truth
  correlator).
- **The Helm chart creates sandboxes now, not just the control plane.**
  `deploy/helm/wardyn` with `k8s.enabled=true` wires the substrate above into
  a real install: least-privilege Role/RoleBinding + ClusterRole for the
  substrate's own API-server calls, a default-deny `NetworkPolicy` (re-opening
  DNS, Postgres, HTTP/SSH ingress, and — only when `k8s.enabled` — apiserver
  egress and a runs-namespace peer), and a `k8s_egress_containment` setup
  check the console surfaces (Enforcing / Not enforcing / Indeterminate). See
  the `wardyn-k8s-setup` Claude Code skill for the full cluster-prereqs
  → values → install → verify recipe, including wiring Entra ID App Roles.
- **Conformance now runs on a real cluster, not just Docker.** CI's
  `conformance-k8s` job proves the k8s substrate on kind with a
  NetworkPolicy-enforcing CNI (`disableDefaultCNI` + a pinned Calico
  manifest — kind's default `kindnet` does not enforce policy, which is
  exactly the negative case the substrate's own canary is designed to
  refuse). The shared conformance suite's one L0-specific case self-skips on
  this target by design (the k8s substrate claims L1, not L0); a dedicated
  L1 case proves what it actually claims instead. See
  [ARCHITECTURE.md](ARCHITECTURE.md)'s "Parity rule".
- **Native SSH into a running sandbox.** `wardynd` can serve `ssh
  <run-id>@host` directly into the same tmux session the web terminal
  attaches to — exec, sftp, and `-L` port forwarding (destination restricted
  to the sandbox's own loopback), each with its own audit action
  (`ssh.exec`/`ssh.sftp`/`ssh.forward`) and the shell path recorded exactly
  like the browser terminal. Registered-public-key auth only, owner-only
  authorization (no operator override yet — see "Named gaps" below). Off by
  default (`WARDYN_SSH_LISTEN` unset = no listener, no host key even
  generated). See [docs/SSH.md](docs/SSH.md).
- **Authorization/RBAC + owner scoping.** Real `admin`/`member` roles, derived
  at OIDC login from Entra App Roles/groups/email (`WARDYN_OIDC_ROLE_MAP`) or
  the legacy `WARDYN_OIDC_OPERATOR_EMAILS` allowlist (still honored,
  additive). A member is scoped to their own runs/approvals/audit rows with
  owner-or-admin gating (byte-identical 404 on a foreign resource — no
  existence oracle), may decide only `egress_domain` approvals on runs they
  own (`credential`/`tool_call` stay admin-only regardless of ownership), has
  their own `inline_policy` clamped to the operator's default-policy ceiling,
  and cannot bring a custom sandbox image or devcontainer repo. The admin
  token and local mode remain always-admin — one shared credential, no
  per-human identity to key a role off. Full detail:
  [docs/OPERATIONS.md "Multi-user: who can change what"](docs/OPERATIONS.md#multi-user-who-can-change-what).
- **Member console.** The web console is role- and kind-aware: a member's nav
  hides operator-only surfaces (policy/workspace/secret CRUD, BYOI), and
  approvals render per-kind (egress-domain approvals a member can act on,
  credential/tool_call ones they can only view).
- **Signed, published release images.** `.github/workflows/release.yml`
  builds and publishes the four images a release ships (`wardynd`,
  `wardyn-proxy`, `agent-claude-code`, `agent-codex-cli`) to
  `ghcr.io/cjohnstoniv/<name>` on a `vX.Y.Z` tag push, cosign-signs each
  keylessly (Fulcio/Rekor via the Actions OIDC token — no long-lived signing
  key to manage), and attaches a CycloneDX SBOM release asset (`make sbom`).
  linux/amd64 only today.

## Planned

Everything below is **planned, unbuilt, and undated**. Where a seam exists but no
implementation does, [docs/PLUGGABILITY.md](docs/PLUGGABILITY.md) says so per row.

| Milestone | Scope |
|---|---|
| **v1.0** | SPIRE identity provider (the `identity.Provider` seam ships; the SPIRE impl does not) · OpenBao secret store (same, for `secretstore.Store`) · L3 MCP/tool gateway · arbitrary-domain L2 TLS interception (targeted LLM/registry MITM already ships, opt-in) · cloud STS federation · OTLP/OCSF SIEM sinks (file/webhook/syslog sinks already ship) · Docker/Compose L1 default-deny via nftables (the k8s target's L1 already ships — NetworkPolicy, boot-time-canary-enforced, blocking `169.254.169.254`; Docker/Compose still relies on L0 structural confinement alone) · HA completion — closing the still-open per-process blockers a second replica hits (chiefly the in-memory, fail-open secret-masking registry; see [docs/OPERATIONS.md](docs/OPERATIONS.md)'s "One replica, by construction" for the exact list and what v0.5 already closed) · k8s substrate parity with Docker: BYOI/devcontainer builds, `local_dir` mounts, per-pod PIDs/disk enforcement, and a k8s ground-truth correlator (see [deploy/helm/wardyn/README.md](deploy/helm/wardyn/README.md)'s "Known gaps") · CC3/Vault (Kata) packaged and GA — experimental today · Cilium `toFQDNs` · hash-chained audit + signed action receipts · separation of duty on the control plane |
| **v1.0 (git-token ref confinement)** | **Token-side** branch-namespace confinement for minted git tokens — the proxy-side push-ref check ships DEFAULT-ON (`agent-run` names the run branch `wardyn/<run-id>/work`; `WARDYN_GIT_BROKER_ENFORCE_BRANCH_NS=false` opts out) and binds the brokered App lane, but the installation token itself cannot self-restrict to a ref prefix. What now ships, opt-in: Wardyn reads a GitHub repository ruleset back (`VerifyRefRuleset`, `internal/broker/ruleset.go`), grades it on the setup checklist (never `fail`), and can refuse every `github_token` mint until one verifies (`WARDYN_GITHUB_REQUIRE_REF_RULESET`, default off). What's still not built: Wardyn never creates or holds the ruleset itself — that needs repo-admin access it deliberately does not request, so creating one stays a manual operator step (`docs/POLICIES.md`) — and the gate defaults off, so an operator who does neither still has an unbound token. `git_pat`/`ssh_key` remain outside any receive-pack parser regardless of the ruleset (`threatmodel/THREAT-MODEL.md` asset #4) |

### Named gaps without a milestone

These are known, documented ceilings. They are listed so they are not mistaken for
shipped behavior; none is scheduled.

- **Never-resident Azure DevOps git egress.** Designed, not built. ADO works today
  through the `git_pat` grant, on which the PAT *is* resident in the sandbox. The
  ceiling: ADO has no token-minting API, so the operator PAT's scope is the boundary
  — never-resident is achievable, per-repo auto-expiring scoping is not.
- **Proxy-side injection of the Bedrock SSO bearer.** Would make the SSO token
  never-resident; the derived role credentials stay resident regardless, because
  SigV4 signs in-process.
- **No grant delivers a secret as a sandbox env var.** The five grant kinds wire
  git's credential helper, inject proxy-side, or write a key file — none puts a
  value in the sandbox environment, so a PAT-authenticated CLI or REST tool
  cannot be brokered at all. Pasting the token into an interactive terminal is
  the only workaround, which does not survive an autonomous run.
  ([field report](docs/adoption/corp-network-onboarding-findings.md))
- **A `git_pat` approval is single-use, so there is no approve-once-per-sandbox.**
  The helper is standing but the mint is not: an approval-gated pull-then-push
  raises two approvals, while `requires_approval: false` auto-issues a real
  personal credential for the whole session. There is no per-run lease in
  between. ([field report](docs/adoption/corp-network-onboarding-findings.md))
- **The `ssh_key` grant is clone-only and does not fit a bind-mounted workspace.**
  The key is written just before the clone and wiped right after, so a `local_dir`
  workspace — which has no clone step — leaves interactive SSH pull/push
  unauthenticated. Rewrite the remote to HTTPS, or accept clone-only SSH.
  ([field report](docs/adoption/corp-network-onboarding-findings.md))
- **Team mode as a packaged, sealed multi-user product** — as opposed to the
  RBAC that ships IN the control plane today (see
  [docs/OPERATIONS.md](docs/OPERATIONS.md)'s "Multi-user: who can change what").
  Admin/member roles and owner scoping are real and shipped (v0.5); what's still
  speculative, no design in the tree: SAML/SCIM provisioning, an
  organization/tenant structure, per-user API tokens (today's only credentials
  are the shared admin bearer token or an OIDC session — no personal,
  independently-revocable API token a member could hand to a script), and
  CUSTOM roles beyond admin/member. The admin token and local mode remain the
  same shared credential they always were: always-admin, no per-human identity,
  no separation of duty from a real admin user (v1.0's row, above).
- **The SSH gateway has no admin/operator override.** SSH authorization is a
  single `run.created_by == the key's registered principal` check — narrower
  than the web terminal's `requireOperator` gate, which lets an admin attach to
  ANY run. An admin who needs another human's run over SSH has no path there
  today; they use the web terminal, same as a member would. The fix is a role
  column an operator's own registered key could satisfy alongside "owner" —
  marked with a `ponytail:` comment at the check itself
  (`internal/api/sshgateway.go`'s `sshAuth`) rather than built, since no
  deployment has asked for it and the narrower behavior is safe by
  construction, not merely unfinished (`threatmodel/THREAT-MODEL.md`
  residual #15).
- **The legacy `sources`/`base_image` workspace columns have no drop date, and
  the migration number reserved for it is gone.** 0.4.5's source-library split
  (migration `0031_source_library.sql`) kept the old embedded columns live for
  compatibility and reserved migration number `0032` in its own comment for
  the column-drop migration, "which must ship in a LATER release, never this
  one." That number is now taken — `0032_ssh_public_keys.sql` shipped in v0.5
  — so the eventual drop migration needs a fresh number whenever it's
  scheduled. Nothing depends on it happening by any particular release; it's
  listed here so the stale "is 0032" comment in `0031_source_library.sql`
  isn't mistaken for a live plan.
- **Age-key rotation for the secret store.** One age identity binds both
  encryption and decryption (`internal/secretstore/pg`); nothing re-encrypts
  stored secrets under a new key, and there is no `wardyn secret rotate`.
  Changing `WARDYN_AGE_KEY` strands every existing ciphertext — see
  [docs/OPERATIONS.md](docs/OPERATIONS.md)'s "The age key has no rotation path".
- **react-router 7 → 8 major bump.** A per-advisory pnpm-audit suppression
  covers GHSA-qwww-vcr4-c8h2 until then — needs a UI owner.
- **Kata/TPROXY/io_uring composer quick-hits.** Parked since the
  composer-readiness work.

## What is not on the roadmap

- **Bring-your-own arbitrary Kubernetes manifests.** One blessed Helm chart, or nothing.
- **A feature that passes on only one target.** The parity rule: a feature is not
  done until it passes the conformance suite on both Docker and kind — CI now
  gates on both, but that is a floor, not a one-time proof (see
  [ARCHITECTURE.md](ARCHITECTURE.md)'s "Parity rule"). The k8s substrate is not
  at overall feature parity with Docker yet even though it passes conformance
  (see [deploy/helm/wardyn/README.md](deploy/helm/wardyn/README.md)'s "Known
  gaps"); a new feature still owes both targets, or an honest, explicit skip.
- **An `enterprise/` directory.** Apache-2.0 everything.
