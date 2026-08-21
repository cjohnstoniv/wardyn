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
| **v0.5** | Kubernetes runner substrate + the Helm chart's first sandbox-capable deploy, conformance green on a real cluster, native SSH access into a run, real admin/member RBAC with owner scoping, signed+published release images (below) | **Shipped (pre-alpha)** — tagged `v0.5.0`, 2026-08-18 (see [CHANGELOG.md](CHANGELOG.md)) |
| **v0.6** | **The enterprise-POC base: cloud deployment + real permissioning.** Capability grants (four kinds, per-kind enforcement switches, IdP groups), Kubernetes as the base deployment story (one-command kind quickstart, day-2 ops, `/readyz`), terminals beyond the browser (`wardyn ssh`, a kind-proven SSH lane, an admin override), governed UI sandboxes (a ticket-gated loopback relay + a code-server image), and the ground-truth counter fix (below) | **Unreleased (pre-alpha)** — code-complete on `prep/v0.6`; not yet merged to main or tagged. The daemon-free merge gate (`make ci`) is green on this branch — all 22 targets, verified end to end 2026-08-20, gitleaks' full-history scan included; `make test-e2e` carries 10 failures that reproduce on a pre-merge baseline on the same host, so they are a pre-existing lane defect, not a 0.6 regression |

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
  like the browser terminal. Registered-public-key auth only, and owner-only
  authorization *at the time* — v0.6 added an admin override (see "What v0.6
  shipped" below, and "Named gaps" for the ceiling it carries). Off by
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
  key to manage), and publishes a CycloneDX SBOM (`make sbom`) as a
  downloadable workflow artifact (deliberately not auto-attached to the
  GitHub Release — attach it by hand if wanted). linux/amd64 only today.

### What v0.6 shipped

- **Permissioning: capability grants for a user, a group, or everyone.** RBAC
  grows past admin/member. An admin grants — or denies — one member, one IdP
  group (the union of the ID token's `roles` and `groups` claims, so Entra App
  Roles work as-is), or every signed-in human a capability on one of four kinds:
  `egress_host` (which hosts they may decide an `egress_domain` approval for,
  and which survive on their own `inline_policy`), `secret` (which stored
  secrets that policy may reference, and which names `GET /secrets` lists back),
  `workspace` (which onboarded workspace they may launch against), and `image`
  (which custom sandbox image they may name at all — the one kind that *widens*
  what a member can do; `devcontainer_repo` stays unconditionally admin-only,
  because it executes attacker-authored build configuration, which is not a
  thing to hand out one row at a time). Rows live in `capability_grants` with a
  per-kind switch in `capability_enforcement` (migration `0042`), managed
  through `GET /permissions`, `POST /permissions/grants`, `DELETE
  /permissions/grants/{id}` and `PUT /permissions/enforcement` (all admin-only),
  with `GET /me/capabilities` as the member-safe read of the caller's own
  effective set. Resolution is **deny beats allow beats the per-kind enforcement
  switch** (`capAllowed`, `internal/api/capabilities.go`) — deny sits above the
  switch deliberately, so a deny row bites on a kind nobody has enforced yet —
  with admins, the admin token, and local mode exempt, and no cache (a new grant
  applies on the next request). **Every switch ships off**: a deployment
  upgraded from 0.5 with no rows written behaves exactly as it did, which is
  what makes this adoptable one kind at a time. The doctrine — *a capability
  bounds what the MEMBER chose, never what the ADMIN pre-authorized* — is why a
  stored policy, a workspace's requirements, scan-seeded hosts, and the model
  provider's own egress are never narrowed. See
  [docs/OPERATIONS.md](docs/OPERATIONS.md)'s "Capabilities: what one member, or
  one group, may do".
- **Kubernetes became the base deployment story, not just a second substrate.**
  `make kind-quickstart` ([`deploy/kind/quickstart.sh`](deploy/kind/quickstart.sh))
  goes from a bare host to a real cluster install in one command — image build,
  a throwaway kind cluster on a NetworkPolicy-enforcing CNI, a Helm install with
  the k8s runner on — and prints the URL and admin token it just minted;
  `make kind-down` takes it away again. The front door now documents both lanes
  side by side: Compose via `make setup`, Kubernetes via the chart, whose
  quickstart sits above the full install walkthrough
  ([`deploy/helm/wardyn/README.md`](deploy/helm/wardyn/README.md)). Day-2
  operations are written from commands actually run against a live cluster
  ([docs/OPERATIONS.md](docs/OPERATIONS.md)). The install also gained the pieces
  a real deployment needs and 0.5 did not have: a `/readyz` endpoint, so the
  chart has a genuine readiness probe rather than a liveness check doing double
  duty (and the probe is pinnable); a `/metrics` that can see a dead store and a
  backed-up audit spool instead of reporting health it never checked; a refusal
  of the ephemeral-age-key trap, where a chart-generated age key would strand
  every stored secret on the next pod restart; an external-DSN install that no
  longer traps the operator in a crash-loop; and `k8s.enabled` refusing
  `serviceAccount.create=false` with no name to fall back to.
- **Terminals beyond the browser.** `wardyn ssh <run-id>` reaches a run over the
  v0.5 gateway with no attach flag to trip on, and `wardyn ssh --print` emits
  the raw command for a script or a demo. The lane is proven on the other
  substrate too — `make test-e2e-ssh-k8s` drives it against kind, not only
  Compose — and demo **V11 ("your terminal, our cluster")** films it end to end
  against the `kind-quickstart` cluster, graded against that install's own audit
  trail rather than an exit code. That beat and its grader are wired and were
  exercised on a live cluster once; the take itself is a release-cut act and is
  not published yet ([docs/DEMO-SCRIPT.md](docs/DEMO-SCRIPT.md)).
  The gateway also gained an **admin override**: a registered key now carries
  the role it was registered under (`role` column, migration
  `0043_ssh_key_role.sql`) and `sshAuth` authorizes `run.CreatedBy == the key's
  principal` **OR** `key.Role == admin`, with every override audited distinctly
  (`ssh.auth` success carries `override:true`). It is honestly weaker than the
  web terminal's live `requireOperator` gate — the role is a registration-time
  stamp, never re-read — and that ceiling is a named gap below, not an
  assumption quietly made. See [docs/SSH.md](docs/SSH.md).
- **UI sandboxes: a governed relay from the browser to one declared port inside
  a run.** A policy may declare `ui_apps` — a name, a loopback port and a path,
  operator-authored, never a command string — and `wardynd` relays exactly those
  ports over the **same exec lane** (`socat` on `Runner.ExecStream`) the SSH
  `-L` forward already uses: no pod/container-IP dial, no `NetworkPolicy`
  change, **no new network path out of the sandbox**. It is off by default and
  lives on a second listener at a second browser origin
  (`WARDYN_UI_SANDBOX_LISTEN`); boot refuses an address equal to `-listen`,
  because what the relay serves is the sandbox's own JavaScript and the separate
  origin is the whole thing keeping it away from the console's session. Access
  is a single-use, 30-second, owner-or-admin attach ticket redeemed for a
  path-scoped `HttpOnly` cookie, and the listener holds no other credential —
  it never falls through to the console session or the admin bearer.
  `wardyn/agent-vscode` (`make agent-image-vscode`) is the first app it carries:
  a pinned, sha256-verified code-server on `127.0.0.1:8080` behind a
  `/usr/local/bin/wardyn-ui-vscode` launcher, which is the entire BYOI contract
  — any image can serve a declared app by shipping one. The bound is stated up
  front, in the product and in [docs/UI-SANDBOXES.md](docs/UI-SANDBOXES.md):
  **nothing inside a relayed app is recorded** — the audit trail says an app was
  opened and closed, never what was done in it. Proven by a conformance case
  that runs the relay's transport identically on both substrates, an
  HTTP-client e2e lane (`make test-e2e-ui-sandbox`) and a browser e2e lane
  (`make test-e2e-ui`). A browser desktop (noVNC) is deliberately **deferred**:
  it is an image variant on this same primitive, not a server change. A general
  native lane stays **exploratory** — the four candidates are costed in
  UI-SANDBOXES.md rather than promised, and natively on the user's own machine
  remains VS Code Remote-SSH over the SSH gateway.
- **The ground-truth counter fix — the one filed defect that came home.** The
  demo series had filed a frozen control-plane counter for 0.6: Tetragon
  exported events, the ingest posted batches, the tally never moved. The root
  cause was the ingest sidecar's container→run index — built from a `docker ps`
  snapshot (running containers only) and replaced wholesale on every refresh,
  while the Tetragon export tails with lag, so a run whose container exited
  before the tail caught up resolved unmapped, was dropped, and moved no counter
  at all. The index is now fed by `docker events` (a container is known at
  CREATE, before its first exec) and merged rather than replaced, with entries
  outliving their container by 15 minutes so a lagging tail still correlates.
  `/healthz` and the heartbeat now publish `dropped_unmapped` beside
  `dropped_total` and `observed_total`, and the idle state names which of its
  two causes it is, so "the sensor saw nothing" and "the sensor saw plenty and
  correlated none" stop reading as the same `observed_total: 0`. The three
  sensor ceilings that investigation measured are now stated where operators
  read them — including `kernel.network.connect` staying permanently dead on
  WSL2 + Docker Desktop, which is environmental and measured, not a bug.
- **Extras.** The react-router advisory suppression is **deleted**:
  GHSA-qwww-vcr4-c8h2 patches at 7.18.2 as well as 8.3.0 — the suppression had
  been carried on a stale note claiming only the 7 → 8 major fixed it — so the
  console takes 7.18.2 and `make npm-audit` is green with **nothing** ignored
  (the major itself stays a named gap below, now on its own merits). A P2 polish
  slice landed across the CLI, API and console: `wardyn logs`, host/run filters
  on `approvals list|get`, tier commands that stop exiting 0 when the tier is
  not enabled, a typo'd site-config key that fails on the host instead of
  silently deleting the setting, `make reset` naming the corporate baseline it
  is about to destroy, the default ceiling policy becoming viewable in UI, CLI
  and API, and a run of console fixes that stop surfaces claiming state they had
  not checked. A fresh ponytail over-engineering audit ranked 26 cuts and
  applied the ones a provenance check did not overturn.
- **What 0.6 deliberately did not ship.** The k8s substrate is still **not** at
  feature parity with Docker — BYOI/devcontainer builds, `local_dir` mounts,
  per-pod PIDs/disk enforcement and a k8s ground-truth correlator remain on the
  v1.0 row, and both [`deploy/helm/wardyn/README.md`](deploy/helm/wardyn/README.md)
  and [docs/OPERATIONS.md](docs/OPERATIONS.md) keep the honest, code-checked
  "Kubernetes: known gaps" list rather than letting the cloud-base framing imply
  parity. The two designed-but-unscheduled candidates from the 0.5 campaign stay
  unscheduled: the sentinel-class PAT lane (proxy-injected git PATs, never
  resident) and routing an interactive run's tool approvals to the console the
  way autonomous `hold` runs already do — both slot into 0.7/0.8 when scheduled,
  not before.

## Planned

Everything below is **planned, unbuilt, and undated**. Where a seam exists but no
implementation does, [docs/PLUGGABILITY.md](docs/PLUGGABILITY.md) says so per row.

v0.7 → v0.8 is the remaining path to alpha: the same governance deployed to
developer desktops (0.7), then the alpha RC (0.8). The cloud base and
permissioning 0.6 owed are shipped — see "What v0.6 shipped" above.
Designed-but-unscheduled candidates from the 0.5 campaign — the
sentinel-class PAT lane (proxy-injected git PATs, never resident) and routing an
interactive run's tool approvals to the console the way autonomous `hold` runs
already do — slot into 0.7/0.8 when scheduled, not before.

| Milestone | Scope |
|---|---|
| **v0.7** | **Enterprise desktop deployment.** The base for orgs deploying Wardyn *onto developer machines* (MacBooks first) the way enterprise application admins actually ship software — managed distribution and managed configuration per current common practice — with the same permissioning system as the k8s cloud mode, enforced locally: the org decides which egress, secrets, and images a developer's agents may use, the developer runs auto-agents inside that envelope. Wardyn becomes the sanctioned way an org lets its developers run agents at all |
| **v0.8** | **Alpha RC.** The follow-through on 0.6/0.7 — the remaining enterprise-deployment enhancements, tools, and pieces — and the **last planned release candidate before the alpha go-live** |
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
  Admin/member roles and owner scoping are real and shipped (v0.5), and v0.6
  added capability grants over a user, an IdP group, or everyone (see "What
  v0.6 shipped") — which is authorization detail on top of those two roles, not
  a tenancy model. What's still speculative, no design in the tree: SAML/SCIM
  provisioning, an
  organization/tenant structure, per-user API tokens (today's only credentials
  are the shared admin bearer token or an OIDC session — no personal,
  independently-revocable API token a member could hand to a script), and
  CUSTOM roles beyond admin/member. The admin token and local mode remain the
  same shared credential they always were: always-admin, no per-human identity,
  no separation of duty from a real admin user (v1.0's row, above).
- **The SSH gateway's admin override is a registration-time stamp, weaker
  than the web terminal's live check.** `sshAuth` now grants an admin's own
  registered key an override — `run.created_by == principal` OR
  `key.role == admin` (migration `0043_ssh_key_role.sql`) — closing the gap
  this bullet used to name. What's left: `role` is stamped once, at
  `POST /me/ssh-keys` time, from the session's role THEN; it is never
  re-checked against the human's role NOW, unlike the web terminal's
  `requireOperator` gate, which reads the session live on every attach. A
  demoted admin's already-registered key keeps the override until that key
  is deleted and re-registered (or revoked) — there's no live lookup or
  expiry to catch a stale stamp automatically. Overrides are audited
  distinctly (`ssh.auth` carries `override:true`), and the ceiling is
  documented, not silently assumed away, in `docs/SSH.md`'s Bounds section
  and `threatmodel/THREAT-MODEL.md` residual #15.
- **The legacy `sources`/`base_image` workspace columns have no drop date, and
  the migration number reserved for it is gone.** 0.4.5's source-library split
  (migration `0031_source_library.sql`) kept the old embedded columns live for
  compatibility and reserved migration number `0032` in its own comment for
  the column-drop migration, "which must ship in a LATER release, never this
  one." That number is now taken — `0032_attach_tickets_token_sha256.sql`
  shipped in v0.5, and the v0.5 k8s/SSH merge renumbered its own new
  migrations up past it (`0033_ssh_public_keys.sql`, `0034_attach_ticket_role.sql`)
  — and v0.6 took the numbers through `0043_ssh_key_role.sql`, so the eventual
  drop migration needs a fresh number (`0044+`) whenever it's scheduled. That
  floor moves with every release; read the migrations directory rather than
  this sentence. Nothing depends on it happening by any particular release; it's
  listed here so the stale "is 0032" comment in `0031_source_library.sql`
  isn't mistaken for a live plan.
- **Age-key rotation for the secret store.** One age identity binds both
  encryption and decryption (`internal/secretstore/pg`); nothing re-encrypts
  stored secrets under a new key, and there is no `wardyn secret rotate`.
  Changing `WARDYN_AGE_KEY` strands every existing ciphertext — see
  [docs/OPERATIONS.md](docs/OPERATIONS.md)'s "The age key has no rotation path".
- **react-router 7 → 8 major bump.** No longer security-forced: GHSA-qwww-vcr4-c8h2
  patches at 7.18.2 as well as 8.3.0, the console ships 7.18.2, and the
  pnpm-audit suppression that once covered it is deleted — `make npm-audit` is
  green with nothing ignored. What remains is the major itself, blocked twice
  over: every stable 8.x peer-depends on React >=19.2.7 (this console is on
  18.3.1, so v8 means a React 19 migration first), and `react-router-dom` has no
  8.x at all — v8 is also a package rename to `react-router`. Needs a UI owner
  and a React 19 decision, not an advisory deadline.
- **Kata/TPROXY/io_uring composer quick-hits.** Parked since the
  composer-readiness work.
- **A member's inline model-access grant needs an operator integration.** A
  member may author an `inline_policy`, but its api_key/git_pat/ssh_key grant is
  clamped to grant KINDS the operator allows and then its {host, secret} pairing
  is dropped unless the operator eligible-listed that exact pairing
  (`filterMemberGrants`, `internal/api/inline_policy.go` — the secret-exfil
  guard: a member must not pair an arbitrary stored secret with an allowlisted
  host). A run's real model-access grant is re-added at launch by
  `foldRunIntegration` (an operator integration) or `applyWorkspaceRequirements`
  (a workspace requirement), so the supported multi-user flow is unaffected. The
  ceiling: a member whose model access relies ONLY on a raw operator secret + a
  wildcard `api_key` ceiling with NO integration and NO workspace requirement
  gets nothing re-added — the run launches without model access (fail-closed, no
  exfil). The drop used to be invisible to an operator — a clamp *warning* in
  preflight/Review and nothing else — so a deliberate member exfil *attempt*
  left no trace; 0.6 closed that: every drop now also records an
  `authz.denied` audit event with reason `grant_pairing_not_eligible`,
  aggregated one event per reason at launch (never on a preflight dry-run) and
  carrying the pairings that went (`auditMemberPolicyDrops`, same file). No
  preview lane disagrees with launch: preflight is the only one, and it
  resolves through the same `resolveRunPolicy` chokepoint and the same
  `resolveRunLLMAccess` verdict the create path uses (`internal/api/preflight.go`),
  so its checklist shows the dropped grant and the resulting no-model-access
  before the member launches. The fix, if pure-BYOK-for-members is a wanted
  flow: re-run the provider-convention model grant AFTER `filterMemberGrants`,
  so a member's own key survives with no integration and no workspace
  requirement behind it.
- **Scan-seeded egress has no provenance gate.** A scanned repo's derived hosts
  are unioned into a run's egress allowlist as *required* with no provenance
  check (`applyWorkspaceRequirements`/`source_scan.go`), so a hostile onboarded
  repo can widen egress (`egress:attacker.com`). This is no longer a
  secret-exfil vector — the member grant-scope drop above means no operator
  secret can be injected on the widened host — but gating scan-seeded egress by
  provenance is a residual follow-up (`threatmodel/THREAT-MODEL.md`).
- **Custom base-image build steps are collected but not layered into the image.**
  The New Workspace wizard's "custom recipe" base image collects, validates, and
  stores Dockerfile-style RUN/ENV/ARG steps, but no build path applies them —
  `FinalizeBase` wraps only the base ref (`internal/api/workspace_run.go`; the
  `ImageBuilder` interface has no `steps` parameter). Until step-layering is
  wired, the typed steps are recorded, not executed; the wizard copy
  (`ui/src/app/lib/workspace-copy.ts`) should not imply they run. The supported
  customization paths today are a devcontainer repo (`WARDYN_ENVBUILD`) or a
  pre-built BYOI image, both of which DO reach the sandbox. Surfaced by the
  2026-08-12 functional-paths review (`local/functional-paths-2026-08-12/`).

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
