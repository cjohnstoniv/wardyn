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
| **v0.4** | Containerized setup as the default, credential CLI, YAML policies, container workspaces with their own model credentials, Bedrock SSO, and the corporate-network build/egress lanes | **Shipped (pre-alpha)** — see [CHANGELOG.md](CHANGELOG.md)'s `[0.4.0]` through `[0.4.5]` entries |
| **v0.5** | Kubernetes runner substrate + the Helm chart's first sandbox-capable deploy, conformance green on a real cluster, native SSH access into a run, real admin/member RBAC with owner scoping, signed+published release images | **Shipped (pre-alpha)** — tagged `v0.5.0`, 2026-08-18 (see [CHANGELOG.md](CHANGELOG.md)) |
| **v0.6** | **The enterprise-POC base: cloud deployment + real permissioning.** Capability grants (four kinds, per-kind enforcement switches, IdP groups), Kubernetes as the base deployment story (one-command kind quickstart, day-2 ops, `/readyz`), terminals beyond the browser (`wardyn ssh`, a kind-proven SSH lane, an admin override), governed UI sandboxes (a ticket-gated loopback relay + a code-server image), and the ground-truth counter fix | **Shipped (pre-alpha)** — `v0.6.0` (see [CHANGELOG.md](CHANGELOG.md)); the release supersets `prep/v0.6` with the demo-video series. The daemon-free merge gate (`make ci`) is green at the release tip minus DCO sign-offs; `make test-e2e` carries 10 failures that reproduce on a pre-merge baseline on the same host — a pre-existing lane defect, not a 0.6 regression |
| **v0.7.0** | **Governance an org can delegate, on hardware it owns.** Assignable governance profiles (a named ceiling bound to a person, a group, or everyone) and a `security_admin` tier that can be handed the verdict without being handed the deployment, the rest of enterprise desktop deployment (systemd installer, MDM-distributable packages, the member-mode envelope, a reachable SSH gateway, digest-pinned upgrades), per-tool policy, never-resident git PATs for non-GitHub forges, and the corporate-network last miles — TLS-inspection root, internal model gateway, PrivateLink Bedrock | **Shipped (pre-alpha)** — `v0.7.0`, 2026-09-09 (see [CHANGELOG.md](CHANGELOG.md)). The macOS `.pkg`, the MDM vendor example and the real-Mac smoke run stay **operator-gated** and are not in it |
| **v0.7.1** | Patch: the console header read the raw OIDC `sub` instead of the IdP's `name` claim for an SSO user; a stock `helm install` (persistence off) crash-looped on an empty recording dir hitting a read-only root filesystem | **Shipped (pre-alpha)** — `v0.7.1`, 2026-09-11 (see [CHANGELOG.md](CHANGELOG.md)) |
| **v0.7.2** | **Workspace Providers** — one admin object for which git/model providers are enabled, for whom, inside what bounds — an agent roster with per-person model credentials, ephemeral disk enforcement on Kubernetes, and seven field-report fixes from a private-endpoint Kubernetes estate | **Shipped (pre-alpha)** — `v0.7.2`, 2026-09-12 (see [CHANGELOG.md](CHANGELOG.md)) |
| **v0.7.3** | A second field report from the same estate: the per-user AWS SSO lane can no longer sign with the wrong identity (account/role pinned, enforced at three doors), the Bedrock check texts and admin sign-in door stopped conflating a deployment-wide fact with a per-person credential gap, and the CSRF Origin guard now applies in every mode | **Shipped (pre-alpha)** — `v0.7.3`, 2026-09-15 (see [CHANGELOG.md](CHANGELOG.md)) |
| **v0.7.4** | **Governance hardening over the whole surface a member or an operator's identity touches.** Per-run credential residency and revocation, the five findings of the 0.7.3 field report plus the owner's two testability asks, a kind-provable AWS SSO test path, an admin's own "view as member", and a repo-wide review campaign's fixes across the runner substrate, the egress proxy and the console | **Shipped (pre-alpha)** — `v0.7.4`, 2026-09-16 (see [CHANGELOG.md](CHANGELOG.md)) |
| **v0.7.5** | A third field report from the same private-endpoint Kubernetes estate (Entra SSO, one enabled roster row: `claude-code` / `bedrock_sso` / `per_user`): the console stops asserting things that are false on that deployment shape (the New Run rail's credential-residency and Recording claims, the member's "Your model key" card, and Getting Started's lede), an admin can preview the not-signed-in member state, the AWS sign-in sandbox now runs the sign-in itself with every attach path joining it, a slow sign-in start no longer reads as unreadable and a new sign-in supersedes an orphaned one, a rebuilt Claude Code image boots without parking approvals on the CLI's own bootstrap, and on Kubernetes an autonomous run's `/tmp` and `/home/agent/work` are now inside `disk_mib` (narrowed, not closed) | **Shipped (pre-alpha)** — `v0.7.5`, 2026-09-17 (see [CHANGELOG.md](CHANGELOG.md)) |
| **v0.7.6** | "The facts exist; connect them to the person" — a fourth field report from the same estate: an actionable model-access state now rides a banner on every screen instead of only Getting Started, a run refused for a dead model credential offers the sign-in instead of directions to it, a slow start says what it is waiting on instead of a poll-tick guess, the AWS sign-in tab opens and closes itself, a spent refresh token stops grading `live` for days, and wardynd's own outbound calls (OIDC, AWS SSO renewal, Entra sync) gain a scoped corporate-proxy knob that does not share `HTTPS_PROXY`'s process-wide blast radius; a mid-run credential lapse holding the run instead of killing it ships behind a kill switch | **Shipped (pre-alpha)** — `v0.7.6` (see [CHANGELOG.md](CHANGELOG.md); tag `v0.7.6`, 2026-09-18) |
| **v0.7.7** | A fifth field report from the same estate: with an expired AWS SSO session, Launch bounced the console to Getting Started. The setup gate stops grading the two per-person model-provider rows, a create-time refusal carries the machine-readable reason `model_credential`, the console opens the sign-in from that refusal and relaunches the same run, and the launch redeems an expired session at the click instead of admitting a spent one | **Shipped (pre-alpha)** — `v0.7.7`, 2026-09-18 (see [CHANGELOG.md](CHANGELOG.md)) |
| **v0.7.8** | Three field reports and the terminal: the setup gate's blocking decision moved server-side (`SetupCheck.Blocking`), the shipped confinement floor is CC1 with the strongest installed class as the default, a dial refusal names its own cause and hop, AWS-lane refusals answer in SDK-readable JSON, `wardyn attach` rides a single-use ticket, and the terminal's holder and focus defects are fixed | **Shipped (pre-alpha)** — `v0.7.8`, 2026-09-19 (see [CHANGELOG.md](CHANGELOG.md)) |
| **v0.7.9** | Patch: the egress proxy shared one TLS config with the sidecar's control-plane client, so on a corporate-CA install it offered HTTP/2 it could not speak and every re-originated request to a peer that accepted the offer failed; the proxy now speaks HTTP/2, handles a peer that speaks it unasked, and files a protocol mismatch as its own refusal instead of a dial failure | **Shipped (pre-alpha)** — `v0.7.9`, 2026-09-21 (see [CHANGELOG.md](CHANGELOG.md)) |
| **v0.7.10** | **Per-person Azure DevOps access on Entra ID**: a run reaches Azure DevOps as the person who started it, with their own sign-in captured at console login and never placed in the sandbox; every REST call and git push is checked against a plain-language capability the run was granted, and a request beyond it is held for approval once or for the run. Also: an SSO-only console posture, Bedrock policy-deny and throttle refusals named on the failed run | **Shipped (pre-alpha)** — `v0.7.10`, 2026-09-22 (see [CHANGELOG.md](CHANGELOG.md)) |
| **v0.7.11** | Patch: Azure DevOps projects and repositories whose names carry spaces or other permitted characters (`Payments Platform`, `Card Auth (v2).Service`) import, launch, clone, fetch and push; every door stores one spelling of the address, and approvals name the repository the same way on the REST and git paths | **Shipped (pre-alpha)** — `v0.7.11`, 2026-09-22 (see [CHANGELOG.md](CHANGELOG.md)) |
| **v0.7.12** | Patch: stored credentials are sealed with AES-256-GCM per row, bound to their owner and name (envelope v1, a one-way conversion on first boot); the control-plane → proxy hop that carries credential values is TLS 1.3, pinned to a CA wardynd mints; every secret-carrying boot setting accepts a `_FILE` path (Vault Agent / CSI), with an opt-in chart mode; an Azure DevOps address's host now ends at `?` or `#` | **Shipped (pre-alpha)** — `v0.7.12`, 2026-09-23 (see [CHANGELOG.md](CHANGELOG.md)) |

## Planned

Everything below is **planned, unbuilt, and undated**. Where a seam exists but no
implementation does, [docs/PLUGGABILITY.md](docs/PLUGGABILITY.md) says so per row.

v0.8 is the remaining path to alpha. The cloud base and permissioning 0.6 owed
are shipped, and so is 0.7's governance and desktop work (above, through
`v0.7.12`), so what is left below is the alpha RC and beyond.

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

**Planned for 0.9: hybrid local + remote.** Today Wardyn has two tiers that do
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
[docs/design/hybrid-0.8.md](docs/design/hybrid-0.8.md). **0.8 ships the seams, not the rollout:** device
enrolment into an org control plane and the one audit stream (the laptop's audit rows federated
to the org) have landed, and 0.8's authorization kernel is built to become the control plane's
decision API (decisions that serialize, stable refusal codes, a versioned list of resource
kinds, a principal that carries its device and origin). **The rollout is 0.9:** the org control
plane deciding for enrolled laptops (signed policy snapshots for offline use, the org deciding
anything that touches org resources), per-run placement between the laptop and the org's
cluster and mixing the two, and the disk link. Tracked on the `0.9.0` milestone.

| Milestone | Scope |
|---|---|
| **v0.8** | **Alpha RC.** The follow-through on 0.6/0.7 — the remaining enterprise-deployment enhancements, tools, and pieces — and the **last planned release candidate before the alpha go-live** |
| **v0.9** | **Hybrid local + remote.** MDM-managed laptops enrolled into a remote org control plane on Kubernetes: the org's authorization kernel decides for every enrolled daemon (signed policy snapshots so a laptop keeps working offline under its last policy; the org decides anything that touches org resources), per-run placement between the laptop and the org's cluster, and mixing the two under one identity, one ceiling and one audit stream · the disk link (a local directory in a remote sandbox, a remote drive read locally) · the `member` role alias removed (0.8 warns) |
| **v1.0** | SPIRE identity provider (the `identity.Provider` seam ships; the SPIRE impl does not) · OpenBao secret store (same, for `secretstore.Store`) · L3 MCP/tool gateway · arbitrary-domain L2 TLS interception (targeted LLM/registry MITM already ships, opt-in) · cloud STS federation · OTLP/OCSF SIEM sinks (file/webhook/syslog sinks already ship) · Docker/Compose L1 default-deny via nftables (the k8s target's L1 already ships — NetworkPolicy, boot-time-canary-enforced, blocking `169.254.169.254`; Docker/Compose still relies on L0 structural confinement alone) · HA completion — closing the still-open per-process blockers a second replica hits (chiefly the in-memory, fail-open secret-masking registry; see [docs/OPERATIONS.md](docs/OPERATIONS.md)'s "One replica, by construction" for the exact list and what v0.5 already closed) · k8s substrate parity with Docker: BYOI/devcontainer builds, `local_dir` mounts, a per-pod PIDs limit, and a k8s ground-truth correlator (see [deploy/helm/wardyn/README.md](deploy/helm/wardyn/README.md)'s "Known gaps") · CC3/Vault (Kata) packaged and GA — experimental today · Cilium `toFQDNs` · signed action receipts (the hash chain itself ships — migration `0047`) · separation of duty on the control plane |
| **v1.0 (git-token ref confinement)** | **Token-side** branch-namespace confinement for minted git tokens — the proxy-side push-ref check ships DEFAULT-ON (`agent-run` names the run branch `wardyn/<run-id>/work`; `WARDYN_GIT_BROKER_ENFORCE_BRANCH_NS=false` opts out) and binds the brokered App lane, but the installation token itself cannot self-restrict to a ref prefix. What now ships, opt-in: Wardyn reads a GitHub repository ruleset back (`VerifyRefRuleset`, `internal/broker/ruleset.go`), grades it on the setup checklist (never `fail`), and can refuse every `github_token` mint until one verifies (`WARDYN_GITHUB_REQUIRE_REF_RULESET`, default off). What's still not built: Wardyn never creates or holds the ruleset itself — that needs repo-admin access it deliberately does not request, so creating one stays a manual operator step (`docs/POLICIES.md`) — and the gate defaults off, so an operator who does neither still has an unbound token. The ruleset is a GitHub-only, `github_token`-only mechanism regardless: `git_pat` gained its own receive-pack parsing since 0.7.2 (`WARDYN_GIT_PAT_BROKER_ENFORCE_BRANCH_NS`, default off) and its own content rules since 0.8 (`push_rules`, both brokered lanes) — only `ssh_key` remains structurally outside any receive-pack parser, since git's SSH transport has no broker seam (`threatmodel/THREAT-MODEL.md` asset #4) |

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
  and [docs/USERS.md](docs/USERS.md) for what a member themself can do).
  Admin/member roles and owner scoping are real and shipped (v0.5), and v0.6
  added capability grants over a user, an IdP group, or everyone (see
  CHANGELOG.md's `[0.6.0]` entry) — which is authorization detail on top of
  those two roles, not a tenancy model, and 0.6 added per-user `wdn_` API
  tokens (personal, independently revocable — same entry). What's still
  speculative, no design in the tree: SAML/SCIM provisioning, an
  organization/tenant structure, and CUSTOM roles beyond admin/member. The
  admin token and local mode remain the
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
  (`filterUserGrants`, `internal/api/inline_policy.go` — the secret-exfil
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
  carrying the pairings that went (`auditUserPolicyDrops`, same file). No
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
- **Drives beyond one per person.** Team-shared drives (one object, many
  principals — breaks the `UNIQUE(subject_type, subject)` + LIMIT-1 resolver
  invariant and needs its own design round) · multiple drives per principal ·
  cloud-drive providers (rclone/OneDrive) · csi `subDir` templating · wardynd
  performing NFS/SMB mounts itself · run-row drive persistence (D3). No
  0.8/0.8.1 issue tracks any of these. Self-service reset, per-user
  uid/Kerberos/cifs `multiuser`, and a same-drive concurrent-run collision
  warning are current, documented non-goals, not gaps —
  [docs/USERS.md](docs/USERS.md) ("there is no self-service reset: a drive
  you have poisoned is reclaimed by your admin with a documented command, so
  ask") and [docs/design/user-drives-prompt.md](docs/design/user-drives-prompt.md)'s
  "Do not design" list ("No Reset", "No share credentials, no per-user uid, no
  Kerberos", "No collision warning").
- **Identity and authz residuals accepted without a build.** `known_principals`
  (other people's subjects on records) · PF-48 (resident credential lanes above
  a re-asserted ceiling, architectural) · **TM #39** (an IdP-FILTERED group
  claim is indistinguishable from a complete one) · governance residuals
  PF-12/15/16/1 (by design) · the three accepted egress residuals B6/B7/B8
  (unassigned-member stored policy, IPv6 redirect literal, SNI-swap probe) · a
  WebSocket close code `4403` on attach (every attach authz refusal is an HTTP
  403 BEFORE the upgrade by deliberate invariant, so a `4403` needs its own
  decision to open a socket for an unauthorized caller) · a DB-clock cookie
  `iat` (the shipped comparison-time fix is recorded as safe to leave
  standing). The per-user API-token group-snapshot refresh and the per-user
  Bedrock-bearer widening this residual used to carry alongside them are both
  shipped (#152, #153).
- **The AWS SSO dispatch credential-refresh mechanism's remaining half.**
  Per-user BEARER/API-key credentials (`per_user` is `bedrock_sso`-only in
  0.7.2) · an explicit opt-in cross-mechanism fallback (needs a policy field,
  a ceiling term and an audit story) · the rest of the mid-run renewal design
  (the legacy no-block path still ships refresh fields in the sandbox cache; a
  renewal channel for runs longer than one access token) · a background
  renewer, if dispatch-time refresh proves insufficient.
- **Review-round findings with a written shape, deliberately not pulled.** An
  `llm_inspection` scan-budget policy (fields and their POLICIES rows) and a
  re-derivation of the docker/k8s hardening-cap rationale have no 0.8/0.8.1
  issue. The console copy/state residue this bullet used to also carry is
  tracked by #157 (part of #84): five of its twelve findings are recoverable
  and covered there, the rest are struck rather than carried as unexplained
  ids (`docs/design/0.8/PLAN.md`'s Risks section). The `setup_items` preflight
  field and the Recordings screen's own pagination this bullet also used to
  carry are both already resolved, not gaps: `setup_items` has a real
  consumer (`wardyn run --dry-run`; #172, closed — the premise that it had
  none was wrong) and Recordings pagination shipped (#159). The second
  terminal-escape chord this residue used to carry alongside them shipped as
  `Ctrl+Shift+Backspace` (#133).
- **Verification debt from private review rounds.** A backlog of claimed-fixed
  findings from prior review rounds that were never re-verified, plus a
  Low/Info-severity residue and the `TEST-GAPS` chronic backlog, has no
  0.8/0.8.1 issue. The two 0.7.2 browser rows this debt used to also carry ("a
  member signs in to AWS SSO and launches", "an expired shared credential
  refuses the run with the named sentence") are resolved differently, not
  carried forward: both need a real OIDC session no Playwright harness holds,
  so they are pinned in Go against the mechanism itself
  (`runs_dispatch_llm_mechanism_test.go`, `awssso_refresh_test.go`) and were
  walked in a live browser against a real tenant before the tag.
- **Dev-box tooling** (a decision, not a product gap — none of it reaches a
  deployment). The `.wslconfig processors=24` bump for the build host · the
  verification harness's own two: the ledger's `init --resume-from` gap, and
  the review persona's re-arm on compaction. Recorded so they are not
  re-discovered as findings in 0.8's rounds.
- **Demo and video track** (not release-gated, owner-timed; the seventeen
  unrecorded episode stubs are tracked above, this section's first bullet).
  The episode 00 script gate · the dialog rewrite set and its owner mock round ·
  the 03c act-3 rewrite (a PAT is brokered by default now) · the five held
  videos' re-take · the 04c re-take · the re-record impact tool, caption lint
  and quota probe.
- **Ops-gated** (owner hardware/tenants). The real-tenant Entra walk · the
  real-AWS PrivateLink walk (partially tracked by #706) · adopter acceptance ·
  the macOS `.pkg` (Apple Developer ID) · an MDM vendor example · a real-Mac
  launchd smoke run · a real playback-engine proof · a live `disk_mib` walk on
  an xfs+pquota Docker host.

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
- **Interactive tool-approval routing through the toolgate FSM.** Spiked and
  refused, not deferred: `wardyn-toolgate` routes only a non-interactive run's
  tool calls to the approval FSM, upstream pins `claude`'s
  `--permission-prompt-tool` to non-interactive use, and the only alternative
  — a hook — fails *open* on a timeout, the wrong default for an approval
  gate. Self-service value collapses anyway: the human deciding the prompt can
  already attach to the run and answer it directly. What shipped instead: an
  interactive run's `tool_approvals=hold` is refused with a 400 naming the
  field, rather than silently accepted and discarded.
