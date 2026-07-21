# Changelog

All notable changes to Wardyn are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); Wardyn is **pre-alpha**
and does not yet follow semantic versioning (interfaces are not stable).

## [Unreleased]

## [0.4.2] — 2026-07-20

A follow-up to 0.4.1 from the same adopter, now running the containerized stack on a
corporate laptop end to end. The full report — including the gaps still open — is in
[docs/adoption/](docs/adoption/corp-network-onboarding-findings.md). One entry below is a
regression 0.4.1 introduced; two more were bugs that had been silent for longer.

### Fixed

- **An unreachable corporate proxy no longer hangs an approved request.** When the
  configured upstream proxy accepted the TCP connection but never answered the `CONNECT`,
  wardyn-proxy waited forever: the handshake read had no deadline, and the MITM path had
  already told the agent `200 Connection Established`. An operator saw an *approved* egress
  sit there with nothing to act on. The handshake is now bounded and a stalled upstream
  surfaces as a normal dial failure — deny + 502, logged. This is the reported "200 then
  hang" on hosts whose connectivity client binds its proxy to loopback only.

- **The `make setup` UI fallback no longer reinstalls.** 0.4.1 taught `make setup` to build
  the UI on the host when the registry can't serve pnpm — but it retried via `make ui`, which
  reinstalls, and the reinstall's postinstall scripts fetch platform-specific native binaries
  (`@tailwindcss/oxide-*`, `@esbuild/*`) that a partial mirror also refuses. So the fallback
  died anyway, with `node_modules` already complete. It now rebuilds from an existing
  `node_modules` when the lockfile matches, and only reinstalls when the tree is absent or
  stale. Note the ceiling: a *fresh clone* has no `node_modules`, so this fixes the second and
  later `make setup`, not a cold start.

- **The New Run wizard no longer claims you have no model access when you do.** With an
  operator-configured cloud model (Bedrock), the Access step still defaulted to the api-key
  option and warned in red that the run's first model call would 404 — while dispatch was
  going to supply model access automatically, and the secret list it pointed at held no model
  key. The preflight now asks the same resolver dispatch uses, and Review defers to that
  verdict instead of guessing. When the preflight is unavailable the old local check still
  applies, so a real gap is never hidden.

### Added

- **`wardyn setup proxy-relay <listen-port> <proxy-port>`.** Forwards a reachable port to a
  forward proxy bound to `127.0.0.1`, which no container can reach — a sandbox's loopback is
  its own, and on a VM-backed Docker host the runtime VM can't reach the host's loopback
  either. Foreground and unsupervised on purpose (Wardyn owns no host daemons); it fails
  immediately if nothing is listening on the target port. See
  [docs/adoption/loopback-only-forward-proxy.md](docs/adoption/loopback-only-forward-proxy.md).

- **The Host proxy step warns when a detected proxy is loopback-bound**, naming the symptom
  and the fix, instead of leaving it to be discovered at the first launch. Shows up in
  `wardyn setup status` too.

- **`wardyn site-config get|apply`.** The corporate baseline (upstream-proxy ref, artifact
  mirrors, SCM hosts) lives in Postgres, so `make reset` and `make reset-all` take it with the
  volume — a reset previously came back up looking healthy with egress silently unconfigured.
  The baseline is now a file you can keep; it carries secret *names*, never values. `reset-all`
  also names what it is about to destroy and prints the capture command first.

### Changed

- **Writable workspaces name the VM-backed-host caveat.** An adopter reported that a
  `writable: true` host mount is read-only in practice under the Wall tier on macOS. Testing
  on a native Linux host with all four runtimes did **not** reproduce it — runc, gVisor, Kata
  and libkrun all wrote successfully — so the cause appears to be the macOS→VM file-sharing
  layer beneath the runtime, not the barrier itself. The Workspaces banner now says exactly
  that, scoped to VM-backed runtimes, rather than claiming a tier "cannot write" (which would
  be a new false statement of the kind these reports keep surfacing). The measured
  tier↔writability matrix is in the adoption doc.

- **The agent registers its workspace as a git `safe.directory`.** A bind-mounted checkout is
  owned by the host user, whose uid rarely matches the sandbox agent's, so git refused every
  command with "detected dubious ownership" — which reads like a Wardyn defect rather than a
  uid mismatch.

## [0.4.1] — 2026-07-20

A corporate-network onboarding fix release, from two adopter field reports (kept in
[docs/adoption/](docs/adoption/)). Both are onboarding-trust bugs on the containerized
default path: one hard-blocked `make setup`, the other made the Getting-Started
checklist assert something it never actually checked. No interface changes.

### Fixed

- **`make setup` no longer dies on a registry that can't serve `pnpm`.** Behind a
  corporate allowlist mirror that hasn't onboarded pnpm, the image's default
  `ui-build` stage failed with a raw `npm error code E404` (or 403) and the stack
  never came up — the one command the docs tell a new user to run did not work out of
  the box. `scripts/up.sh` now catches a failed build and retries with the UI built on
  your host (`make ui` + `WARDYN_UI_STAGE=ui-prebuilt`), explaining each step. The
  escape hatch was already there; it just required knowing an env var the failure
  never named. An explicit `WARDYN_UI_STAGE` is still honored and disables the
  fallback, and the OSS path with a working registry is unchanged — the retry branch
  is never entered. Chose retry over probing the registry first: the 404 is on the
  *tarball* path, so a mirror that proxies metadata answers a probe with 200 and the
  build dies anyway.

- **Staging a corporate CA no longer breaks the image build.** The `ui-build` stage
  ran `update-ca-certificates`, but its `node:*-bookworm-slim` base purges the
  `ca-certificates` package in its own build, so the binary is absent — meaning any
  operator who followed `make doctor`'s own advice and staged `deploy/images/corp-ca.pem`
  hit a hard `exit 127`, every time, before the pnpm step even ran. That stage now
  relies on `NODE_EXTRA_CA_CERTS` alone (npm/pnpm are its only TLS clients, and they
  read it). `deploy/images/README.md`'s snippet — which is what produced the bug —
  was corrected so it stops reproducing it.

- **The Host proxy step tells the truth on the containerized stack.** It reported
  "No host-side proxy configuration detected" on hosts unambiguously behind a
  corporate proxy, in the same session whose `HTTP_PROXY` Wardyn's own image build had
  just consumed. `DetectHostProxy()` runs *in* the wardynd process, and in a distroless
  container every tier is structurally blind: the env tier sees only the container's
  env (compose forwarded the proxy as a **build arg** only), `HOME` is unset so no
  shell profile or tool config is reachable, there is no `git` binary, and the OS/PAC
  tier dispatches on the *process's* `GOOS`. `make setup` now runs the same detector
  **on the host** (new `wardyn setup detect-proxy`, from a host-native binary the image
  cross-compiles) and seeds the result in, recovering the macOS `scutil`/PAC,
  Windows-registry, shell-profile, git and tool-config tiers that are inherently
  host-side. It re-runs on every `up`, so it cannot go stale across a network change.
  Deliberately **not** done by forwarding `HTTP_PROXY` into wardynd's runtime
  environment: Go's `net/http` honors those names process-wide, which would silently
  reroute wardynd's own OIDC discovery, audit webhooks, GitHub App minting and AWS
  credential chain through the corporate proxy — a live-traffic change to fix a
  diagnostic. A run's egress is unaffected either way (the sandbox env is built from
  scratch and always points at wardyn-proxy).

- **An honest empty result when detection genuinely can't look.** When wardynd is
  containerized and no host-side reading was seeded, the step no longer asserts a
  false negative: it says detection ran inside the container, names what it therefore
  could not read, and carries a next step. The step's static lede ("Wardyn detected
  these host proxy settings…"), which rendered unconditionally *above* an empty
  result, is gone. Same honesty rule the Vault/KVM copy already followed.

- **A set-but-empty `HTTP_PROXY` no longer counts as a detected proxy.** `os.LookupEnv`
  reports ok for `export HTTP_PROXY=`, and every consumer downstream tested presence
  only — so an empty value rendered a blank-valued "detected" row. Empty and
  whitespace-only values are now filtered, in the one place all tiers route through.

## [0.4.0] — 2026-07-19

Wardyn remains **pre-alpha**: interfaces are not stable and this release changes
several defaults. Read "Upgrading from 0.3.1" below before pulling it onto a
host that was running 0.3.1.

### Added

- **The console's default sizing now matches 110% browser zoom.** The root font
  token moved from 16px to 17.6px and the hardcoded `px` text utilities were
  converted to `rem`, so text and spacing scale together rather than the small
  labels being left behind. One token (`--font-size` in `ui/src/styles/theme.css`)
  rescales the whole console.

- **Container as an execution environment.** A workspace can now be a container
  image (a new `container` kind, `Source` = image ref, no mount) alongside a local
  directory or repository, and any workspace/container can carry an operator-owned
  model/harness credential binding (`none|managed|api_key|bedrock` — secret
  names/refs only, never secret values). A run inherits the model access of the
  workspace/container it picks; the binding is folded into the run policy at create
  and injected proxy-side, so the sandbox never holds the credential. Onboard and
  edit bindings from the Workspaces screen or `PUT /workspaces/{id}/llm-cred`
  (migration `0024_workspace_llm_cred`).
- **YAML policies.** `wardyn run --policy-file` and `wardyn policy create|update -f`
  accept JSON or YAML; `wardyn policy render -f <file>` (or `-f -`) converts either
  to canonical JSON and strictly validates it without touching the control plane.
  Commented examples ship in `examples/policies/`: `sandbox.yaml`,
  `sandbox-claude.yaml`, and `sandbox-workspace.yaml` (a governed agent on a real
  local directory — note the mount source must be an **onboarded** `local_dir`
  workspace, and `read_only` defaults to `true`, so edits need it set `false`).
- **`wardyn subscription connect|status|disconnect`.** Capture a `claude setup-token`
  from **stdin only** (never argv, never `.env`), stored age-encrypted and injected
  proxy-side into eligible runs — the sandbox holds only an inert sentinel. Connect
  is idempotent (skip-if-live, `--reconnect` to replace). The reserved secret name
  `wardyn-harness-anthropic-oauth` is blocked from the generic secrets API so this
  is the only supported path. `WARDYN_SUBSCRIPTION_TOKEN` seeds it headlessly through
  `scripts/up.sh` / `scripts/ci-run.sh`. SDK: `ConnectManagedSubscription` /
  `DisconnectManagedSubscription`. The setup-token is long-lived (~1 year),
  age-encrypted and non-rotating — documented, not hidden.
- **`wardyn setup status`**: the console's readiness checklist in the terminal
  (same `/setup/status` source), each unmet check naming the exact next command.
- **AI Run Composer on the container path.** A composer backend that runs the real
  `claude` inside a governed one-shot sandbox with the managed subscription injected
  proxy-side (never resident), mirroring the governed scan-run pattern — brokered
  `/internal/compose-results/{runID}`, `WARDYN_COMPOSE_ONLY` agent-run mode,
  cross-run guard, fail-closed when no subscription is connected, reclaim-on-timeout.
- **Containerized AWS SSO login for Bedrock.** A fourth Bedrock credential path for
  SSO-only orgs: the operator authorizes a device code in any browser and Bedrock
  runs get short-lived role credentials — no host `aws sso login`, no `~/.aws` mount.
  `deploy/images/aws-sso` is a dedicated login image (AWS CLI v2, GPG-verified) so
  ~600 MB of CLI stays out of normal runs; `cmd/wardyn-aws-sso` uploads the SSO cache
  through the brokered internal endpoint; `runs_bedrock` materializes a minimal
  synthetic `~/.aws` (sso-session + hashed cache file). The pane collects your org
  portal URL and the server seeds a credential-free `[sso-session wardyn]` via
  `WARDYN_AWS_SSO_CONFIG_B64`; Wardyn persists no copy. **Honest bound:** the SSO
  token and the derived role credentials are **resident** in the sandbox (the UI says
  so with an amber chip), and Wardyn cannot revoke a captured SSO session. Validated
  against a fake sso-oidc/portal built from the real botocore service models, not
  against a live IAM Identity Center.
- **Standard AWS environment is honored.** `WARDYN_BEDROCK_REGION` /
  `WARDYN_BEDROCK_AWS_PROFILE` fall back to `AWS_REGION` / `AWS_DEFAULT_REGION` /
  `AWS_PROFILE`; the Wardyn-specific names still win. A region alone cannot enable
  Bedrock (that also needs a model), so this cannot switch the transport on by surprise.
- **Corporate-network image builds.** Corp-CA staging (`corp-ca.pem`, documented in
  `deploy/images/README.md`) and `NPM_REGISTRY` / `HTTP_PROXY` / `HTTPS_PROXY` /
  `NO_PROXY` are threaded into every `docker build` and through compose `build.args`;
  `Dockerfile.wardynd` gains a `UI_STAGE` selector (`ui-build` default,
  `ui-prebuilt` via `WARDYN_UI_STAGE`) so a mirror that cannot serve pnpm consumes a
  host-built `ui/dist`. `up.sh` builds agent images one at a time and continues past a
  blocked image with a summary instead of aborting the bring-up. **corepack does not
  work around a mirror missing pnpm** (it fetches from the same 404ing registry path)
  — hence the prebuilt stage.
- **Opt-in native agent-CLI install for corp networks** (npm stays the default):
  `CLAUDE_INSTALL=native` installs the native `claude` binary, checksum-verified
  against the release manifest from `downloads.claude.ai` (the only host it contacts —
  allowlist it) or from a host-staged binary for fully offline builds;
  `CLAUDE_CODE_VERSION` pins it. `CODEX_INSTALL=native` is staged-only and fails
  loudly if no binary is staged, because codex has no Wardyn-verified public download
  contract. `scripts/stage-agent-binary.sh` does the host-side staging.
- **Shared-host concurrency for the compose control plane.** Every explicitly named
  compose object is parameterized off `WARDYN_NS` (default `wardyn`, so the
  single-user default is byte-for-byte unchanged): container names, the internal
  network (`WARDYN_INTERNAL_NETWORK`, now threaded into `wardynd` config), and the
  recordings volume. `WARDYN_PG_PORT` parameterizes the Postgres host port.
  `ci-run.sh` takes a per-job `COMPOSE_PROJECT_NAME` + `WARDYN_NS` (unique per
  invocation, pinnable via `WARDYN_CI_PROJECT`), ephemeral host ports, a
  container-health wait, and a project-scoped `down --volumes` that can no longer wipe
  another job's stack. `make test-e2e-concurrent` (`scripts/test-concurrent.sh`) is the
  live two-job acceptance test and passes. Boundary: this is safe for **one trusted
  operator** (e.g. a CI fleet under one service account), not for mutually distrusting
  tenants.
- **Fail-closed resource caps.** On a host where the daemon silently ignores
  `NanoCPUs`/`Memory`/`PidsLimit` (cgroup v1, or rootless without controller
  delegation), the sandbox ran effectively uncapped and nothing detected it. The
  daemon's `ContainerCreate` response warnings are now authoritative: any
  "…Limitation discarded" warning refuses the run and rolls the sandbox back.
  `WARDYN_ALLOW_UNENFORCEABLE_CAPS=1` overrides on a trusted host (downgrades to a
  loud warning); `wardyn doctor` surfaces the signal pre-boot as an advisory hint.
- **Run type in the New Run wizard.** The Basics step offers "Agent run" vs
  "Governed command" (a plain shell command, `task_mode=exec`, no agent, no LLM
  credentials), and bring-your-own base image is promoted out of Advanced into a
  first-class field. Picking a container workspace attaches it as the run's base
  image (it rides `run.image`, so the backend resolves it back by image ref to inherit
  its bound credentials instead of 422ing at the onboarded-mount gate).
- **Harness-aware demo.** A fifth demo ("The agent in the box") appears on `/demos`
  once a model/harness provider is connected, scoped to Anthropic egress. Like every
  demo it comes up idle and you run `claude` yourself in the attached terminal. The
  four keyless egress-boundary demos stay LLM-free and remain the frozen set inside
  Getting Started.
- **Enterprise onboarding and a corp-aware `doctor`.** The wizard now offers the
  Bedrock credentials the backend already read but never exposed — the preferred
  never-resident bearer (`bedrock-api-key`) and the SSO session token
  (`aws-session-token`), ordered by `resolveBedrockAuth` precedence. `up.sh` wires
  operator-provided Bedrock config (region/model, `~/.aws` bind with the uid-1000 ACL
  note) into `deploy/compose/.env` on the container path. Rancher Desktop is detected
  by docker context and its non-bind-mountable `~/.rd/docker.sock` is remapped to the
  in-VM socket, and `wardyn setup wall` gains a Rancher branch (`rdctl shell`) instead
  of telling a Rancher user to install Colima. `doctor` warns when a forward proxy is
  set with no corp CA staged (before a three-minute build dies on x509) and asserts the
  chosen docker socket is actually bind-mountable, not merely reachable.
- **Rootless and Podman: documented and probed.** THREAT-MODEL now states the
  supported rootless model — rootless Docker/Podman is supported at **CC1 only**; CC2
  and CC3 are refused fail-closed (gVisor needs `--TESTONLY-unsafe-nonroot`, Kata needs
  device passthrough) — and reframes the upstream corp-proxy hop as a supported,
  operator-configured egress lane with its bounds stated (it does not make private IPs
  reachable; only the resolved-IP re-check is deferred to that proxy). It also answers
  the JVM/Deno trust-store question: MITM is not mandatory for any egress class, so
  only a client pointed at an explicitly MITM'd host is affected. `scripts/test-podman.sh`
  probes the exact primitives the runner depends on and **passes against a live
  rootless Podman 4.9.3** (internal bridge blocks off-host egress, runtimes map
  readable, host-gateway resolves, caps verified by reading the container's cgroup v2
  `cpu.max`/`memory.max`/`pids.max`).
- **`docs/ADO-GIT-BROKER.md`**: a reviewed **design only** for never-resident
  proxy-side Azure DevOps git egress. The working lane today is still the resident
  `git_pat`. The named ceiling: Azure DevOps has no token-minting API, so the operator
  PAT's scope is the boundary — never-resident yes, per-repo auto-expiring scoping no.
  Not implemented, because the switch touches the credential path and needs a real ADO
  PAT and repo to validate.

### Changed

- **`make setup` is containerized by default.** Host mode is now an advanced escape
  hatch (`WARDYN_SETUP_MODE=local`); `WARDYN_SETUP_MODE=team` prints a notice and exits.
- **First-run setup is a mandatory gate.** The welcome screen is a single
  "Get started" call to action into Getting Started — the "try a 2-minute demo sandbox"
  side door and the "skip for now" / "finish later" escapes are gone. While the gate is
  active every route except `/setup` and `/demos` redirects to `/setup`, the
  Operate/Configure/Forensics nav groups are hidden, and the top-bar New-run action is
  hidden. The gate keys on the console being **new**, not on backend readiness (see
  Fixed), and team/SSO deployments are never gated.
- **The model/harness provider is optional.** Only the sandbox barrier is a hard
  requirement: the `llm_provider` setup check is INFO rather than a warning, the step
  carries a neutral "Optional" chip with an explicit "Skip — run without a model"
  control, and the Review/Launch rollup keys off the barrier alone. The AI Composer and
  agent runs need a provider; a governed command (`task_mode=exec`), a
  bring-your-own container, and an interactive run you drive yourself do not.
- **The model step asks about the harness first.** Choosing "container login" no
  longer silently commits you to a Claude subscription: you pick the agent harness,
  then that harness's credential path, each named for the credential rather than the
  mechanism ("Set up Claude subscription" / "Set up Anthropic API key" / "Set up AWS
  Bedrock", the last expanding its four modes inline). Each path keeps its posture chip
  — green proxy-injected, amber resident.
- **The setup funnel is ordered by prerequisite, not by theme.** Host Proxy and
  Artifact Redirect move out of a collapsible "Corporate network" section that sat
  *after* the demos and into Essentials *ahead* of the model step (connecting a model
  needs egress, and so do the demos, so meeting an unconfigured proxy later made Wardyn
  look broken rather than unconfigured). Credentials now precedes Workspaces, because
  onboarding a private repo needs the git credential to clone. The fast-path "You're
  ready — launch your first run now" banner is removed; it duplicated the Launch step
  and talked over whatever step was being configured.
- **Platform-first framing.** Wardyn is presented as a governed-sandbox platform for
  any workload — scripts, builds, tools, and coding agents — with AI agents as the
  flagship use rather than the definition (hero, IntroBlurb, setup tagline, runs copy,
  glossary, README).
- **The AI Run Composer is marked Beta** (a chip on the "Describe your task" entry
  card and on the "AI Run Composer" review eyebrow). "Configure manually" is unmarked.
- **The composer's "Proposed setup" review leads with what blocks you.** The model's
  rationale and advisory `model_notes` collapse behind "Why this setup" disclosures;
  real blockers, the deterministic clamp notices, and the risk gate stay primary.
- **Setup persists the chosen Docker socket.** `wardyn_pick_docker_host` wrote
  `WARDYN_DOCKER_SOCK` into the environment only, so any `docker compose up -d wardynd`
  run outside `up.sh` fell back to compose's `/var/run/docker.sock` default. On a
  dual-daemon box that daemon has no `runsc`/`kata`, so the barrier silently collapsed
  from three tiers to Fence-only. The value is now written into `deploy/compose/.env`,
  so every later compose invocation from any shell drives the same daemon.
- **Operator-supplied egress domains are validated server-side.** A mid-label wildcard
  like `oidc.*.amazonaws.com` compiles to an exact hostname no request can equal — the
  matcher supports a **leading** `*.` suffix or an exact host, nothing else. Until now
  the API, inline policies and `WARDYN_DEFAULT_POLICY` all accepted such an entry (only
  the UI checked, client-side), so a policy could carry a silently dead rule. One
  predicate, `ValidDomainEntry`, now runs at the `validatePolicySpec` chokepoint every
  operator ingest point routes through. Entries with a port or path attached
  (`*.example.com:0`, `*.example.com/path`) are rejected too. **This can reject a
  policy that 0.3.1 accepted — but only entries that never matched anything.**
- **A half-specified per-workspace Bedrock override is rejected at write time.** A
  Bedrock model id is a region-scoped inference profile, so a region without a model
  (or the reverse) fails at invoke. Omitting the block entirely remains the
  inherit-everything case. `validateWorkspaceLLMCred` also rejects an `api_key_secret`
  naming a sink-reserved secret.
- **The resident-secret disclosure is corrected.** The threat model claimed secrets
  reach the sandbox in "two named, bounded exceptions" and said so in three places that
  did not agree on which two. Reading the code, there are **eight** — including the
  AWS SSO token this release adds, which the threat model did not mention at all.
  Section 5.1a is now the single authoritative enumeration: what lands, why it cannot
  be proxy-injected, what bounds it, and where no bound exists (Wardyn cannot revoke a
  captured SSO session, and verbatim-only masking does not match the base64 copy carried
  in the env var). The other sites point at it instead of restating a count.
- **The compose banner's "production path is Kubernetes" claim is corrected**: the
  Kubernetes data plane is v0.5-planned and cannot create sandboxes yet.
- **CLI list output prints ids in full.** `run list` printed 8-character ids and
  `run kill <that id>` then rejected them ("invalid UUID length: 8"); the same mismatch
  broke `approvals list` → `approve`/`deny` and `policy list` → `run --policy`.
  The ID column of runs/approvals/policies is no longer truncated; context-only columns
  (an approval's RUN column, an audit target) still are.
- Personal paths and usernames are scrubbed from the tree (the example policy no longer
  pins the author's home directory, `docs/FRESH-START.md` no longer names a
  pre-rename checkout, local-principal fixtures use `alice`). No behavior change.
- Repo gates are back in truth with the tree: the diagram manifest re-points at
  `runs_lifecycle.go`/`runs_dispatch.go` after the `runs.go` split, `workspaces.tsx`
  is split on two single-concern seams (1226 → 609 + 437 + 217 lines) instead of being
  allowlisted, and the image-pin gate resolves a bare `${VAR}` against the Dockerfile's
  own `ARG` default before its alias check (so `FROM ${UI_STAGE}` is no longer a false
  positive, and a `${VAR}` that resolves to a real registry image still needs a digest).

### Fixed

- **A workspace's model credential binding never reached the run's persisted grants —
  and the operator's Claude subscription was billed for it.** `persistRunGrants`
  snapshotted the run spec's grants, proxy injections and SCM egress; `applyWorkspaceCreds`
  mutated that same by-value spec fifty-two lines later and nothing re-read it. So a
  workspace bound to its **own `api_key`** contributed no grant at all and the run fell
  through to the control-plane managed subscription — the operator paid for a run the
  workspace had explicitly bound to its own key. `managed` and `bedrock` bindings could
  not displace a competing api-key grant either, so the api key won over the transport
  the operator chose. The binding was stored, rendered in the UI and audited; it simply
  never took effect. Credential resolution now runs **before** grants are persisted, and
  the tests drive `POST /api/v1/runs` and assert on the persisted grants — the only
  formulation that could have caught it. One visible consequence: when a workspace's
  approved egress already listed `api.anthropic.com`, that host is now contributed by the
  credential binding, so it no longer appears in the `run.workspace.egress` audit's
  `added_domains`. The domain sets dedupe, so the effective policy is unchanged.
- **Per-workspace Bedrock bindings are actually applied.** Until now a workspace's
  Bedrock region/model/profile was stored and displayed while dispatch explicitly ignored
  it (the UI said so). Region, model and profile are threaded through
  `resolveBedrockAuth` with the global config as fallback, the region's
  `bedrock-runtime`/`bedrock` hosts are unioned into the run's egress, the SSO region
  falls back to the **effective** region (not the global one, which would hand a
  relocated run the wrong oidc/portal endpoints), and the `run.llm.bedrock` audit names
  the effective region. Not claimed, because it needs live Bedrock: that a cross-region
  inference-profile id resolves in the overridden region, and the bearer-mode exchange
  against a real `bedrock-runtime` endpoint.
- **The AWS SSO login pre-allowed three hosts the proxy could never match.**
  `oidc.*.amazonaws.com`, `portal.sso.*.amazonaws.com` and `device.sso.*.amazonaws.com`
  are mid-label wildcards, which `classifyDomain` compiles to literal hostnames matching
  nothing — so the login failed on the very hosts it claimed to allow, stored an empty
  account/role, and every downstream Bedrock run inherited `sso_account_id = `. The
  regional hosts are now derived at launch from `BedrockAWSSSORegion` (falling back to
  `BedrockRegion`) and recorded in the `harness.login.started` audit. With **no** region
  configured nothing regional is pre-allowed and the two hosts surface as first-use
  approvals — deliberately not falling back to `*.amazonaws.com`, which would hand the
  sandbox S3, EC2 and STS to fix a login. Net egress is strictly narrower than what
  0.3.1 shipped.
- **The AWS SSO login sandbox had no `~/.aws` at all**, while the setup pane auto-typed
  `aws sso login --no-browser --use-device-code` — a command that needs an
  `sso_start_url` and `sso_region` to already exist. No start-URL configuration existed
  anywhere (not a flag, not the Config struct, not the UI), so the sandbox was launched
  structurally unable to complete its one job, and the only way through was to run
  `aws configure sso` by hand in the attach terminal, which nothing documented. The pane
  now collects the org portal URL and the server seeds an all-or-nothing session block.
  The request is refused (400) when the start URL is missing, is not https, or contains
  whitespace (a newline would smuggle extra keys into the generated INI), or when no SSO
  region is configured — naming the flag to set.
- **The `aws-sso` image was in the default agent-image map and offered by the setup UI,
  but neither setup path built it**, so first use failed at pull against a `:local` tag
  that exists in no registry. Both build loops now build it.
- **The first-run gate could lock an existing console out on a transient daemon blip.**
  The gate keyed on `!has_runs || !ready` and the setup orchestrator does not cache probe
  errors, so a momentary Docker hiccup pulled an operator with existing runs out of the
  entire console — every route redirecting to `/setup`, all nav hidden, the only escape on
  the funnel's last step. (0.3.1's claim that returning consoles are never gated was not
  true.) The gate now keys on the console being new; readiness is left to the soft
  auto-open.
- **The managed Claude subscription never actually worked for runs.**
  `detect_anthropic_mode` looked for credentials only under `~/.claude`, but a managed
  subscription materializes its inert sentinel into `CLAUDE_CONFIG_DIR` (dispatch points
  there because the resident path may mount `~/.claude` read-only). The run fell through
  to "apikey" mode, which keeps `ANTHROPIC_BASE_URL` on the x-api-key inject gateway while
  the proxy injects an OAuth Bearer — surfacing as "401 Invalid bearer token".
  `CLAUDE_CONFIG_DIR` is checked first now, so the run detects "subscription" and tunnels
  to `api.anthropic.com` for the proxy swap.
- **The composer's model-access verdict was gated on the wrong toggle.** With a connected
  setup-token and the per-run "use subscription" opt-in off, the Review checklist announced
  "no model access — this run will do nothing" about a run dispatch would happily
  credential. That toggle gates the credential **mount** path; a managed subscription needs
  no mount. Mount gating and verdict are now separate.
- `examples/policies/sandbox-claude.yaml`'s `github_token` grant had `repos: []`, which
  the git-broker rejects ("github token requires at least one repo").
- **The resource-cap gate ran after `ContainerStart`, and its pre-flight probe
  false-positived on Podman.** An untrusted agent container on an uncapped host was
  therefore started and executed for the duration of the check before rollback tore it
  down — a host that cannot enforce caps must not launch the container at all, and
  started-then-killed does not satisfy that; the gate now sits between create and start.
  Separately the probe trusted `docker info`'s `MemoryLimit`/`PidsLimit`/`CPUCfsQuota`
  booleans, which rootless Podman 4.9.3 under-reports (`CpuCfsQuota=false` even though
  the quota binds, verified by reading the container's cgroup) — so Wardyn refused runs
  whose caps do enforce. The authoritative post-create warning replaces it; the
  `docker info` booleans are now only an advisory `doctor` hint, with the Podman caveat
  spelled out.
- **The New Run wizard silently dropped bring-your-own-image and `task_mode`.** Neither
  was ever forwarded onto the wire, so BYOI was lost end to end from the UI.
- **The subscription login URL arrived truncated** ("Invalid response_type: missing"):
  `claude setup-token` hard-wraps its OAuth URL at the PTY width, cutting both the scraped
  link and the fallback link. The login PTY is now forced to 512 columns so it does not
  wrap at the source.
- **The composer review printed two sentences twice**: the model-access line (the
  setup-checklist row's Detail is `llm_access.note` verbatim, and a standalone success
  line repeated it — the standalone line now shows only when there is no checklist row to
  carry it), and, on the launch-blocked path, the top risk rationale (shown both inline
  next to the badge and as the first entry of the "High-risk configuration" list — the
  inline copy now shows only when there is no list).
- Docker-tagged AWS SSO tests no longer burn a 30-second timeout each against a `:local`
  image nothing builds; they skip when the image is absent.

### Security

- **The dex host port was published on `0.0.0.0`.** It is now loopback-only and
  parameterized (`WARDYN_DEX_PORT`, `0` for an ephemeral port) so concurrent
  `sso`-profile stacks do not collide; dex only runs under the `sso` compose profile.
- Server-side egress-domain validation (see Changed) closes a defect class where an
  operator-authored allowlist entry could be silently dead. Adversarial review caught the
  first version of the predicate failing open in exactly the way it was written to
  prevent — only the exact-host branch was guarded — so both branches are checked; removing
  either guard fails five pinned cases.
- `pkg/client`'s `HarnessLogin` method is deleted. It had zero callers, and `client.go`
  and `docs/sdk.md` both already listed harness-login as **not** covered by the SDK — the
  method contradicted its own documentation.

### Upgrading from 0.3.1

- **`make setup` now brings up the containerized stack.** If you were running host mode,
  pass `WARDYN_SETUP_MODE=local` explicitly.
- **Re-run setup on an existing compose deployment** (or hand-edit
  `deploy/compose/.env`): `WARDYN_DOCKER_SOCK` is now persisted there. If you have more
  than one Docker daemon and you skip this, a plain `docker compose up -d wardynd` drives
  compose's default socket and the barrier silently collapses to Fence-only.
- **A host that cannot enforce resource caps will now refuse to launch runs.** cgroup v1,
  or rootless without controller delegation, previously ran uncapped and silent; you will
  now see `resource caps not enforceable on this host`. Fix the host, or set
  `WARDYN_ALLOW_UNENFORCEABLE_CAPS=1` if you trust the workload — that returns 0.3.1's
  behavior, uncapped.
- **A fresh console cannot be skipped past Getting Started.** Automation that drove a new
  local-mode console straight to `/runs` must complete setup first, or seed the completion
  flag. Team/SSO deployments and consoles that already have runs are unaffected.
- **Check your policies for mid-label wildcards.** An `AllowedDomains` entry like
  `oidc.*.amazonaws.com`, or one carrying a port or path, is now rejected at write time by
  the API, inline `--policy-file` policies and `WARDYN_DEFAULT_POLICY`. Such an entry never
  matched any request, so rewriting it as a leading `*.` suffix or an exact host changes
  what your policy *does*, not just whether it saves.
- **Check any per-workspace Bedrock binding.** A half-specified override (region without
  model, or model without region) is now rejected on write, and — unlike 0.3.1 — a complete
  one is actually applied at dispatch, so a workspace that has carried a stale
  region/model since 0.3.1 will start moving its runs there. Omit the block to inherit the
  server's global Bedrock config.
- **Verify which runs your workspace credential bindings bill.** Before this release an
  `api_key` binding was ignored and those runs were billed to the control-plane managed
  subscription; after it they use the workspace's key.
- Rebuild your agent images (`make agent-images-core` or `scripts/up.sh`) — the `aws-sso`
  login image is new and is pulled by tag from no registry.

## [0.3.1] — 2026-07-18

### Added

- **Repo-scoped git egress (the git-broker).** GitHub repositories are cloned and
  pushed through the `wardyn-proxy` (`/wardyn/gh/<org>/<repo>`) instead of the
  sandbox reaching `github.com` directly. The proxy enforces a per-repo allowlist
  and mints the scoped GitHub App installation token server-side, so the token never
  enters the sandbox and a run can reach only its granted repositories.
  Consequently `github.com` is no longer placed in a run's egress allowlist, and an
  un-granted GitHub repository is denied — the repository, not the host, is the unit
  of trust. Clone and push are both brokered.
- **Getting Started demos.** The setup funnel now includes four hands-on
  egress-boundary demos (as sidebar sub-steps, before you onboard your own
  repositories), each showing its policy, the equivalent New-Run setup steps, and an
  inline audit panel so you can watch the boundary hold in real time.
- **Container login for a Claude subscription.** The Model/Harness Provider step can
  launch a login sandbox, run `claude setup-token`, open the OAuth URL, and capture
  the returned token automatically — no manual copy/paste. The credential is stored
  proxy-side and injected into runs on the wire; the sandbox never holds it.

- **List endpoints paginate.** `runs`/`policies`/`approvals`/`workspaces` and
  the audit query take explicit `limit`/`offset` params (defaults preserve
  existing behavior) and disclose truncation via an `X-Wardyn-Truncated`
  header — the audit endpoint's previously hidden 1000/500 caps are now
  explicit. New composite DB indexes back the run-scoped audit sort.
- **SDK route-family coverage.** `pkg/client` gains approvals, workspaces
  (CRUD + scan/verify), site-config, setup, audit and health methods, honest
  package-doc coverage claims, and `--limit` on the CLI list commands.
- **Mobile navigation.** The console works below the `md` breakpoint: a
  hamburger-triggered drawer with full keyboard/ARIA handling replaces the
  previously hidden-with-no-fallback sidebar.
- **`wardyn record task` documented**, and the CLI now prints recovery
  guidance ("is wardynd running? try --url/WARDYN_URL") when it cannot reach
  the daemon instead of a bare dial error.

- **CLI: run is now one noun** (bare `create` + `list`/`get`/`kill` subcommands,
  `runs` alias), a central exit-code taxonomy (`0`=ok, `2`=auth, `3`=client 4xx,
  `4`=server 5xx, `5`=network, `124`=wait-timeout), `--json` on all list/mutate
  commands, and unwrapped API error messages.
- **`wardyn run --json`** for machine-readable output — emits the raw run object
  immediately (before the `--wait` loop), so scripts no longer have to
  sed-scrape the run ID out of human-readable text.
- **`wardyn approvals list [--state]`**: discover a pending approval id from the
  CLI without the UI. API errors are now typed with the real HTTP status (auth
  failures exit `2`, distinguishable from a 404/409/network failure).
- **SDK**: `CreateRunRequest` gains `DevcontainerRepo`/`DevcontainerRef`, so the
  public Go SDK carries every user-facing create-run field.
- **Every `WARDYN_*` variable is documented** in a single reference
  ([docs/ENV.md](docs/ENV.md)), enforced by a test that fails if a variable read
  in the code is missing from the doc.

### Changed

- **Host setup detects the right Docker daemon.** On a host with more than one
  Docker daemon, `make setup` now derives `WARDYN_DOCKER_SOCK` from `DOCKER_HOST`,
  so a tier-capable daemon's confinement classes (Wall/Vault) appear without a
  manual export — previously the stack could silently come up Fence-only.
- Bad `WARDYN_*` environment values (e.g. `WARDYN_ENVBUILD=treu`) now fail loud
  (exit 2, naming the variable and value) instead of silently falling back to
  the default.
- **The runner/substrate seam self-registers.** Adding a confinement substrate
  (e.g. a future Kubernetes driver) no longer requires editing the control-plane
  boot path — it registers through the component registry like the identity,
  secret-store, and recording seams, and `/healthz` reports the substrate that
  is actually selected.
- **Control-plane API and egress-proxy logs are now structured** (`slog` with
  typed attributes). Previously these two packages still emitted unstructured
  `log.Printf` lines — including the panic-in-reconcile and "run may be stranded"
  lines — which were lost when shipping to a structured log sink.

### Fixed

- **Concurrent runs no longer share one egress allowlist.** Two runs created at
  the same time could alias the same policy slice backing array and clobber
  each other's allowed domains under load.
- **A run outliving 1 hour keeps working.** The per-run identity token now
  renews instead of expiring with no refresh path, which previously killed
  decision logs, approvals, credential mints, and subscription re-resolution
  all at once.
- Recording no longer swallows upload errors that should trip the 64 MiB
  capture cap, and the managed-harness login run (which prints a long-lived
  Anthropic OAuth token to its PTY) is never recorded.
- **The sandbox kill switch survives a control-plane restart.** The
  orchestrator's ref→substrate routing is now persisted, so after a `wardynd`
  restart with more than one confinement substrate wired, `Exec`/`Attach`/`Stop`/
  `Kill` for in-flight runs still resolve — previously they failed "no substrate
  tracked for ref", breaking teardown and credential revocation for pre-restart
  runs.
- **Light theme now meets WCAG AA contrast (C004).** The teal-500-family
  semantic tokens (primary text, and success/warning/danger text on their
  `-subtle` tints) fell as low as 1.94:1, below the 4.5:1 floor for normal text;
  darkened to the 700/800 family (5.47–6.47:1). Dark theme is unchanged.
- **Keyboard access + ARIA for recordings, table rows, and skip-navigation.**
  Recording cards and clickable runs/policies table rows are now reachable and
  activatable by keyboard (Enter/Space), icon-only row-action menus
  (secrets/workspaces/policies) gained `aria-label`s, a skip-to-content link was
  added, and copy-to-clipboard only reports "Copied" once the write actually
  resolves (no false positive on insecure-context/LAN HTTP).
- **Finalizing a workspace no longer silently drops a live verify/record run's
  result.** `handleFinalizeWorkspace` marked the workspace ready and cleared its
  active-run pointer with no liveness check, unlike its verify/record siblings;
  a still-`RUNNING` verify/record run's later result then 409'd on the cleared
  pointer, and the workspace was marked ready on zero evidence. It now refuses
  (409) while the run is live, and the store write behind it is fenced so a run
  that claims the slot between the guard's read and its write can no longer be
  clobbered either.
- **A transient docker-daemon probe error no longer fails a healthy run.** Boot
  reconcile and the live completion watcher treated any liveness-probe error the
  same as a genuine terminal state, finalizing a still-`RUNNING` run `FAILED`
  and revoking its credentials on a momentary blip. Only a definitive terminal
  state finalizes now; a persistently-unreachable sandbox still gives up, but
  only after about a minute of consecutive probe errors.
- **The eBPF/Tetragon ground-truth audit stream no longer goes permanently
  blind about an hour into a run.** The shipped compose wiring held a single
  static host-sensor token with nothing to refresh it; `wardynd` now runs a
  rotator that mints and writes a fresh token before the old one expires.

### Security

- **NAT64-smuggled metadata targets are now blocked** in the composer's egress
  transport (a `64:ff9b::a9fe:a9fe`-style literal previously reached
  `169.254.169.254` past the SSRF guard).
- Base images are now digest-pinned across Dockerfiles, `go-github` is
  collapsed to a single v88 major, and `GO-2026-5932` (unmaintained
  `x/crypto/openpgp`, reached only via `filippo.io/age`, no called path) is
  documented in `SECURITY.md`.
- **Credential revocation on the kill/failure paths fixed (C002 + C003).** A
  kill that lost its terminal-state race to a concurrent dispatch used to
  already have stripped a run's credentials before knowing whether it actually
  won — leaving a run that turns out to stay live with dead credentials behind
  a silent 409; revocation now runs only after the terminal transition is won.
  Separately, none of the 9 internal create/dispatch `FAILED` transitions ran
  the revoke cascade, so a run that failed during creation or dispatch left its
  minted identity token and broker credentials un-revoked; every failure
  transition now revokes.
- **A sandbox could no longer escape the workspace via a planted symlink.**
  `handleFinalizeWorkspace`'s env-as-code emit followed sandbox-planted
  symlinks out of the workspace directory, letting a compromised sandbox
  overwrite host files outside it; the emit no longer leaves the workspace.

## [0.3.0] — 2026-07-14

### Added

- **CI mode (BYOA)** ([docs/CI.md](docs/CI.md)): run a governed sandboxed job from a
  pipeline with no pre-running Wardyn, no UI, no human.
  - `wardyn run --wait [--timeout]` blocks to the run's terminal state and exits with
    the outcome (`COMPLETED`=0, `FAILED`=the agent/task's real exit code from the
    `run.complete` audit event, `KILLED`/`STOPPED`=2, timeout=124); `wardyn runs get
    <id> [--json]` wraps `GET /api/v1/runs/{id}`.
  - `wardyn run --image <ref>` exposes the existing BYOI wrap from the CLI: any
    user-supplied container is wrapped with the runner tools and governed.
  - `task_mode: "exec"` (`wardyn run --task-mode exec`): the task runs as a plain
    shell command instead of the agent harness — same clone/creds/egress/recording
    wiring, no agent, no LLM credentials. Recorded in the `run.create` audit event.
  - `scripts/ci-run.sh`: one-shot fresh control plane → preflight → governed run →
    `--wait` → artifacts (`run.json`, `audit.json`) → teardown, exit code propagated.
    Plus `examples/policies/ci.json` (unattended fail-closed baseline),
    `deploy/compose/docker-compose.ci.yaml` (envbuild overlay), and copy-paste
    pipeline examples `docs/ci/github-actions.yml` / `docs/ci/azure-pipelines.yml`.
- **Demo sandboxes** (`/demos`): four hands-on scenarios (sealed box, fail-then-approve,
  held-at-the-door, unconditional metadata/private-IP denial) that launch interactive,
  workspace-free, model-free sandboxes with an embedded terminal + live approvals —
  prove the egress boundary before onboarding any workspace. Entry from the Welcome
  hero and the setup funnel; gated only on the sandbox barrier. TRY-IT gains Level 0.5.
- **Run preflight** (`POST /api/v1/runs/preflight`): a dry-run of launch resolution +
  gating; the manual wizard's Review step now shows the composer's setup-readiness
  checklist (secrets/workspaces/backend/egress) and the automatic blast-radius
  confinement raise before launch.
- **CLI**: `wardyn run --policy-file <spec.json>` sends a full inline policy; `--repo`
  is now optional (server parity — repo-less scratch runs).
- `make screenshots`: reproducible docs screenshot capture against a dedicated seeded
  backend (dark theme, 1440×900); docs/img PNGs regenerated for the 0.2 UI.

### Changed

- **AI Run Composer surfaces automatically** when a composer backend is configured
  (`WARDYN_COMPOSER_CONFIG`); the compile-time `COMPOSER_UI_ENABLED` flag is gone.
  Without a backend the New Run dialog falls back to the manual wizard, as before.
- The New Run wizard no longer requires a workspace: an empty selection launches an
  ephemeral scratch-directory run (the API always allowed this).
- Getting Started's "Launch your first run" now gates on the sandbox barrier only;
  a missing model shows a non-blocking notice (interactive runs work without one).
- The managed-subscription fallback no longer silently widens a zero-egress policy
  with `api.anthropic.com`; a policy authored sealed stays sealed.
- README rewritten for concision (530 → ~300 lines): quickstart defers to
  `docs/TRY-IT.md`, raw curl examples moved to `docs/sdk.md`, `WARDYN_LOCAL_MODE`
  detail moved to `deploy/compose/README.md`, egress table to `ARCHITECTURE.md`.

### Fixed

- Preflight/inline-policy previews no longer write `policy.inline` audit events
  (dry-runs are not authorizations; the audit feed stays a clean system of record).
- `scripts/setup.sh` now ends with prominent warnings when the agent-image build or
  age-key persistence quietly failed mid-setup (both previously warn-and-continue).

## [0.2.0] — 2026-07-13

The v0.2 milestone was the **Docker-only honest pilot**: every control the docs
claim is actually enforced in code (or honestly marked unbuilt), the operator
surface is complete enough to run a real pilot, and the deployment is verifiable.
Kubernetes, SPIRE, OpenBao, the L3 MCP gateway, and arbitrary-domain L2
TLS-intercept remain v0.5 (targeted TLS-MITM of LLM/registry hosts already
ships — see the threat model's §5.1a claims contract).

### Added
- **Writable workspace mounts.** A local-directory workspace can opt in to
  read-write mounting for its runs (migration 0016). Previously the run wiring
  never set the mount's writable flag, so every imported workspace mounted
  read-only — silently defeating Record/Verify sessions that need to install
  dependencies or build. Opt-in checkbox with a host-persistence warning; the
  default stays read-only.
- **Safest-path recommendations across the setup surfaces.** A presence-only
  git-credential posture probe (gh CLI login, credential.helper,
  `~/.git-credentials`, `~/.netrc` — values never read) grades the SCM step
  against a safest-path ladder (GitHub App → brokered fine-grained PAT →
  deploy key → standing resident keys); the Provider step shows
  proxy-injected-vs-resident residency chips on every auth option; the
  per-run confinement picker badges the strongest available tier Recommended;
  the Secrets screen marks standing `ssh-key-*`/`git-pat-*` credentials with
  an amber Standing chip.
- **Scoped deploy-key generation in setup.** The installer prints the 5-rung
  git-credential ladder before any import decision and can generate an
  ed25519 read-only deploy key: the private half goes stdin-only into the
  encrypted store and is shredded from disk; the public half is printed with
  paste instructions and the honest per-host-slot/per-repo ceiling
  (multi-repo work → fine-grained PAT).
- **Bring Your Own Image (BYOI).** A run may name an arbitrary base image;
  the control plane wraps it with the runner tools (`internal/envbuild`
  FinalizeBase; opt-in via `WARDYN_ENVBUILD`) and gates launch on an in-sandbox
  `agent-run --selftest`, fail-closed, with faithful recorded exit codes. The
  wrap is wrap-only (`FROM` + `COPY`, no image-controlled code on the host): a
  base declaring `ONBUILD` triggers is refused, since a `FROM` fires them on the
  host daemon outside every confinement tier. The base ref may be a tag or a
  digest (`repo@sha256:…`) — a pinned, pre-pulled base is honored without a
  registry round-trip — but pinning is NOT enforced; a mutable tag is resolved at
  wrap time and pinning remains an operator practice. The New Run wizard gains a custom-image field; live
  proof harness: `scripts/run-e2e-byoi.sh` (`make test-e2e-byoi`). Operator
  docs in `deploy/images/README.md`.
- **Wardyn-managed Claude subscription via container login.** A containerized
  control plane (no host `~/.claude` to stage) can connect a Claude
  subscription from the Getting Started page: an interactive login sandbox
  plus a pasted `claude setup-token` credential, stored age-encrypted under a
  reserved secret name and injected proxy-side like the resident-login path.
  The managed credential is strictly a **fallback** — an explicit
  anthropic-api-key injection always wins, so composing with "Use my Claude
  subscription" unchecked keeps the api-key path.
- **Single-use WebSocket attach tickets.** Browser terminal attach now works
  in token mode: `POST /api/v1/runs/{id}/attach-ticket` mints a single-use,
  short-TTL ticket for the WS handshake (browsers cannot put a bearer token
  on a WebSocket upgrade).
- **Run image provenance + portable MITM CA trust.** Every run persists the
  RESOLVED sandbox image it actually ran (shown on run detail), and the
  per-run TLS-MITM CA moved to an any-uid-writable path with a combined
  system-roots+CA bundle wired via `SSL_CERT_FILE`/`REQUESTS_CA_BUNDLE`/
  `CURL_CA_BUNDLE`, so curl/Python/Ruby in wrapped and BYOI images trust the
  proxy's TLS termination (JVM keystores and `DENO_CERT` remain documented
  gaps).
- **`make setup` — a consent-first, host-mode installer.** The interactive
  `scripts/setup.sh` detects the host (Docker daemons, WSL, barrier capability,
  an existing `claude` login) and sets up **host mode**: it stages the operator's
  Claude login for sandboxes (live token proxy-injected — the resident copy is an
  inert sentinel, no usable credential), builds `bin/wardynd` + the UI,
  reuses/starts the compose Postgres, and runs `wardynd` as a background host
  process (PID/log under `~/.wardyn/`, stop with the new `make stop-host`). Every
  auto-detected credential (Claude login, AWS/Bedrock keys, SCM PAT/SSH key) is
  gated behind a plan-then-prompt; a headless run writes nothing and destroys no
  volume without an explicit opt-in flag. Barrier installs (gVisor/Kata) are never
  run with silent sudo — the exact commands are printed instead.
- **Team (compose multi-user) deployment is marked coming-soon.**
  `WARDYN_SETUP_MODE=team` prints a notice and exits, the `make setup-host` /
  `make setup-team` shortcuts are removed, and the UI's "Sign in with SSO"
  button is disabled. The compose stack itself still runs for its other uses
  (`make compose-up`, `make demo`, `make reset`, and as the containerized
  single-user mode below); the OIDC/SSO backend remains present and CI-tested.
- **One front door: `make setup` asks host vs containerized.** With a TTY and
  no explicit `WARDYN_SETUP_MODE`, setup asks where the control plane should
  run — **host** (default; wardynd runs as you, your Claude login usable
  directly) or **containerized** (delegates to `scripts/up.sh up`: the compose
  stack, single-user, the Docker Desktop + WSL2 NAT workspace-Verify/Record
  fix). Headless stays promptless and defaults to host.
- **Named recording sessions + confined Verify sessions.** Record is now a
  user-named interactive session with model access (the fixed build/test task
  taxonomy is gone). Verify launches a fresh confined session for an existing
  recording — default-deny egress limited to the approved set, with live
  approvals — to re-run the same steps under least privilege and prove a
  profile works before it's relied on (a live re-run, not a byte-for-byte
  replay of the captured session).
- **Recorded profiles as run sources.** The New Run dialog's Basics step
  offers a workspace's recorded sessions as profiles; picking one fast-tracks
  to Review, and the recording's observed egress is loaded into the run's
  allowlist for review. A profile can be saved as-is from the recording
  (workspace + recording name).
- **Live first-use egress approvals.** `first_use_approval` is now a
  three-mode enum — `always_deny` / `deny_with_review` / `wait_for_review` —
  where `wait_for_review` holds the sandbox connection at the proxy until an
  operator decides (bounded wait, fail-closed), with live approval prompts
  surfaced next to the attached terminal.
- **Corporate-baseline setup steps in the Getting Started wizard.** New
  Host Proxy, SCM Provider, and Artifact Redirect steps (plus a dedicated
  Review step and a detection-driven, provider-family "Model/Harness
  Provider" step): operator site-config persists an upstream (parent) proxy,
  per-ecosystem artifact-mirror redirection with proxy-side token injection,
  and SCM PAT credentials (`git-pat-<host>` secrets, Azure DevOps egress),
  all composed into dispatch.
- **AWS Bedrock as a model provider for Claude runs.** Operator-configured
  (`WARDYN_BEDROCK_REGION` + `WARDYN_BEDROCK_MODEL`): a `bedrock-api-key`
  bearer token is proxy-injected (never resident), or SigV4 access keys sign
  in-process (resident — masked, withheld from scan/verify, documented in the
  threat model).
- **Opt-in AI assists.** An advisory AI profile fallback for workspace scans
  (`WARDYN_SCAN_AI_ADVISOR`, off by default) and a human-gated AI verify-fix
  diagnosis panel in the import flow.
- **Workspace scanning now detects what a workspace NEEDS.** Onboarding scans
  extract — from committed files only, NAMES ONLY (values are never read; real
  `.env`-style files are recorded presence-only and never opened, and none of the
  new detector targets can reach the AI-advisory sample path) — the secrets a
  workspace expects (`.env.example`-family keys, Spring/Quarkus `${VAR}`
  placeholders with has-a-default classification, compose `${VAR?}` interpolations,
  SealedSecret `encryptedData` key names), the backing services it implies (compose
  images + env-name families), and content-derived egress *suggestions* (Dockerfile
  base-image registries). Suggestions are advisory by construction: only the
  filename-keyed marker table and the new operator-owned per-workspace
  **approved egress** list (`PUT /workspaces/{id}/approved-egress` — strict host
  validation, full-replacement, audited as `workspace.egress.approve`, stored
  outside the scan-owned profile via a scoped single-column write) ever widen a
  run's allowlist. The composer checklist gains non-blocking `workspace_secret`
  rows (required-only, capped at 5 + a summary row, one-click add-secret prefill
  via the same name grounding the composer uses); the workspace "View profile"
  dialog is now a structured needs panel (secret names with kind badges, services,
  three-tier egress with approve/remove, `.env`-exposure warning, honest
  blind-spot copy) instead of a raw JSON dump. Scan audit events carry counts
  only. All new scan facts are re-validated control-plane-side (charset + hard
  caps), since repo-scan facts are sandbox-controlled.
  - Detector coverage was then broadened: `.env.example` RHS-emptiness
    classification (`KEY=` empty ⇒ required, `KEY=<value>` ⇒ optional-has-default);
    k8s `secretKeyRef` data keys (block + inline YAML); CI `secrets.*` refs
    (classified CI-only/optional so they never spam the checklist; `GITHUB_TOKEN`
    dropped); source-code env reads (`getenv`/`process.env`/`os.environ`, advisory);
    maven `<repository>`/gradle `maven { url }` hosts and Dockerfile `FROM`
    registries → suggested egress; build-heap ceilings (`-Xmx` /
    `--max-old-space-size`) as an advisory "needs ~N GB" hint; and a
    high-precision **leaked-value** warning pass (AWS/GitHub/Stripe/PEM/JWT/… — the
    one lane that reads values, but records only `path:line + kind`, never the
    bytes). Deep monorepo layouts (walk depth 4→6) now reach `overlays/prod`
    SealedSecrets. The three egress preset lists (default policy, wizard, risk
    baseline) were reconciled so a scanner-unioned registry never reads as
    custom-risk egress.
  - **Observed-egress telemetry** (`GET /workspaces/{id}/observed-egress`): the
    least-privilege loop — surfaces the egress hosts that runs using a workspace
    were actually *denied* as operator promotion candidates for its approved-egress
    list. Read-only and advisory; it widens nothing itself.
- **Editing a workspace's source/kind (or a repo's ref) now resets its scan
  state** (profile, image cache, status → `pending_scan`, approved egress) — the
  old profile was reviewed against different content. This also fixes
  `PUT /workspaces/{id}` zeroing the scan-owned columns (a `status` CHECK
  violation against real Postgres).
- **Local host mode** (`-local-mode` / `WARDYN_LOCAL_MODE`): the single-developer
  localhost path. Bypasses the public-API login entirely (no SSO/Dex, no token) and
  attributes every action to the local operator (`WARDYN_LOCAL_OPERATOR`, default
  `local:<os-user>`), keeping the `sub`/`sponsor`/`decided_by`/audit attribution
  chain meaningful. Auto-enabled when no auth is configured AND the bind is loopback.
  Refuses to serve a no-auth API on an EXPLICIT publicly-routable IP; only WARNS
  (does not refuse) on an unspecified bind (`0.0.0.0`, the `WARDYN_LISTEN` default) —
  bind/publish loopback-only for a real guarantee (the Compose default already
  publishes `127.0.0.1`). Sidecar run-token auth is unaffected. The CLI now works
  without a token against a local daemon. (`internal/api/http.go`, `cmd/wardynd`.)
- **Sandbox resource limits** end-to-end: a `Resources` block on `RunPolicySpec`
  (cpu/memory/PIDs/disk) wired into the docker driver. EVERY sandbox is now
  CPU/memory/PID capped with conservative platform defaults even when a policy sets
  nothing — `MemorySwap` is pinned to the memory cap (no silent 2× via swap), a
  `PidsLimit` fork-bomb guard is always set, and a best-effort disk quota applies on
  quota-capable storage drivers — so a fleet of independent agents can't OOM,
  fork-bomb, or disk-fill the host or each other.
- **Recording Mode** (`internal/recordmode`, `POST /api/v1/runs/{id}/profile`,
  `wardyn record`): learn a reusable least-privilege sandbox profile (a saved
  RunPolicy) from what a run ACTUALLY did. Capture is a single read over the existing
  append-only audit stream (egress decisions + eBPF ground-truth + credential mints);
  synthesis is deterministic and flows through the SAME composer clamp+validate+grade
  pipeline, so a recording can never mint a profile beyond operator policy.
- **Crash/restart recovery**: a boot-time reconciler re-derives the state of any run
  left non-terminal by a previous process and re-attaches a status-polling watcher or
  finalizes + revokes — runs are no longer stranded RUNNING with a live sandbox and
  un-revoked credentials after a `wardynd` restart. Background goroutines (reaper,
  sweeper, watchers) are now panic-contained so one panic can't take the whole
  control plane (and the kill-switch) down.
- **Interactive vs non-interactive** is now a first-class, persisted run property
  (`agent_runs.interactive`), surfaced through the API/CLI/UI.
- **Workspace-collision warning**: launching a second independent agent against a host
  directory another active run already uses now emits an advisory warning (on the
  create response + an audit event) — discouraged, never blocked.
- **`store.Store` seam**: the control plane talks to an abstract persistence interface
  instead of `*pgxpool.Pool` directly, so a future pure-Go/SQLite backend can be
  swapped in.
- **Fleet view** in the web UI: a live, auto-refreshing board of all runs with
  per-agent spawn/attach/kill and a Recording-Mode profile-review surface.
- **Secret output masking** (`internal/secretmask`): a per-run + process-global
  registry of secret values and a boundary-safe streaming masker that replaces
  verbatim occurrences with `<secret-hidden>` on the PTY/asciicast recording
  stream, audit event payloads, and proxy decision logs. Honest residual
  (documented + negative-tested): verbatim byte-identical leakage only.
- **Selectable Confinement Class** end-to-end (UI → API → CLI → run). A run may
  request an equal-or-stronger class than the policy minimum; weaker is refused.
- **`GET /api/v1/runs/{id}/grants`**: real credential-grant eligibility records
  (the UI no longer synthesizes them from audit events).
- **Policy management CRUD** across store, REST API (`/api/v1/policies`), Go SDK,
  CLI (`wardyn policy …`), and a functional UI Policies screen.
- **Run completion tracking**: runs transition to `COMPLETED`/`FAILED` with the
  agent exit code instead of sitting `RUNNING` until reaped.
- **Control-plane TLS**: optional built-in TLS (`WARDYN_TLS_CERT`/`KEY`) and
  reverse-proxy termination (`WARDYN_TLS_TERMINATED`); session and login cookies
  are marked `Secure` under TLS.
- **`SECURITY.md`** responsible-disclosure policy.
- **Supply-chain CI**: `govulncheck`, static analysis, and a DCO sign-off check
  on PRs; an SBOM job stub for the release path.
- A real, CI-blocking **Docker conformance gate** that asserts L0 structural
  egress (no default route) against a live daemon.
- **eBPF/Tetragon ground-truth audit stream** (the SECOND of the three advertised
  audit streams): a target-agnostic mapper (`internal/groundtruth`) from Tetragon
  JSON-export events to `kernel.*` audit events (`kernel.process.exec`,
  `kernel.network.connect`, `kernel.file.write`, plus `kernel.sensor.heartbeat`
  and `kernel.sensor.blind`); a host-scoped sidecar
  (`cmd/wardyn-tetragon-ingest`) that tails the export, correlates events to runs
  via the `wardyn.run-id` container label, and POSTs batches to a new
  `POST /api/v1/internal/groundtruth` endpoint (separate `aud=wardyn-groundtruth`
  audit-write-only token; events recorded append-only -> Postgres AND every SIEM
  sink with zero new fanout code). `/healthz` now publishes
  `ebpf_groundtruth: {state, last_heartbeat, dropped_total}` so the stream reads
  `healthy` ONLY while sensor heartbeats arrive (`unavailable` otherwise — honest
  degradation). Detection, not prevention: the `ld-linux`/`mmap` loader bypass is
  FLAGGED (`data.loader=true`), never blocked; host eBPF is blind inside CC3/Kata
  guests (one-time `kernel.sensor.blind`). Compose ships a `groundtruth` profile
  (privileged `tetragon` sensor + ingest sidecar + a `tcp_connect`/sensitive-
  file-write `TracingPolicy`). Dependency choice: consumes Tetragon's JSON export
  (no `cilium/tetragon` dep) and shells out to `docker ps` for correlation (no
  docker client dep) — go.mod is unchanged.

### Changed
- **The AI Run Composer's Describe surface is hidden in the UI for this
  release** (`COMPOSER_UI_ENABLED=false` in `ui/src/app/lib/features.ts`; the
  backend and `WARDYN_COMPOSER_CONFIG` remain). Recorded profiles are the
  primary path to a governed run; New Run opens directly into the manual
  wizard.
- **`POST /api/v1/runs` now validates secret references for a stored or default
  policy too, not just an inline one.** Previously a run created with `policy_id`
  (or no policy at all) could name an `api_key`/`git_pat` grant whose secret
  didn't exist — or exist but with no secret store configured — and only fail
  later, opaquely, at first proxy injection or git clone. It now 422s at create
  time with the missing secret's name, the same fail-closed check the inline
  path already applied. This is a deliberate behavior change for existing API
  callers of a stored/default policy; the wizard and composer review panels
  both offer a one-click "add this secret" retry on the 422.
- Documentation (`README`, `ARCHITECTURE`, `THREAT-MODEL`) now tags every control
  inline as `[shipped]` / `[v0.2 — building]` / `[v0.5+ — planned]`, removing
  present-tense overclaims. The eBPF/Tetragon ground-truth audit stream — the
  last remaining `[v0.2 — building]` control — is now `[shipped]` (detection-only,
  honestly degradable).
- CC1 confinement now **explicitly** pins host-gated AppArmor (`docker-default`)
  and asserts RuntimeDefault seccomp (never `unconfined`); CC2/CC3 omit the
  AppArmor pin because the runtime mediates syscalls.
- The Approvals UI "Decided" tab now shows decided approvals (was always empty).
- The lifecycle now sweeps stale `PENDING` approvals to `EXPIRED`.
- **Truth-in-grading for `git_pat`/`ssh_key`**: both now grade HIGH with an
  honest rationale (Wardyn can neither expire nor down-scope them), matching
  what the approvals screen already said. codex-cli runs with an `ssh_key`
  grant now fail loud at create time (dropped grant + operator warning +
  audit event) instead of failing silently mid-run — codex-cli has no SSH
  clone lane.
- **Bedrock model-access honesty**: the static-key consent states that
  long-lived SigV4 keys become resident in sandboxes (they cannot be
  proxy-injected) and names the safer AWS-SSO rung; `compose up` warns when a
  stale `WARDYN_AGENT_IMAGES` override names an image absent from the picked
  daemon.
- **`make reset-all` — a true full reset** across both modes (host daemon,
  compose stack with every profile enabled, stray networks/volumes resolved by
  exact name, a named allowlist of `~/.wardyn` install files; `--dry-run`
  prints the manifest and doubles as the clean-state proof). `make reset` is
  now host-aware: it offers to stop a live host-mode wardynd instead of
  starting a containerized one into a guaranteed :8080 collision.

### Security
- **Audit log has a durable local fallback, and it now covers every writer.** A
  failed primary (Postgres) audit write is no longer silently swallowed: it is
  logged loudly and spooled to a durable local append-only JSONL fallback
  (`WARDYN_AUDIT_SPOOL`). The spool now sits BELOW masking in a shared recorder
  chain (`maskingRecorder` → `spoolingRecorder` → the store), so a
  `credential.mint` / `run.kill` / egress-deny event is masked + durably
  spooled for EVERY audit writer (API, broker, identity, approvals, sweeper) on
  a Postgres outage — not just the API server as before. This is NOT a
  transactional guarantee: the record call remains fire-and-forget at each call
  site, so it narrows, but does not close, the window where an event could
  still be lost (if both the primary write and the spool append fail).
- **The kill-switch no longer lies.** If any teardown/revocation step fails, the
  run.kill audit records the TRUE outcome (`failure`, with a distinct
  `run.revoke/failure` event) and the API returns 500 so the operator/CLI retries —
  instead of reporting `success` while a minted token may still be live. The cascade
  runs on a context detached from the request (a client disconnect can't half-apply a
  kill) with a bounded revoke retry.
- **Composer LLM egress is governed.** The composer's own third-party model calls now
  use a hardened transport (SSRF/private-IP guard, host allowlist, no ambient-proxy
  trust, redirect pinning) instead of leaking through `http.DefaultTransport`.
- **`wardyn-rec` command injection fixed.** The asciinema `-c` argument is now built
  with allowlist-based POSIX quoting, so a crafted run task/command can no longer
  inject a shell.
- **`wardyn-git-helper` caller authentication + confined devcontainer builds** raise
  the bar on the two documented self-governance gaps (an in-sandbox token-exfil
  channel and untrusted build-time code execution): the git helper authenticates its
  caller, and the build path defaults to network-off with resource caps and stricter
  path validation.
- **Fail closed on the publicly-known demo age key.** The Compose default no
  longer ships a usable age key (it generates an ephemeral one); `wardynd`
  refuses to start if handed the old git-published demo key.
- **Go toolchain 1.26.5** closes GO-2026-5856 (crypto/tls Encrypted Client
  Hello privacy leak; the affected symbols are reachable from the egress
  proxy's TLS-MITM handshake and serve paths).
- Release-cut dependency refresh: go-oidc 3.20, pgx 5.10, ghinstallation
  2.19, anthropic-sdk-go 1.56; lucide-react 1.24 (upstream removed brand
  icons — the GitHub glyph becomes GitBranch), jsdom 29, @tailwindcss/vite
  4.3.2; CI action majors (setup-go v6 with a pinned patch go-version,
  gitleaks-action v3, upload-artifact v7, setup-node v6, pnpm v6, helm v5).

### Fixed
- A fresh `git clone` could not build the `wardynd` image: `ui/pnpm-lock.yaml`
  was ignored and untracked while the Dockerfile uses `--frozen-lockfile`. The
  lockfile is now tracked.
- Removed a prebuilt binary from version control and corrected stale code/docs
  comments.
- Detect-phase honesty on dual-daemon boxes: every entry point now picks the
  SAME docker daemon (`wardyn_pick_docker_host`), locally-built images are
  tagged `:local` instead of `:demo`, and the WSL2 NAT Verify/Record warning
  only fires when the picked daemon actually is Docker Desktop.
- Import scan failures now surface the real server-side cause in the scan
  panel instead of a generic private-repo-credentials guess.
- Setup no longer offers dead-end interactive CLI logins on a sealed
  (containerized) control plane, and the GitHub App card's Needs-setup badge
  gained a click target — it jumps straight to the Credentials step.

## [0.1.0] — pre-alpha baseline

Initial Wardyn control plane: per-run JWT-SVID identity with full
`sub`/`act`/`sponsor` attribution, approval FSM, atomic approval-gated credential
broker, L0/L1/L2 layered egress with proxy-side LLM-key injection, append-only
Postgres audit + PTY session replay, CC1/CC2 confinement gating, and the Docker
Compose deployment.
