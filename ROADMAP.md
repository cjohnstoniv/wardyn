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
| **v0.6** | **The enterprise-POC base: cloud deployment + real permissioning.** Capability grants (four kinds, per-kind enforcement switches, IdP groups), Kubernetes as the base deployment story (one-command kind quickstart, day-2 ops, `/readyz`), terminals beyond the browser (`wardyn ssh`, a kind-proven SSH lane, an admin override), governed UI sandboxes (a ticket-gated loopback relay + a code-server image), and the ground-truth counter fix (below) | **Shipped (pre-alpha)** — `v0.6.0` (see [CHANGELOG.md](CHANGELOG.md)); the release supersets `prep/v0.6` with the demo-video series. The daemon-free merge gate (`make ci`) is green at the release tip minus DCO sign-offs; `make test-e2e` carries 10 failures that reproduce on a pre-merge baseline on the same host — a pre-existing lane defect, not a 0.6 regression |
| **v0.7.0** | **Governance an org can delegate, on hardware it owns.** Assignable governance profiles (a named ceiling bound to a person, a group, or everyone) and a `security_admin` tier that can be handed the verdict without being handed the deployment, the rest of enterprise desktop deployment (systemd installer, MDM-distributable packages, the member-mode envelope, a reachable SSH gateway, digest-pinned upgrades), per-tool policy, never-resident git PATs for non-GitHub forges, and the corporate-network last miles — TLS-inspection root, internal model gateway, PrivateLink Bedrock (below) | **Shipped (pre-alpha)** — `v0.7.0`, 2026-09-09 (see [CHANGELOG.md](CHANGELOG.md)). The macOS `.pkg`, the MDM vendor example and the real-Mac smoke run stay **operator-gated** and are not in it |
| **v0.7.1** | Patch: the console header read the raw OIDC `sub` instead of the IdP's `name` claim for an SSO user; a stock `helm install` (persistence off) crash-looped on an empty recording dir hitting a read-only root filesystem | **Shipped (pre-alpha)** — `v0.7.1`, 2026-09-11 (see [CHANGELOG.md](CHANGELOG.md)) |
| **v0.7.2** | **Workspace Providers** — one admin object for which git/model providers are enabled, for whom, inside what bounds — an agent roster with per-person model credentials, ephemeral disk enforcement on Kubernetes, and seven field-report fixes from a private-endpoint Kubernetes estate (below) | **Shipped (pre-alpha)** — `v0.7.2`, 2026-09-12 (see [CHANGELOG.md](CHANGELOG.md)) |
| **v0.7.3** | A second field report from the same estate: the per-user AWS SSO lane can no longer sign with the wrong identity (account/role pinned, enforced at three doors), the Bedrock check texts and admin sign-in door stopped conflating a deployment-wide fact with a per-person credential gap, and the CSRF Origin guard now applies in every mode (below) | **Shipped (pre-alpha)** — `v0.7.3`, 2026-09-15 (see [CHANGELOG.md](CHANGELOG.md)) |
| **v0.7.4** | **Governance hardening over the whole surface a member or an operator's identity touches.** Per-run credential residency and revocation, the five findings of the 0.7.3 field report plus the owner's two testability asks, a kind-provable AWS SSO test path, an admin's own "view as member", and a repo-wide review campaign's fixes across the runner substrate, the egress proxy and the console (below) | **Shipped (pre-alpha)** — `v0.7.4`, 2026-09-16 (see [CHANGELOG.md](CHANGELOG.md)) |
| **v0.7.5** | A third field report from the same private-endpoint Kubernetes estate (Entra SSO, one enabled roster row: `claude-code` / `bedrock_sso` / `per_user`): the console stops asserting things that are false on that deployment shape (the New Run rail's credential-residency and Recording claims, the member's "Your model key" card, and Getting Started's lede), an admin can preview the not-signed-in member state, the AWS sign-in sandbox now runs the sign-in itself with every attach path joining it, a slow sign-in start no longer reads as unreadable and a new sign-in supersedes an orphaned one, a rebuilt Claude Code image boots without parking approvals on the CLI's own bootstrap, and on Kubernetes an autonomous run's `/tmp` and `/home/agent/work` are now inside `disk_mib` (narrowed, not closed) (below) | **Shipped (pre-alpha)** — `v0.7.5`, 2026-09-17 (see [CHANGELOG.md](CHANGELOG.md)) |
| **v0.7.6** | "The facts exist; connect them to the person" — a fourth field report from the same estate: an actionable model-access state now rides a banner on every screen instead of only Getting Started, a run refused for a dead model credential offers the sign-in instead of directions to it, a slow start says what it is waiting on instead of a poll-tick guess, the AWS sign-in tab opens and closes itself, a spent refresh token stops grading `live` for days, and wardynd's own outbound calls (OIDC, AWS SSO renewal, Entra sync) gain a scoped corporate-proxy knob that does not share `HTTPS_PROXY`'s process-wide blast radius; a mid-run credential lapse holding the run instead of killing it ships behind a kill switch (below) | **Shipped (pre-alpha)** — `v0.7.6` (see [CHANGELOG.md](CHANGELOG.md); tag `v0.7.6`, 2026-09-18) |
| **v0.7.7** | A fifth field report from the same estate: with an expired AWS SSO session, Launch bounced the console to Getting Started. The setup gate stops grading the two per-person model-provider rows, a create-time refusal carries the machine-readable reason `model_credential`, the console opens the sign-in from that refusal and relaunches the same run, and the launch redeems an expired session at the click instead of admitting a spent one (below) | **Shipped (pre-alpha)** — `v0.7.7`, 2026-09-18 (see [CHANGELOG.md](CHANGELOG.md)) |
| **v0.7.8** | Three field reports and the terminal: the setup gate's blocking decision moved server-side (`SetupCheck.Blocking`), the shipped confinement floor is CC1 with the strongest installed class as the default, a dial refusal names its own cause and hop, AWS-lane refusals answer in SDK-readable JSON, `wardyn attach` rides a single-use ticket, and the terminal's holder and focus defects are fixed (below) | **Shipped (pre-alpha)** — `v0.7.8`, 2026-09-19 (see [CHANGELOG.md](CHANGELOG.md)) |
| **v0.7.9** | Patch: the egress proxy shared one TLS config with the sidecar's control-plane client, so on a corporate-CA install it offered HTTP/2 it could not speak and every re-originated request to a peer that accepted the offer failed; the proxy now speaks HTTP/2, handles a peer that speaks it unasked, and files a protocol mismatch as its own refusal instead of a dial failure | **Shipped (pre-alpha)** — `v0.7.9`, 2026-09-21 (see [CHANGELOG.md](CHANGELOG.md)) |
| **v0.7.10** | **Per-person Azure DevOps access on Entra ID**: a run reaches Azure DevOps as the person who started it, with their own sign-in captured at console login and never placed in the sandbox; every REST call and git push is checked against a plain-language capability the run was granted, and a request beyond it is held for approval once or for the run. Also: an SSO-only console posture, Bedrock policy-deny and throttle refusals named on the failed run | **Shipped (pre-alpha)** — `v0.7.10`, 2026-09-22 (see [CHANGELOG.md](CHANGELOG.md)) |
| **v0.7.11** | Patch: Azure DevOps projects and repositories whose names carry spaces or other permitted characters (`Payments Platform`, `Card Auth (v2).Service`) import, launch, clone, fetch and push; every door stores one spelling of the address, and approvals name the repository the same way on the REST and git paths | **Shipped (pre-alpha)** — `v0.7.11`, 2026-09-22 (see [CHANGELOG.md](CHANGELOG.md)) |
| **v0.7.12** | Patch: stored credentials are sealed with AES-256-GCM per row, bound to their owner and name (envelope v1, a one-way conversion on first boot); the control-plane → proxy hop that carries credential values is TLS 1.3, pinned to a CA wardynd mints; every secret-carrying boot setting accepts a `_FILE` path (Vault Agent / CSI), with an opt-in chart mode; an Azure DevOps address's host now ends at `?` or `#` | **Shipped (pre-alpha)** — `v0.7.12`, 2026-09-23 (see [CHANGELOG.md](CHANGELOG.md)) |

### What v0.4 shipped

- **Containerized setup is the default.** `make setup` brings up the compose stack;
  host mode is an advanced escape hatch (`WARDYN_SETUP_MODE=local`). The console
  gates a *new* install behind Getting Started, and the model/harness step is
  harness-first and skippable (only the sandbox barrier is required).
- **Credentials are first-class at the CLI.** `wardyn subscription
  connect|status|disconnect` (stdin only, age-encrypted, injected proxy-side) and
  `wardyn setup status`, which prints the exact next command per unmet check.
  `WARDYN_SUBSCRIPTION_TOKEN` is not a headless seed — `make setup` warns and
  ignores it; connect through the CLI or the console (`docs/ENV.md` says why).
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
  `local_dir` mounts, no per-pod PIDs limit, no ground-truth
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
- **Signed, published, attested release images.** `.github/workflows/release.yml`
  builds and publishes the seven images a release ships (`wardynd`,
  `wardyn-proxy`, `agent-base`, `agent-codex-cli`, `agent-aws-sso`,
  `agent-vscode`, `agent-novnc`) to
  `ghcr.io/cjohnstoniv/<name>` on a `vX.Y.Z` tag push, multi-arch
  (linux/amd64 + linux/arm64), and cosign-signs each keylessly (Fulcio/Rekor via
  the Actions OIDC token — no long-lived signing key to manage). Each digest
  additionally carries an attested CycloneDX SBOM scanned from the PUSHED IMAGE
  (not the source tree, which omits every base-image package) and
  `attest-build-provenance`. The Release itself carries those SBOMs,
  `THIRD-PARTY-NOTICES.md`, and a cosign-signed `SHA256SUMS`.
  `docs/VERIFY.md` is the consumer-side procedure.

  No image containing a proprietary vendor CLI is published:
  `agent-claude-code` is a local build recipe (`make agent-images`), and
  `agent-base` ships in its place.

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
  Compose — and demo **V13 ("your terminal, our cluster")** films it end to end
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
  (`make test-e2e-ui`). A browser desktop (noVNC) **shipped in 0.7**
  (`deploy/images/novnc/`, `make agent-image-novnc`) — and, as predicted, it is
  an image variant on this same primitive with **no server change**. Both
  `agent-vscode` and `agent-novnc` publish from the next tagged release (#141):
  the trivy matrix, a per-image SBOM and a GPL source offer for a whole desktop
  turned out to be exactly the supply-chain workstream predicted, now landed in
  `release.yml`'s `images-ui-sandbox` job rather than deferred indefinitely. A
  general
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
- **Desktop tier: a governed daemon on a managed laptop — the macOS install
  lane, shipped-half.** What v0.7 was going to build from nothing, 0.6 shipped
  the first half of: `deploy/desktop/` is a real, documented, machine-checked
  configuration of the same compose stack every other single-host deployment
  runs, not a separate build — `WARDYN_LOCAL_MODE`, a per-device `age.key`
  minted by the installer and never by MDM, and the org's ceiling delivered as
  an ordinary `policy.json` file ([docs/DESKTOP.md](docs/DESKTOP.md)). The
  install lane itself — `install.sh`, a launchd `LaunchDaemon`, and the
  `wardyn-desktop.sh` wrapper it runs — was macOS-only in 0.6; **0.7 built the
  Linux/systemd path** the topology diagram shows. **Member role: none, by design, on
  the local-mode variant** — local-mode callers are *always* admins
  (`Server.requireOperator`), so this tier's default posture has no member/
  admin split at all, only "the developer is the operator." The envelope's
  documented SSO variant does carry real OIDC member/admin RBAC (same code
  path as every other tier). In 0.6 `wardyn-desktop.sh`'s automatic
  `site-config apply` only ran under local mode, on the premise that the wrapper
  had no CLI-usable credential under SSO — **0.7 found that premise wrong** (the
  MDM-delivered admin token is already in the container and authenticates even
  with OIDC configured) and deleted the gate. `scripts/test-desktop-profile.sh`
  (`make test-scripts`) and `ci.yml`'s `desktop-envelope` job (boots the real
  compose profile and proves `/policies/default`, no-policy resolution and
  Recording Mode synthesis all honor the managed ceiling) are the honesty
  gates; a scripted smoke run against a real Mac is **still owed** — see
  [docs/DESKTOP.md](docs/DESKTOP.md) "Try it, once, on a real Mac". 0.7 did not
  close it: it needs hardware, not code.
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
  not checked. A fresh over-engineering audit ranked 26 cuts and
  applied the ones a provenance check did not overturn.
- **What 0.6 deliberately did not ship.** The k8s substrate is still **not** at
  feature parity with Docker — BYOI/devcontainer builds, `local_dir` mounts,
  a per-pod PIDs limit and a k8s ground-truth correlator remain on the
  v1.0 row, and both [`deploy/helm/wardyn/README.md`](deploy/helm/wardyn/README.md)
  and [docs/OPERATIONS.md](docs/OPERATIONS.md) keep the honest, code-checked
  "Kubernetes: known gaps" list rather than letting the cloud-base framing imply
  parity. Both of the 0.5 campaign's designed-but-unscheduled candidates — the
  sentinel-class PAT lane and **C0** — were taken up in 0.7 and are recorded
  below.

### What v0.7 shipped

Shipped as `v0.7.0`; [CHANGELOG.md](CHANGELOG.md)'s `[0.7.0]` entry is the full
list.

- **Governance profiles — an assignable ceiling, not one deployment-wide
  default.** A named policy ceiling an admin binds to a person, an SSO group, or
  everyone, so a contractor group and a platform team hold genuinely different
  limits on one install. Precedence is user over group over all (priority, then
  name, breaking ties). A profile **replaces** the deployment default rather than
  composing with it — the only shape where reading a profile tells you what it
  permits — and with no assignment every resolution is byte-for-byte what it was.
  A profile can only ever NARROW credential eligibility, and its denied hosts are
  re-asserted inside dispatch, after the phases that add corporate hosts and
  credential injections — including the brokered git and PAT lanes, which never
  consulted the deny list before. See [docs/OPERATIONS.md](docs/OPERATIONS.md)'s
  "Three roles, and who sets the walls".
- **Two more capability kinds, and a quota.** `agent` and `integration` join the
  grant table (both narrowing, so an upgrade with no rows written changes
  nothing), bounding which harness a member may launch and which AI-provider
  integration they may name on their own run — never the workspace pin or the site
  default, which are yours. `max_concurrent_runs` joins a governance profile's
  limits beside the two launch modes it can refuse.
- **A second admin tier, and a console that can be delegated to it.**
  `security_admin` governs the verdict — profiles, permissions, egress decisions,
  token inventory, audit verification — and deliberately does **not** reach into a
  run: it is never stamped on an SSH key or an attach ticket, and no capability
  grant can widen it. A mapped tier only, never derivable from the operator
  allowlist. That separation is what makes the surface safe to hand out.
- **Enterprise desktop deployment — the rest of it.** The Linux/systemd installer
  and uninstaller, an MDM-distributable `.deb`/`.rpm`/tarball built from a clean
  tree, the member-mode (m′) envelope, the SSH gateway actually reachable on the
  tier, digest-pinned images with a working upgrade path, and a browser desktop
  (noVNC) as an image variant.
- **Per-tool policy.** `tool_rules` makes an autonomous run no longer
  all-or-nothing, with a console editor that refuses what the API would refuse, in
  the same order, before the round trip.
- **The sentinel-class PAT lane** (proxy-injected git PATs, never resident) — the
  last designed-but-unscheduled candidate from the 0.5 campaign. A `git_pat` for a
  non-GitHub forge is now minted proxy-side and injected on the outbound leg,
  closing the asymmetry with `github_token`. It makes the credential
  non-resident; it does not make it least-privilege, because Wardyn cannot narrow
  a scope the operator issued.
- **External clients drive a sandbox over the SSH gateway.** Scripted key
  registration, readiness and target discovery (`wardyn ssh-key ensure`, `wardyn
  run wait-ready --json`, `wardyn ssh --json`), plus a per-run git push-namespace
  opt-out (`git_push_any_branch`), audited on every push that uses it.
- **The corporate-network last miles.** A trusted TLS-inspection root
  (`WARDYN_TRUSTED_CA_FILE`) picked up by the daemon, the proxy sidecar and every
  sandbox; an internal model gateway for the api-key lane; declared internal
  hostnames; and Bedrock reached through a VPC (PrivateLink) endpoint
  (`WARDYN_BEDROCK_BASE_URL`).
- **Members do more without an admin.** Their own secrets and their own model API
  key (never reachable from anyone else's run), a member Getting Started of their
  own, and an admin People step that writes role mappings live from the console —
  guarded by a posture-flip acknowledgement and a refusal to remove the acting
  admin's own access.
- **C0 is RESOLVED, not deferred again** — as a refusal, not a build. The spike
  found no mechanism that fails closed for an interactive session:
  `wardyn-toolgate` routes only a NON-interactive run's tool calls to the approval
  FSM, upstream pins `claude`'s `--permission-prompt-tool` to non-interactive use,
  and the hook-based alternative fails *open* on a timeout — the wrong default for
  an approval gate. Self-service value collapses anyway: the human deciding the
  prompt can already attach to the run and answer it directly. What shipped is the
  honest half — `tool_approvals=hold` on an interactive run used to be accepted
  and silently discarded, and is now refused with a 400 naming the field.
- **Still owed, and operator-gated.** The macOS `.pkg` (needs an Apple Developer
  ID), the MDM vendor example (needs a tenant), and the owed real-Mac smoke run.

### What v0.7.2 shipped

Shipped as `v0.7.2`; [CHANGELOG.md](CHANGELOG.md)'s `[0.7.2]` entry is the full
list. 0.7.2 is a patch line carrying ONE unplanned feature — recorded
as a dated exception in [RELEASING.md](RELEASING.md), since fast-forwarding
`release/0.7` onto a feature-carrying `main` IS the release branch taking a
feature — plus the follow-ups from two customer field reports on a
private-endpoint Kubernetes estate.

- **Workspace Providers — one admin object for which providers are enabled, for
  whom, inside what bounds.** Every mechanism this needed already existed in 0.7.1
  as a separate seam (git-host credential lanes, the never-resident PAT broker,
  site-config's upstream proxy, user drives with per-tier size overrides,
  `disk_mib`); what did not exist was a single statement of org policy over them.
  `SiteConfig` gains `workspace_providers` — git-provider rows (`github` |
  `azure_devops`, allowed HTTPS base URLs, permitted credential lanes) and two
  storage ceilings — with its own admin-only `GET`/`PUT /workspace-providers`
  beside the `PUT /site-config` door MDM already delivers to every laptop. A
  repository a run clones has to be on an enabled provider, asked at **ten** doors
  (workspace create/update/scan/build, the source library, run create over the
  RESOLVED spec, the legacy `repo` field, `devcontainer_repo`, and the record and
  source-scan launchers that create runs without passing either request-path
  gate). Providers mint nothing — they veto: a credential lane a row does not
  permit drops its wiring and says so on the `201`. A seventh capability kind,
  `workspace_provider`, bounds which row a member's work may come from. The Git
  host card retires into the provider row it always described. With no rows
  written every predicate is a no-op, and an upgraded 0.7.1 install answers
  byte-for-byte what it answered before.
- **An agent roster, and a model credential per person.** `SiteConfig` gains
  `agent_providers` — per agent: enabled, ONE model-access mechanism drawn from
  the lanes that already exist at dispatch, and whether that credential is shared
  or captured per person — because availability used to be an image map with no
  auth semantics, and one admin's captured AWS SSO session silently backed every
  member's runs. A declared mechanism is never quietly swapped for another: the
  lane dispatch actually SELECTED is compared against the row before a sandbox
  exists, so a deployment configured for an API key stops dispatching Bedrock
  because a region and a bearer secret happen to be present. A renewable AWS SSO
  session is renewed control-plane-side at dispatch rather than discarded, and the
  sandbox's cache stops carrying the refresh token while the control plane holds
  it — two parties rotating one token is how hourly re-auth became the resting
  state.
- **Ephemeral disk is enforced on Kubernetes.** A run's `disk_mib` becomes the
  agent container's `resources.limits[ephemeral-storage]`, so the kubelet bounds
  the writable layer and evicts the pod over it. `StorageEnforcement` gains
  `eviction`, the honest word that completes its five-word set, and both
  substrates' disk caps now report one word on the admin setup status. This
  closes one item on the k8s-parity row
  below; BYOI/devcontainer builds, `local_dir` mounts, a per-pod PIDs limit and
  the ground-truth correlator stay open.
- **The field reports' seven findings.** The console no longer fails OPEN to admin
  when `/me` does not answer; a SiteConfig save says it applies from the next
  dispatch; the egress "N held" badge counts only PENDING; a run's terminal
  transition CANCELS its outstanding approvals (migration `0062`) instead of
  leaving live Approve/Deny buttons on a dead run; the audit trail stops evicting
  itself (a self-inflicted renew loop backs off and gives up, and identical
  consecutive `auth.failed` rows fold into one summary row carrying a count);
  a private-IP denial is answered once per run rather than once per retry; and the
  `cidrs` docs trap is inverted — empty is the right default.
- **Console and refusal copy ships DRAFT.** Every new `400`/`412`/`422` body and
  console string is a frozen DRAFT constant pending the maintainer's canon sitting;
  the tests assert through the constants, so the swap is a one-file diff per lane.

### What v0.7.3 shipped

Shipped as `v0.7.3`; [CHANGELOG.md](CHANGELOG.md)'s `[0.7.3]` entry is the full
list. A second field report from the same private-endpoint Kubernetes
estate, written inside the first hour of running 0.7.2's `agent_providers` roster
— 7 findings and 1 confirmation, all in the new surfaces, plus the CSRF Origin
guard 0.7.2 itself named as an open gap.

- **The per-user AWS SSO lane can no longer sign with the wrong identity.** An
  admin now pins which AWS account and role a `per_user` sign-in may capture,
  the login sandbox asks when several accounts are reachable and nothing is
  pinned, and the pin is enforced at three fail-closed doors — roster save,
  sign-in, and capture — so a cloud team granting an unrelated SSO entitlement
  can no longer silently re-point which identity a deployment authenticates as.
  Wardyn still cannot see the resulting Bedrock 403 (the SSO-mode call is an
  opaque SigV4 tunnel), so the fix is fail-fast upstream rather than a
  once-per-run memo — the wrong identity never starts a run.
- **The Bedrock check texts, the admin sign-in door, and the admin bearer
  token's own `model_access` all stopped describing a deployment-wide fact for
  a per-person credential gap.** A `per_user` admin now reads the one action
  that can succeed instead of three dead ends; Settings → Model provider's
  sign-in dialog stopped discarding the roster's stored portal URL; and the
  shared admin token's own state reads `not_applicable` rather than a
  "Sign in to AWS" action it cannot take — unless a session it already
  captured is live, in which case it grades normally, because dispatch still
  serves it. The token can no longer capture a NEW `per_user` session at all
  (refused `422`, audited) wherever a console sign-in exists to redirect to.
- **Declaring a per-person lane and signing in to it are linked.** The Agents
  tab says, at save, that the lane is per person and the save only declares it;
  Settings' Model provider card names where the lane lives and whose sign-in
  its badge reads.
- **The `Fence` / `NetworkPolicy: enforcing` header chips are gone.** Both were
  fixed at boot and conveyed nothing after one read on every screen a member or
  admin opened; the same verdict and tier matrix already lived on the admin
  setup page's Environment step, which is now the only place to read posture.
- **"Start a run like this one" reaches every terminal run.** The clone CTA
  moved off the killed-run panel onto the run header for any terminal state,
  and joined the Runs-list row kebab on both the board and the table.
- **The cross-origin (CSRF) guard on a cookie-authenticated mutation now
  applies in every mode**, closing the gap 0.7.2 left open outside LocalMode,
  with the same widening reaching the browser PTY-attach WebSocket behind a
  TLS-terminating ingress.

### What v0.7.4 shipped

Shipped as `v0.7.4`; [CHANGELOG.md](CHANGELOG.md)'s `[0.7.4]` entry is the full
list. Two bodies of work met in it: the five priority findings of the 0.7.3 field
report (with the same estate's two testability asks), and a review campaign over
the whole `feat/v0.7.4` delta.

The field report and the asks:

- **A member can attach to their own AWS SSO login sandbox** (P1), and
  `POST /setup/harness-login` no longer blocks the caller through a cold image
  pull (P5) — the login pane returns as soon as the run has an id and waits in
  the browser instead.
- **An admin can exercise the member path without a second identity** (P2, and
  the owner's second ask): "view as member" clamps a signed-in admin — either
  tier — to a member's ceilings for the session, marks every admin-tier refusal
  it meets in the audit trail, and is exited from the banner it paints on every
  screen.
- **The member's own Getting Started no longer calls an admin-only endpoint**
  (P3). A member's cold load renders from the member-projected `/setup/status`
  the server redacts for them, rather than 403-ing its way to an empty page.
- **A roster pin is enforced against a stored capture that contradicts it**
  (P4). `POST /runs` answers 422 and dispatch fails closed, naming both the
  account/role the stored session is for and the account/role the agent now
  allows, instead of the run discovering it as an opaque IAM 403. Setting the
  pin does not *invalidate* the stored capture — that stays a Known gap in the
  CHANGELOG; what changed is that the contradicted capture can no longer start
  a run.
- **AWS SSO in multi-user SSO mode is testable on `kind`** (the owner's first
  ask): a scripted walk over a fake IAM Identity Center that a maintainer can
  run start to finish, and that the member-mode, login-pane and pin-dispatch
  work above is proven on.

The review campaign, by class:

- **Every front-door `helm install wardyn` recipe renders.** Three pasteable
  blocks failed closed at the chart's own preflights (the age-key source, the
  runs-namespace choice, the CC2/CC3 RuntimeClass pin); a guard now holds every
  pasteable fenced block in the front-door docs to the arms that apply to it —
  text-only, and an elided `...` snippet is excluded rather than validated.
- **A run's credential-bearing Kubernetes objects are reclaimed even when its
  pods are already gone** — and the Role that does it still has no Secret-body
  read capability in any configuration.
- **A run's credential is resident for less of its life, and revocable for
  the rest of it.** A killed run's token opens no `/internal/*` door, a
  governance profile that walls off Bedrock withholds the resident AWS keys and
  the host `~/.aws` mount too, a pasted credential cannot overwrite a
  containerized-login capture, and the UI-sandbox relay session is
  bounded-stale rather than a frozen eight-hour bearer.
- **The egress proxy's injection is port- and policy-normalization-aware.** A
  port-qualified allowlist entry no longer crash-loops the sidecar, cleartext
  injection is refused on the TLS-conventional ports, a port-qualified wildcard
  deny cancels the binding, injection over plain port 80 needs a genuinely bare
  entry, and a trailing-dot or non-ASCII policy entry is no longer silently
  dead — **read the CHANGELOG's entry before upgrading a policy authored with a
  trailing dot: it now grants the host it names, where it previously granted
  nothing.** Control-plane traffic also stopped riding the corporate proxy.
- **A 5xx no longer hands a member the database.** Every door a member can
  reach answers a store failure with the operator-facing sentence alone and
  sends the raw driver text to the log. The admin-tier and run-token sites are
  not converted; the CHANGELOG records that as a gap.
- **The member console tells a member the truth.** Member-visibility fixes
  across Runs, Providers, Workspaces, Approvals and Setup, and the same
  treatment for the `security_admin` tier, which receives the same redacted
  status body a member does.
- **Accessibility and theming**, swept across the console's screens.
- **Docs match code.** A blind docs↔code pass over the install, operate and
  reference docs, with the drifts it found fixed at the doc or at the code,
  whichever was wrong.

### What v0.7.5 shipped

Shipped as `v0.7.5`; [CHANGELOG.md](CHANGELOG.md)'s `[0.7.5]` entry is the full
list. A third field report from the same private-endpoint Kubernetes estate —
Entra SSO, one enabled roster row (`claude-code` / `bedrock_sso` / `per_user`) —
found seven findings (plus 2b, a lede that contradicted its own chip) in the
console's own copy and the AWS sign-in sandbox's behaviour on that deployment
shape, all fixed below alongside the estate's disclosed Kubernetes gap.

- **The New Run rail stopped asserting things that are false on this
  deployment shape** (finding 1). Its "What this run can do" panel now reads
  the server's own graded residency for the model credential and the
  `/healthz` recording state instead of two unconditional claims, one of which
  was a false assurance on the per-user Bedrock SSO lane.
- **A member's "Your model key" card stopped saying "already done" or
  "provided by your admin" under a per-person AWS SSO lane** (findings 2 and
  2b) — the card now reads the same truth table the "Model access" chip above
  it already used, and the page's lede stopped saying "shared credentials"
  under a lane that is specifically not shared.
- **An admin can preview a new member's not-signed-in state** (finding 3).
  "View as a new member (not signed in)" is a second posture beside the plain
  member-mode toggle, offered only on a `per_user` deployment, that hides the
  admin's own captured AWS session for the session and refuses a sign-in
  attempted inside it rather than capturing over the admin's own identity.
- **The AWS sign-in sandbox now runs its own sign-in, and every attach path
  joins it** (finding 4) — the console's sign-in pane, `wardyn attach`, an SSH
  attach and the Runs list all land on the same running sign-in instead of the
  Runs-list path handing out a bare, unlabelled shell.
- **A rebuilt Claude Code image boots without parking an approval on the
  agent's own bootstrap** (finding 5): the plugin-marketplace auto-install,
  the self-updater and the changelog fetch are off by default in `agent-base`
  and any image built from it — an image on another base, or an older pinned
  tag, still parks them (Known gap).
- **A slow sign-in start no longer reads as unreadable, and a new sign-in
  supersedes an orphaned one** (findings 6 and 7): the wait is graded on a
  clock instead of a poll-tick budget too short for a cold image pull, and
  starting a sign-in now closes that person's previous one server-side before
  the retry can be refused by a concurrency cap.
- **On Kubernetes, an autonomous run's `disk_mib` now bounds its `/tmp` and
  workdir writes** — narrowed, not closed: the rest of `$HOME` an autonomous
  run's agent writes to is still outside the cap, and an interactive run was
  already fully metered since 0.7.2. The estate's own disclosed gap from
  0.7.4.

What did **not** close this release — see the CHANGELOG's "Known gaps and
deferrals" for the full statement of each: the rest of `$HOME` an autonomous
k8s run's agent writes to, outside the `/tmp`/workdir cap; a rare
cross-replica timestamp race that can still leave two live sign-in sandboxes
for one person; a green **hosted** nightly run, not yet observed even though
the four harness defects that kept it red are fixed; and Claude Code's own
*Bypass Permissions mode* confirmation, which a run launched with "let it use
tools before I attach" still parks on until a human attaches and answers it.

### What v0.7.6 shipped

Shipped as `v0.7.6`; [CHANGELOG.md](CHANGELOG.md)'s entry is the full list. A
fourth field report from the same private-endpoint Kubernetes estate
consolidated one journey — a person getting AWS SSO working and running a
Claude Code agent — into eight findings: the facts the deployment already
computes (`model_access`, a starting run's own waiting reason, a dispatch
refusal's cause) were not reaching the person who needed them. Seven of the
eight are unconditional below; the eighth ships behind a kill switch.

- **An actionable model-access state now rides a banner on every screen**
  (finding 2), not only Getting Started — a strip in focus mode and on the run
  cockpit, carrying the AWS sign-in itself in a dialog. Suppressed only where a
  page already mounts the same sign-in.
- **New Run's model warning is about the person, not the deployment**
  (finding 1): the rail now reads the same per-person `model_access` state the
  banner does, instead of a deployment-wide fact that was already true the
  moment an admin saved a `per_user` roster row.
- **A run refused for a dead model credential now offers the sign-in, not
  directions to it** (finding 3), on the run page itself, and the refusal's
  own destination clause now names a door the reader — not just an admin — can
  open.
- **A slow start says what it is waiting on** (finding 6): the kubelet's own
  reason reaches the run header, the Runs board and the sign-in pane, and a
  terminal reason (an unpullable image, a bad reference) ends the wait in
  seconds instead of after five minutes of an unexplained clock.
- **The AWS sign-in tab opens and closes itself** (finding 7): a tab opens on
  the click that starts the sign-in rather than depending on a later
  auto-navigation browsers block, and the sign-in panel closes itself the
  moment the server confirms the session was stored rather than waiting on the
  sandbox's own message to cross the wire.
- **A spent AWS SSO refresh token stops grading `live` for days** (finding 5):
  grading now consults the same in-memory spent-mark the refresher maintains,
  instead of a registration timestamp a spent token can no longer redeem.
- **wardynd's own outbound calls gain a scoped corporate-proxy knob**
  (finding 8), `WARDYN_DAEMON_PROXY_URL`, that does not share `HTTPS_PROXY`'s
  process-wide blast radius into the Kubernetes client's own API access.
- **A mid-run AWS SSO credential lapse holds the run instead of killing it**
  (finding 4) — the SSO access token is no longer written into the sandbox on
  that lane; the proxy injects it on the wire instead, and a lapsed session
  parks the sandbox's next credential exchange (bounded by
  `WARDYN_CREDENTIAL_REAUTH_TIMEOUT`, default 600 s) while its owner signs in
  again, then resumes the SAME run. The measured tolerance of the reference
  agent's own SDK — at least eleven minutes, the test's own ceiling —
  comfortably outlasts that hold, so 600 s stands. Shipped **on by
  default**; the kill switch `WARDYN_AWS_SSO_PROXY_INJECT=off` is the
  rollback. The docker-gated resume test
  (`TestDocker_TheSameRunResumesWhenTheHoldReleases`) has run green on this
  release's tip (22 s).

What did **not** close this release, and was not bundled into 0.7.7 either —
0.7.7 answered a different field report instead — see the CHANGELOG's "Known
gaps" for the full statement of each: the §7.4 frozen copy table's
admin-only remedy clause, a credentialed-proxy form of the daemon proxy
knob, the spent-token mark's in-memory (unpersisted) posture, the Runs
board's group header, and the three items already promised after 0.7.5 (the
per-person supersede lock, a third Kubernetes cache volume, the admin-tier
5xx driver-text sweep, O-1) — all seven now sit on the 0.8 plan
([docs/design/0.8/PLAN.md](docs/design/0.8/PLAN.md)).

### What v0.7.7 shipped

Shipped as `v0.7.7`, 2026-09-18; [CHANGELOG.md](CHANGELOG.md)'s entry is the full
list. One journey from the same private-endpoint estate — an admin with an expired
AWS SSO session clicks Launch — and both halves of what went wrong:

- **The setup gate no longer bounces a lapsed admin to Getting Started.** The two
  per-person model-provider rows (`llm_provider`, `bedrock_provider`) are optional
  per person and stop gating the console; host posture rows still do.
- **A refused launch names its class.** The create-time 422 carries
  `reason: model_credential`; the console opens the sign-in door from it and
  relaunches the same run once the capture lands.
- **Launch redeems an expired session at the click.** A spent refresh token is a
  422 with the door; an AWS outage is a 422 without one ("launch again in a
  moment") — the two are told apart, and neither launches a run that will fail.
- Carried from 0.7.6, still open: the per-person sign-in supersede lock, the third
  Kubernetes cache volume, the admin-tier 5xx driver-text sweep — all on the 0.8
  plan.

### What v0.7.8 shipped

Shipped as `v0.7.8`, 2026-09-19; [CHANGELOG.md](CHANGELOG.md)'s entry is the full
list. Three field reports (a lapsed-SSO admin bounced to setup, control bytes in a
PTY-scraped device URL, an operator's corporate-proxy hour) plus the terminal:

- **The setup gate is a daemon decision** (`SetupCheck.Blocking`): only a runner
  failure, an unmeetable confinement floor or a missing role mapping confiscate
  the console; every other row keeps its grade and stays out of the way.
- **The confinement floor is CC1, the default is the strongest installed class**,
  and a run records whether its class was requested or defaulted.
- **A dial refusal says why**: `builtin:dial-failed` carries the failed stage,
  the underlying error and the hop attempted; AWS-lane refusals answer in
  SDK-readable JSON instead of a plain-text 502 handed to a JSON parser.
- **`wardyn attach` rides a single-use ticket**, and the terminal's stranded
  holder, focus and remount defects are fixed; in-place observer promotion and
  the ordinary-use corpus are 0.8 items.

## Planned

Everything below is **planned, unbuilt, and undated**. Where a seam exists but no
implementation does, [docs/PLUGGABILITY.md](docs/PLUGGABILITY.md) says so per row.

v0.8 is the remaining path to alpha. The cloud base and permissioning 0.6 owed
are shipped, and so is 0.7's governance and desktop work (above, through
`v0.7.6`), so what is left below is the alpha RC and beyond.

**v0.8 is in progress (from 2026-09-19).** The plan — every lane, decision and open
question — is [docs/design/0.8/PLAN.md](docs/design/0.8/PLAN.md); the work is tracked on
the `0.8.0` and `0.8.1` milestones, one issue per lane, and nothing starts before its issue
carries the `approved` label ([CONTRIBUTING.md](CONTRIBUTING.md)).

**New for 0.8: posture-gated autonomy** — an org-defined rubric mapping a
sandbox's containment posture (egress reach, secrets present, confinement class)
to a permitted autonomy level, enforced both at the Wardyn boundary and, for
agents that support managed settings, by generating that agent's enterprise
policy file. Researched during 0.7 and deliberately not built in it; the
groundwork is that the posture inputs and the approval FSM it would ride already
exist.

**Also new for 0.8: hybrid local + remote.** Today Wardyn has two tiers that do
not know about each other — an org control plane on Kubernetes
([docs/OPERATIONS.md](docs/OPERATIONS.md)) and a local daemon per laptop,
MDM-managed, one machine per developer ([docs/DESKTOP.md](docs/DESKTOP.md): "A
local daemon per laptop. No shared control plane, no cluster"). Hybrid is the
deployment where they are one product: the org runs the control plane on the
cluster, MDM installs Wardyn on the laptop in member mode, and the **same person
under the same org-managed policy flexes a sandbox between local and remote
hardware** — a quick edit on the laptop's own CPU, a long build on the cluster's
— with one identity, one ceiling, one audit stream. The disk half follows: a
local directory linked into a remote sandbox, and a remote drive readable
locally. The groundwork exists — member mode (`m′`) already makes the developer a
non-operator against an org IdP, 0.7.2's `SiteConfig.WorkspaceProviders` is
already an org-authored provider policy MDM delivers as
`/etc/wardyn/site-config.json`, and the SSH gateway already carries an sftp
channel into a running sandbox. What does not exist is enrolment of a desktop
into a *remote* control plane, per-run placement, and any link between a laptop's
filesystem and a cluster sandbox. Researched in 0.7.2 and written up in
[docs/design/hybrid-0.8.md](docs/design/hybrid-0.8.md); not built in it.

**Punted from 0.7.x, by id.** Every deferral 0.7.0/0.7.1/0.7.2 took a disposition
on and did not build. The ledger is public here rather than only in a plan file;
each id is searchable in the source it came from
(`local/review-0.7/FOLLOW-UPS-0.7.1.md`, the drives `DESIGN.md`/`FOLLOWUPS.md`,
and `threatmodel/THREAT-MODEL.md`'s residual numbers).

- **Drives.** `wardyn drive get|apply` CLI (UD-cli) · in-product reclaim + a PVC
  delete verb + a `drive.reclaim` audit row (UD-reclaim) · member self-service
  Reset (UD-reset) · drive-aware concurrent-run collision warning + run-row drive
  persistence (UD-collision / D3) · widening `/drives/grants` and `/preview` to
  the security-admin tier (UD-tier) · the share readability probe as uid 1000
  (UD-readprobe) · **byte enforcement on Docker volumes and shares (TM #36)** —
  never built into the product, and 0.8 closes it as a documented ceiling instead:
  an XFS project-quota recipe for operators (`docs/OPERATIONS.md`, "User drives on
  Docker") and `threatmodel/THREAT-MODEL.md` #36 updated to "accepted with a
  recipe" — the case `types.StorageEnforcementFilesystem` reserved a value for ·
  **team-shared drives**
  (one object, many principals — it breaks the `UNIQUE(subject_type, subject)` +
  LIMIT-1 resolver invariant and needs its own design round) · multiple drives per
  principal · drive as a workspace-library source · a top-level nav item · per-user
  uid / Kerberos / cifs `multiuser` · wardynd performing NFS/SMB mounts itself ·
  csi `subDir` templating · cloud-drive providers (rclone/OneDrive) · a read-only
  `Runner.ProbeDrive` so create, preflight and `/me` share one probe instead of
  only dispatch knowing (`FOLLOWUPS.md:18`) · **TM #34**, the drive object-name
  separator collision: 0.7.1's migration `0061` closed the slug-uniqueness half,
  and the fixed-width drive id that closes the rest re-homes everyone who already
  has storage under a minted name. Documentation debt on the same feature, by its
  own `FOLLOWUPS.md` ids: `:3` and `:8` fixed in `docs/design/user-drives-prompt.md`
  (the object name is now documented as derived, never taken as typed; the
  preview's backend-unavailable answer is now documented as the shipped 422, not
  "previewed as a warning"). `:11` was a miscall, not a defect: `code-block.tsx`'s
  `makeMono` is one factory, and `drives/display.tsx`, `governance/display.tsx`,
  `setup/access-panel.tsx` and `new-run/workspace-card.tsx` are four configured
  bindings of it, never copies — nothing to deduplicate. Still open: `:7` (the
  `disk_mib` text-to-speech trap in a demo script — not locatable anywhere in the
  tree) and the three demo-track lines `:4`, `:5` and `:6`, which ride the demo
  bullet below.
- **Identity and authz.** Other people's subjects on records (`known_principals`)
  · PF-48 resident credential lanes above a re-asserted ceiling (architectural) ·
  **TM #38** (a per-user API token's group snapshot never refreshes — the
  demoted-admin window) · **TM #39** (an IdP-FILTERED group claim is
  indistinguishable from a complete one) · governance residuals PF-12/15/16/1 (by
  design) · the three accepted egress residuals B6/B7/B8 (unassigned-member stored
  policy, IPv6 redirect literal, SNI-swap probe) · **R4-F110** (a WebSocket close
  code `4403` on attach: every attach authz refusal is an HTTP 403 BEFORE the
  upgrade by deliberate invariant, so a 4403 needs a decision to open a socket for
  an unauthorized caller) · **R1-F289** (a DB-clock cookie `iat`; the shipped
  comparison-time fix is recorded as safe to leave standing).
- **Deployment.** Subscription/managed runs through the internal model gateway ·
  BYO-Bedrock for members · an air-gapped video mirror + config-driven CSP
  · the gateway auth-scheme seam · `ssh_key` clone-only vs bind-mounted workspaces
  (F11) · age-key rotation (F12) · react-router 8 (F13) · the Kata/TPROXY/io_uring
  quick-hits (F14) · the k8s parity list (F23, on the v1.0 row below) · ADO
  per-repo scoping (F9 — impossible without an ADO minting API, so it stays a
  documented ceiling, and 0.7.2's provider rows bound the ADDRESS, never the
  token's own scope).
- **Workstream C follow-ons.** Per-user BEARER/API-key credentials (`per_user` is
  `bedrock_sso`-only in 0.7.2) · explicit opt-in cross-mechanism fallback (needs a
  policy field, a ceiling term and an audit story) · the rest of Phase B (the
  LEGACY no-block path still ships refresh fields in the sandbox cache; a mid-run
  renewal channel for runs longer than one access token) · a background renewer, if
  dispatch-time refresh proves insufficient.
- **R3/R4 residue with a written shape, deliberately not pulled.** B3-F073
  (`llm_inspection` scan-budget policy fields + their POLICIES rows) ·
  F074-hardening (re-deriving the docker/k8s hardening-cap rationale) · the console
  copy/state items F141-panes, F143-control, F132-followup, F004-followup, F027-a,
  F069-a, F051-a/F092-a, F142-copy, F112-nit, F070 and F093 — one owner mock batch,
  which **F049-mock** (the drives-mock State 3b) joins: it was authored as a mock
  STATE on the providers mock rather than built, so it is the same sitting's to rule
  · **R4-F009**: the `setup_items` preflight field is fetched on every Review and
  has no consumer, but deleting it also strips five `preflight_test.go` cases'
  real coverage of `deriveSetupItems`, so the disposition is to delete the field
  AND move that coverage onto `deriveSetupItems` directly (or build the rail that
  consumes it) — not the quick win it was filed as · **R4-F077's pagination
  control**: the recording-metadata projection shipped, the Recordings screen's
  own pagination did not, and it is filed for a mock round rather than invented
  here · **a second terminal-escape chord for AltGr layouts**: 0.7.2's `Ctrl+]`
  exits the cockpit terminal on US-style keyboards, but on DE/FR/ES layouts `]`
  needs AltGr, the browser sees `altKey`, and the binding does not fire — so the
  WCAG 2.1.2 trap stands on those layouts. The fix is a second chord, which is a
  canon decision (one spelling reaches the on-screen hint) rather than a
  keyhandler change, so it waits for the sitting that rules it.
- **Verification debt.** R5's 137 and R6's 70 claim passes · R3/R4 round 2 · R7
  round 2 · R1's 62 fixed-but-unverified · the Low/Info residue · the TEST-GAPS
  chronic backlog · **two 0.7.2 browser rows that this harness cannot deliver**:
  "a member signs in to AWS SSO and launches" and "an expired shared credential
  refuses the run with the named sentence". Both need a real OIDC session — the
  capability resolver derives its subjects from the OIDC context alone, so the
  admin bearer the Playwright harness holds has none and the refusal it reaches is
  a different one. They are pinned in Go against the mechanism itself
  (`runs_dispatch_llm_mechanism_test.go`, `awssso_refresh_test.go`) and walked in a
  live browser against a real tenant before the tag, rather than left to a
  Playwright row that would assert the harness instead of the product.
- **Dev-box tooling** (a decision, not a product gap — none of it reaches a
  deployment). The `.wslconfig processors=24` bump for the build host · the
  verification harness's own two: the ledger's `init --resume-from` gap, and the
  review persona's re-arm on compaction. Recorded so they are not re-discovered as
  findings in 0.8's rounds.
- **Demo and video track** (not release-gated, owner-timed). The episode 00 script
  gate · the dialog rewrite set · the F101 mock round · the seventeen unrecorded stubs
  · the 03c act-3 rewrite (a PAT is brokered by default now) · the five held videos'
  re-take · the 04c re-take · the re-record impact tool, caption lint and quota
  probe.
- **Ops-gated** (owner hardware/tenants). The real-tenant Entra walk · the
  real-AWS PrivateLink walk · adopter acceptance · the macOS `.pkg` (Apple
  Developer ID) · an MDM vendor example · a real-Mac launchd smoke run · a real
  playback-engine proof · a live `disk_mib` walk on an xfs+pquota Docker host.

| Milestone | Scope |
|---|---|
| **v0.8** | **Alpha RC.** The follow-through on 0.6/0.7 — the remaining enterprise-deployment enhancements, tools, and pieces — and the **last planned release candidate before the alpha go-live** |
| **v1.0** | SPIRE identity provider (the `identity.Provider` seam ships; the SPIRE impl does not) · OpenBao secret store (same, for `secretstore.Store`) · L3 MCP/tool gateway · arbitrary-domain L2 TLS interception (targeted LLM/registry MITM already ships, opt-in) · cloud STS federation · OTLP/OCSF SIEM sinks (file/webhook/syslog sinks already ship) · Docker/Compose L1 default-deny via nftables (the k8s target's L1 already ships — NetworkPolicy, boot-time-canary-enforced, blocking `169.254.169.254`; Docker/Compose still relies on L0 structural confinement alone) · HA completion — closing the still-open per-process blockers a second replica hits (chiefly the in-memory, fail-open secret-masking registry; see [docs/OPERATIONS.md](docs/OPERATIONS.md)'s "One replica, by construction" for the exact list and what v0.5 already closed) · k8s substrate parity with Docker: BYOI/devcontainer builds, `local_dir` mounts, a per-pod PIDs limit, and a k8s ground-truth correlator (see [deploy/helm/wardyn/README.md](deploy/helm/wardyn/README.md)'s "Known gaps") · CC3/Vault (Kata) packaged and GA — experimental today · Cilium `toFQDNs` · signed action receipts (the hash chain itself ships — migration `0047`) · separation of duty on the control plane |
| **v1.0 (git-token ref confinement)** | **Token-side** branch-namespace confinement for minted git tokens — the proxy-side push-ref check ships DEFAULT-ON (`agent-run` names the run branch `wardyn/<run-id>/work`; `WARDYN_GIT_BROKER_ENFORCE_BRANCH_NS=false` opts out) and binds the brokered App lane, but the installation token itself cannot self-restrict to a ref prefix. What now ships, opt-in: Wardyn reads a GitHub repository ruleset back (`VerifyRefRuleset`, `internal/broker/ruleset.go`), grades it on the setup checklist (never `fail`), and can refuse every `github_token` mint until one verifies (`WARDYN_GITHUB_REQUIRE_REF_RULESET`, default off). What's still not built: Wardyn never creates or holds the ruleset itself — that needs repo-admin access it deliberately does not request, so creating one stays a manual operator step (`docs/POLICIES.md`) — and the gate defaults off, so an operator who does neither still has an unbound token. `git_pat`/`ssh_key` remain outside any receive-pack parser regardless of the ruleset (`threatmodel/THREAT-MODEL.md` asset #4) |

### Named gaps without a milestone

These are known, documented ceilings. They are listed so they are not mistaken for
shipped behavior; none is scheduled.

- **Seventeen of the 23 catalogued demo episodes are still unrecorded stubs**
  (`ui/src/app/lib/demo-videos.ts`'s `EPISODES`; `cmd/wardynd/demo_videos_guard_test.go`
  pins the count). 0.7 organised the install paths around three audiences —
  single-user, multi-user, and joining a Wardyn someone else runs — and the
  episodes now play inside Getting Started itself (a manifest per episode,
  watched only on explicit click). `00` (front door), `03c` (authorized-then-issued),
  `04` (add a workspace), `04b` (a member's own workspace), `04c` (who may do
  what — governance profiles and role mappings), `04d` (a member's own drive),
  `06`–`10` (first run, interactive, autonomous, record, approvals), `11` (CI),
  `12` (audit and attach), `12b` (admin operations), `02b` (managed desktop),
  `02c` (cloud) and `13` (SSH into a cluster run) still carry `tag: null` —
  their grader arms and persona quizzes are written, the takes are not shot.

- **`--agent` is still required for a run that names no image.** **0.7 narrowed
  this rather than closing it.** A `task_mode=exec` run that carries an `--image`
  (or an attached workspace) no longer needs an agent — which was the case that
  made the CLI read AI-first, and which our own CI docs used to demonstrate the
  workaround for. What remains: a bare `wardyn run --task-mode exec --task 'echo
  hi'` with no image still 400s, because there is nothing to run it in.
  The residual is therefore "the run must name SOMETHING", not "the run must name
  an agent". Deliberately not defaulted further: defaulting the agent to
  `claude-code` would make an agentless run eligible for the operator's live
  subscription credential, and defaulting it to `byoa`/`none` resolves to an
  image that is not published.
- **Azure DevOps and GitLab PATs are non-resident but not scoped.** **0.7 built
  the never-resident half**: a `git_pat` for a non-GitHub forge is minted
  proxy-side and injected on the outbound leg, so the credential no longer enters
  the sandbox. What this entry now names is the half that remains and cannot be
  built the same way: ADO has no token-minting API, so the operator PAT's scope
  is the boundary. Per-repo, auto-expiring scoping is achievable on GitHub
  because an installation token can be minted narrow; it is not achievable on a
  PAT Wardyn merely holds. The `WARDYN_GIT_PAT_BROKER` broker is therefore a
  residency control, never a least-privilege one.
- **Proxy-side injection of the Bedrock SSO bearer.** Would make the SSO token
  never-resident; the derived role credentials stay resident regardless, because
  SigV4 signs in-process.
- **The `ssh_key` grant is clone-only and does not fit a bind-mounted workspace.**
  The key is written just before the clone and wiped right after, so a `local_dir`
  workspace — which has no clone step — leaves interactive SSH pull/push
  unauthenticated. Rewrite the remote to HTTPS, or accept clone-only SSH.
  ([field report](docs/adoption/corp-network-onboarding-findings.md))
- **Team mode as a packaged, sealed multi-user product** — as opposed to the
  RBAC that ships IN the control plane today (see
  [docs/OPERATIONS.md](docs/OPERATIONS.md)'s "Multi-user: who can change what",
  and [docs/MEMBERS.md](docs/MEMBERS.md) for what a member themself can do).
  Admin/member roles and owner scoping are real and shipped (v0.5), and v0.6
  added capability grants over a user, an IdP group, or everyone (see "What
  v0.6 shipped") — which is authorization detail on top of those two roles, not
  a tenancy model, and 0.6 added per-user `wdn_` API tokens (personal,
  independently revocable — see "What v0.6 shipped"). What's still speculative,
  no design in the tree: SAML/SCIM provisioning, an organization/tenant
  structure, and
  CUSTOM roles beyond admin/member. The admin token and local mode remain the
  same shared credential they always were: always-admin, no per-human identity,
  no separation of duty from a real admin user (v1.0's row, above).
- **The SSH gateway's admin override is a bounded-stale stamp, weaker than
  the web terminal's live check.** `sshAuth` grants an admin's own registered
  key an override — `run.created_by == principal` OR (`key.role == admin` AND
  `key.role_checked_at` no older than `WARDYN_SSH_ROLE_TTL`, migrations
  `0043_ssh_key_role.sql` and `0046_ssh_key_role_checked_at.sql`). The stamp
  is no longer registration-time-only: every OIDC login re-stamps `role` and
  `role_checked_at` for all of that principal's keys (`oidc.Config.OnLogin`),
  and the TTL (default `24h`) expires a stamp on its own even if the human
  never logs in again — closing the "keeps the override forever until
  re-registered" gap this bullet used to name. What's left: the gateway still
  never reads the human's role LIVE at connect time, unlike the web
  terminal's `requireOperator` gate — SSH carries no session for that gate to
  read — so a demotion can still ride an unexpired stamp for up to one TTL
  window. Overrides are audited distinctly (`ssh.auth` carries
  `override:true`), and the ceiling is documented, not silently assumed away,
  in `docs/SSH.md`'s Bounds section and `threatmodel/THREAT-MODEL.md`
  residual #15.
- **The legacy `sources`/`base_image` workspace columns have no drop date, and
  the migration number reserved for it is gone.** 0.4.5's source-library split
  (migration `0031_source_library.sql`) kept the old embedded columns live for
  compatibility and reserved migration number `0032` in its own comment for
  the column-drop migration, "which must ship in a LATER release, never this
  one." That number is now taken — `0032_attach_tickets_token_sha256.sql`
  shipped in v0.5, and the v0.5 k8s/SSH merge renumbered its own new
  migrations up past it (`0033_ssh_public_keys.sql`, `0034_attach_ticket_role.sql`)
  — and v0.6 took the numbers through `0043_ssh_key_role.sql`, so the eventual
  drop migration needs a fresh number whenever it's scheduled. That
  floor moves with every release; read the migrations directory rather than
  this sentence. Nothing depends on it happening by any particular release; it's
  listed here so the stale "is 0032" comment in `0031_source_library.sql`
  isn't mistaken for a live plan.
- **Age-key rotation is offline and operator-driven.** `wardynd -rotate-age-key`
  now re-encrypts every stored secret to a fresh identity in one transaction
  ([docs/OPERATIONS.md](docs/OPERATIONS.md)'s "Rotating the age key"), so the old
  "no rotation path at all" ceiling is gone. What remains: the daemon has to be
  **stopped** for it, and nothing enforces that — no wardynd holds a
  process-lifetime advisory lock, so the tool can refuse a second concurrent
  rotation but cannot see a serving process. There is also no scheduled or
  automatic rotation, and no `wardyn secret rotate`: the CLI deliberately never
  touches the key.
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
  before the member launches.

  **Shipped in 0.7:** the pure-BYOK-for-members flow this bullet used to name as
  a suggested fix — a member's own stored provider-convention key now survives
  with no operator integration and no workspace requirement behind it (per-
  principal secrets, migration `0050`; see the 0.7 CHANGELOG entry). This is
  still not the member-mode model-access story on the desktop tier's m′ profile,
  which reaches a model via **Bedrock** — daemon-level MDM-set configuration
  rather than a per-member credential, routing around this gate entirely
  (`docs/DESKTOP.md` "Model access on m′").
- **A shell banner and a substrate-honest `ConfinementChip` reading the k8s
  NetworkPolicy canary posture.** The Go field (`/healthz.network_policy`) and
  the boot-time audit event ship in 0.7; nothing in the console or the CLI
  reads either yet.
- **An air-gapped video mirror, and a config-driven CSP to match it.** The demo
  episodes stream from a fixed GitHub release host; a cluster with no outbound
  internet has no way to serve or allow them.
- **Member-BYO-Bedrock.** Per-principal secrets (0.7) cover the api-key
  provider-convention lane; the four Bedrock/SigV4 credential names stay
  admin-only for member writes.
- **A gateway needing its own request-header shape.** The internal model
  gateway (0.7) forwards the sandbox's existing provider headers unchanged;
  a gateway that expects a different header or auth scheme has no seam yet.
- **Subscription and Wardyn-managed runs through the internal model gateway.**
  0.7 scopes the gateway to the api-key lane only; those lanes need
  `deploy/images/claude-code/agent-run` to honour an explicit operator-set
  base URL, which needs an image rebuild — 0.8.
- **The Network step rendering "N trusted CA certs."** `/setup/status`
  carries the count (0.7); no console reader exists yet.

## What is not on the roadmap

- **Bring-your-own arbitrary Kubernetes manifests.** One blessed Helm chart, or nothing.
- **A feature that passes on only one target.** The parity rule: a feature is not
  done until it passes the conformance suite on both Docker and kind — CI now
  gates on both, but that is a floor, not a one-time proof (see
  [ARCHITECTURE.md](ARCHITECTURE.md)'s "Parity rule"). The k8s substrate is not
  at overall feature parity with Docker yet even though it passes conformance
  (see [deploy/helm/wardyn/README.md](deploy/helm/wardyn/README.md)'s "Known
  gaps"); a new feature still owes both targets, or an honest, explicit skip.
- **A paid or open-core edition.** Apache-2.0 everything.
