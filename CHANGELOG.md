# Changelog

All notable changes to Wardyn are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); Wardyn is **pre-alpha**
and does not yet follow semantic versioning (interfaces are not stable).

**v0.3.1 and older live in [CHANGELOG-ARCHIVE.md](CHANGELOG-ARCHIVE.md).**

## [Unreleased]

### Changed

- **Integrations no longer install tools; the tool side of the integration
  concept is removed.** An integration is a connection — secrets + egress —
  and never decides what is installed in an image. Concretely: naming an
  `anthropic_*` integration no longer conditions the `claude-code` bake;
  instead **every Wardyn-generated recommended image now carries `claude-code`
  unconditionally as standard tooling** (like git or curl — the same
  checksum-verified native install, `genStandardTools` in
  `internal/workspacescan/gen.go`). Repo-own devcontainers and BYO/registry
  images stay verbatim — never injected into. The image cache key is salted
  (`v2`), so every previously built workspace image rebuilds once on next
  use — pre-change images may lack the now-standard CLI and are never
  trusted. (docs/OPERATIONS.md "Every generated image carries the
  claude-code CLI as standard tooling".)

### Removed

- **The Integrations page's Tools tab**, the integration→tool "carries"
  chips in the workspace wizard (base-image, build, and verify steps), and
  the client-side mirrors of the bake conditions. Tools are what the image
  carries; the Integrations surface now speaks only to connections.

## [0.5.0] — 2026-08-12

### Security

- **OIDC sessions now carry a derived admin/member role** (`WARDYN_OIDC_ROLE_MAP`,
  `internal/auth/oidc`'s `deriveRole`). Upgrading forces one SSO re-login: a pre-0.5
  session cookie carries no role and now decodes as no session (`decodeSession`), never
  as an authenticated session with an undefined role.
- **Authorization enforcement: the admin/member role is now enforced, plus
  owner-or-admin scoping.** `requireOperator`/`isOperator` gate on the session's
  role instead of re-checking `WARDYN_OIDC_OPERATOR_EMAILS` directly (the
  allowlist still works — it feeds role derivation via `LegacyAdminEmails`, one
  source of truth instead of two). `GET /metrics` now carries the admin gate
  explicitly. A member is scoped to their OWN runs/approvals on `GET /runs`,
  `GET /approvals` (unscoped), and `GET /audit` (`?run_id=` of an owned run,
  else an empty result — never a cross-user leak); `GET/kill/profile/grants` on
  a run, the recording replay, an approval decide, and the attach-ticket mint
  all use an owner-or-admin gate that answers a foreign resource with the
  byte-identical 404 a missing one gets (no existence oracle). Attach tickets
  now carry the minting principal's role (migration 0034), since the
  interactive-attach WebSocket's `?ticket=` lane authenticates entirely off the
  ticket and never runs the normal session check. `GET /setup/status` redacts
  operator-diagnostic detail (environment checks, resident CLI detection,
  secret names, runner detail) for a member. A member's `inline_policy` on
  `POST /runs` (and its preflight dry-run) is now clamped to the operator's
  default policy ceiling before resolution, and bringing a custom sandbox
  image (`image`) is admin-only. Two routes move from admin-only to
  owner-or-admin: minting an attach ticket and deciding an approval, both
  restricted to the run's own creator (or an admin) either way. A new
  `authz.denied` audit action records a member's admin-surface or BYOI
  denials (not a foreign-resource 404 — that stays silent by design, matching
  the no-existence-oracle rule above).

### Added

- **Kubernetes runner substrate** (`internal/runner/k8s`, `-tags k8s`,
  `WARDYN_RUNNER=k8s`): a second, independent confinement substrate behind the
  existing `substrate.Substrate` seam — wardynd creates/manages sandboxes as
  pods instead of Docker containers. L1 (NetworkPolicy-enforced), not L0
  (structural) like Docker: a boot-time two-phase egress canary proves the
  cluster's CNI actually enforces `NetworkPolicy` before the substrate will
  start at all, refusing to boot otherwise
  (`WARDYN_K8S_ALLOW_UNENFORCED_NETPOL=1` is the loud, logged opt-out). CC1
  out of the box; `WARDYN_CONFINEMENT_MAP`/the chart's `k8s.runtimeClasses`
  pin CC2/CC3 to a registered RuntimeClass. Not at parity with Docker yet —
  no BYOI/devcontainer builds, no `local_dir` mounts, no per-pod PIDs/disk
  enforcement, no k8s ground-truth correlator (see
  `deploy/helm/wardyn/README.md`/`docs/OPERATIONS.md`'s "Known gaps").
- **The Helm chart (`deploy/helm/wardyn`) can now create sandboxes, not just
  the control plane.** `k8s.enabled=true` wires the substrate above into a
  real install: least-privilege `Role`/`RoleBinding` + `ClusterRole` scoped to
  exactly the verbs the substrate issues, a default-deny `NetworkPolicy`
  extended with apiserver egress and a runs-namespace ingress peer, and a new
  `k8s_egress_containment` setup check the console surfaces (Enforcing / Not
  enforcing / Indeterminate). `test/conformance`'s `conformance-k8s` CI job
  now proves the substrate on a real cluster (kind, `disableDefaultCNI` + a
  pinned Calico manifest — kind's default CNI does not enforce
  `NetworkPolicy`); the suite's one L0-specific case self-skips there by
  design (the substrate claims L1, not L0) and a dedicated L1 case proves
  what it actually claims instead. New `.claude/skills/wardyn-k8s-setup`
  skill: cluster prereqs, values authoring, wiring Entra ID App Roles for
  admin/member RBAC, install/verify, and a symptom→cause→fix table.
- **Native SSH into a running sandbox.** `wardynd` serves `ssh
  <run-id>@host` (registered public keys only, owner-only authorization)
  directly into the same tmux session the web terminal attaches to: exec
  (exit-code propagation), the sandbox's own `sftp-server` subsystem, and
  `-L` port forwarding restricted to the sandbox's own loopback. Each
  primitive gets its own audit action (`ssh.exec`/`ssh.sftp`/`ssh.forward`);
  the shell path is recorded exactly like the browser terminal (`ssh-`
  prefixed session key). Off by default (`WARDYN_SSH_LISTEN` unset — no
  listener, no host key even generated). See `docs/SSH.md`.
- **Member console.** The web console is now role- and kind-aware: a
  member's nav hides operator-only surfaces (policy/workspace/secret CRUD,
  BYOI), and the approvals view renders per-kind — `egress_domain` approvals
  a member can decide, `credential`/`tool_call` ones they can only view.
  Getting Started gained a Kubernetes-runner flavor (source-honest copy for
  what the k8s substrate does and doesn't support yet).
- **Signed, published release images.** `.github/workflows/release.yml`
  builds and pushes the four images a release ships (`wardynd`,
  `wardyn-proxy`, `agent-claude-code`, `agent-codex-cli`) to
  `ghcr.io/cjohnstoniv/<name>` on a `vX.Y.Z` tag, cosign-signs each keylessly
  (Fulcio/Rekor via the Actions OIDC token), and attaches a CycloneDX SBOM
  release asset via the existing `make sbom` target. linux/amd64 only today.
- **CLI confinement-tier aliases + `/healthz` friendly names.** The run
  commands accept `--confinement fence|wall|vault` as aliases for CC1/CC2/CC3
  (with trust-model flag help), and `/healthz` now exposes a
  `confinement_names` CC-code→friendly-name map (mirroring the console's
  `cc-meta.ts`) so a scriptable consumer learns "CC1" means "Fence" without
  hardcoding it (`internal/api/server.go`, `commands.go`, `types.go`).
- **Sandbox image builder setup check (`env_builder`).** `/setup/status` now
  reports whether the per-run image builder is wired — the path a
  devcontainer build or a `--image` (BYOI) run needs. INFO (never a warning)
  when off, the bare-binary default, so a `--image`/devcontainer run that
  would otherwise silently no-op reads as a real, fixable checklist row.
- **Non-blocking model-resolution warning.** A codex or managed-subscription
  run whose model access resolves ambiguously now surfaces an advisory
  warning (`resolveRunLLMAccess`/`runNeedsModelWarning`, `runs.go`) instead of
  failing opaquely at dispatch.

### Fixed

- **`ssh.forward` audit rows survived a killed session.** A client that
  killed its whole SSH session mid-`-L`-forward could race
  `handleSSHConn`'s connection-teardown context cancellation against
  `handleSSHDirectTCPIP`'s own trailing `ssh.forward` audit write — caught
  live by the SSH e2e's `-L` forward step. The write now runs on the
  daemon-lifetime `BaseCtx` instead of the connection's own (soon-cancelled)
  context, the same fix already applied to the shell path's `session.detach`
  write; the identical latent bug in the `ssh.exec`/`ssh.sftp` trailing
  writes was fixed alongside it. Pinned by
  `TestSSHGateway_ForwardAuditSurvivesKill`.
- **The wizard's Build step showed only a bare spinner — the real image-build
  output went solely to wardynd's own log, invisible to whoever triggered
  the build.** `handleBuildWorkspace`'s goroutine now threads a bounded
  per-workspace log ring (`buildTracker.Log`, 500 lines, oldest dropped)
  through `resolveWorkspaceImage` into the `api.ImageBuilder` call as an
  explicit `logSink io.Writer`; the wardynd docker adapter tees it with the
  existing slog sink so operator logs keep receiving every line unchanged.
  `GET`/`POST /workspaces/{id}/build` now carry `log` in the response, and
  `step-build.tsx` renders it in a scrollable pane that stays up through the
  done/failed states too — the failure line plus the log is the debugging
  story.
- **Editing an onboarded workspace through the "Edit source…" dialog could
  silently destroy it.** The legacy single-form edit dialog rendered blank
  for any multi-source workspace, and its save path submitted the
  deprecated scalar shape — which `decodeWorkspaceRequest` folds into
  exactly ONE source, collapsing `sources[]` and wiping
  Requirements/Profile/ApprovedEgress on save. `AddWorkspaceDialog` is
  retired; the "Edit workspace…" kebab item (workspaces.tsx and
  workspace-detail.tsx) now opens the same wizard used for onboarding,
  hydrated from the row (sources, base image, requirements) and landed on
  whatever step the workspace hasn't cleared yet, saving through the
  composition-shape `sources[]`/`base_image` PUT the wizard's own Base
  image step already used.
- **An outside click or Esc could strand a half-onboarded workspace
  mid-wizard with no way back.** Most steps (including Build) have no
  explicit Close button, and dismissing the dialog never deleted anything
  server-side, so a stray outside-click or Esc left the operator locked out
  of a workspace they'd started onboarding. `WorkspaceWizard`'s
  `DialogContent` now blocks outside-click and Esc dismissal once a
  workspace exists and the step isn't Done — the same condition its own
  footer note already warns about. The X button stays a deliberate
  one-click close either way, and the "Edit workspace…" fix above gives the
  operator a way back regardless.
- **Recording-replay CSP (`script-src 'wasm-unsafe-eval'`).** The asciinema
  WASM replay player calls `WebAssembly.instantiate()`, which a bare
  `default-src 'self'` CSP refuses — the player renders its chrome but never
  plays (duration stuck at `--:--`). `script-src` now adds `'wasm-unsafe-eval'`
  (WASM compilation ONLY — not `unsafe-eval`, no JS `eval`/`Function`), so
  replay plays while scripts stay locked to same-origin.
- **FAILED-run reason surfaced.** A run that ends in FAILED now carries a
  human-readable reason instead of a bare terminal status.
- **`scripts/ci-run.sh` teardown.** The CI one-shot now tears its compose
  stack down cleanly on exit.
- **`WARDYN_LOCAL_MODE` bypass under preserved OIDC.** `scripts/up.sh` now
  warns when local-mode would silently bypass a still-configured OIDC backend
  (preserved config), and documents the registry `PORT=0` caveat.

### Documentation

- **Positioning pass (W3).** README/ARCHITECTURE/OPERATIONS/TRY-IT and the
  threat model now surface the v0.5 moat honestly: the `wait_for_review`
  in-flight connection hold vs. the Enterprise-only analog in Vault/Teleport,
  the Apache-2.0 no-paid-tier + audit-completeness framing, and the L1/L2
  metadata-server defense-in-depth.
- **Kubernetes platform requirements.** The Helm chart README's
  Prerequisites now state the full platform contract in one place:
  Kubernetes 1.20+, Helm 3, a NetworkPolicy-**enforcing** CNI (verified by
  the boot-time egress canary, which refuses a non-enforcing substrate),
  Postgres 12+, and the optional RuntimeClass (CC2/CC3) and OIDC add-ons.

## [0.4.5] — 2026-08-11

### Added

- **Workspaces split into three tiers: a shared source library, a shared
  base-image catalog, and the workspace as the aggregate that composes them.**
  A repo or directory's requirements (secrets, hosts, write paths) are now
  configured once as a library **source** and attached to any number of
  workspaces; base images became a shared **catalog** ("recommended" stays a
  per-workspace derived build). Shipped expand-only and wire-compatible
  (existing clients keep sending `sources[]`), and a source's own re-scan
  only fills missing contract rows, never overwrites an operator's edit. The
  tiers are first-class in the console too: the Workspaces page carries a
  Directories & repos · Base images · Workspaces tab strip, the
  Getting-started rail carries the same three, in that order, as dedicated
  "Your work" steps, and the Add-workspace wizard composes from them —
  attaching a library source or picking a catalog image instead of
  re-declaring either. See `docs/OPERATIONS.md` ("Workspaces: three tiers")
  for the new endpoints, CLI, fold precedence, delete-in-use behavior, and
  overrides' reachability.
- **Record's verify loop closes: approving a held host writes the contract row,
  immediately, on the right tier.** A confined verify session now holds an
  off-policy host at the door; approving it durably writes an `egress:<host>`
  row into *that workspace's* contract, never the shared source (a plain
  run's approval isn't durable). A verify session's own holds stay
  egress-only by construction — that door can raise an egress approval but
  never request a secret; the approval queue elsewhere also carries
  credential and tool-call holds (`internal/types.ApprovalKind`). See
  `docs/POLICIES.md` ("`first_use_approval` modes") and `docs/TRY-IT.md`
  ("Level 2.5") for the write-back mechanism.
- **Corporate network is its own Getting-started step, and it comes before
  Integrations** — because on a corporate network every integration after it
  depends on the path it configures, and discovering that at the point an
  integration fails to validate is too late. Two tabs: **Host proxy** presents
  what Wardyn detected in the host's environment as evidence with a "Use this"
  next to each row, rather than asking an operator to retype what the machine
  already knows; **Egress redirection** holds the redirects.
- **The upstream proxy URL is no longer forced to be a secret.** `SiteConfig`
  gained `upstream_proxy_url` beside the existing `upstream_proxy_secret_ref`.
  Most corporate proxy URLs carry no credential, and making every operator
  mint a secret to store `http://proxy.corp.internal:3128` taught the wrong
  lesson about what a secret is. A URL that *does* embed `user:pass@` is
  rejected server-side by `validateSiteConfig` and must go to the secret store
  — enforced in the API, not merely discouraged in the UI, because the client
  is not the thing standing between a credential and the config document.
- **Two connectivity probes that actually probe**: `POST
  /api/v1/site-config/test-proxy` and `POST /api/v1/site-config/test-redirect`
  (operator-only, audited). Each launches a throwaway confined sandbox and
  makes a real request through the path a run would take, returning `reached`,
  `blocked`, `bypass`, or an honest `no_runner`. The redirect probe's second
  fetch is the interesting one: it re-requests the public host with the proxy
  deliberately bypassed, which catches a redirect that is configured but not
  enforced — a state that looks identical to a working one until a run quietly
  pulls from the internet. curl's exit codes are reported as what they mean
  (DNS, refused, TLS, timeout) rather than collapsing into "failed". These are
  the only test buttons in the product; everywhere else Wardyn still refuses to
  claim it verified a credential it cannot dial.
- **The container login announces itself before anything launches.** The login
  pane now opens on a numbered "what happens next" (a sandboxed login run, a
  claude.ai tab, what gets stored) instead of jumping straight to a terminal
  and browser tab with no warning; the AWS flow gets the same treatment. The
  login terminal no longer overflows or dwarfs its dialog, and a prior capture
  now shows its age with a "Log in again" option instead of reverting to a bare
  "Log in" as though nothing had happened.
- **The Requirements step reads in dependency order, and Verify is what closes
  it.** The tab strip was Record · Egress · Secrets · Files & services; it now
  reads Reach · Secrets · Files & services · Verify — the wizard shows only the
  first three (Verify is its own rail step); the fourth tab is the detail
  page's. The Base image step dropped every AI-specific sentence and now speaks
  only in tool inventory: `claude-code` appears as a chip exactly when a named
  `anthropic_*` integration bakes it into the recommended build — no image is
  ever inspected or has tools injected into it otherwise. See
  `docs/OPERATIONS.md` ("A named Anthropic integration bakes the claude-code
  CLI; nothing bakes codex-cli").
- **The leak banner earns two tiers.** Seventeen red rows of a repo's own test
  fixtures — fake keys that exist because the tests need key-shaped strings —
  train an operator to ignore the banner, the exact reflex it exists to
  prevent. Findings under test-conventional paths (`*_test.go`, `testdata/`,
  `__tests__/`, `*.test.*`/`*.spec.*`, `fixtures/`) now collapse into one
  muted, expandable line ("usually fixtures; confirm they're fake — they mount
  like everything else") on the wizard, the workspace page, the Done step's
  carry-forward and the list's attention cell; the red headline is reserved
  for findings outside them. Never suppression: still shown, still counted.
  And the step's lede stops overclaiming: the scan reads the directory the way
  a run would mount it — gitignored files included.
- **One Add flow, search-first.** "Add integration" opens on "type what you're
  connecting" with the categories browsable below it. Picking lands where a
  real question remains and nowhere else: "Anthropic" still splits into API
  key vs Claude subscription so it opens that choice preselected, Bedrock opens
  on its credential lanes, an OpenAI key goes straight to connect, and a git
  host lands on the SCM ladder. The old "AI provider or SCM host?" card grid is
  gone — by the time anything opens, that question has always been answered by
  the pick itself.
- **An integration is any named external system, not just a model provider or a
  git host.** The Integrations page defined itself as "named connections to the
  systems outside Wardyn" and then offered two examples of one. It now covers
  package & artifact feeds, container registries, cloud providers, data stores,
  MCP servers, work tracking, observability, and an **Other service** catch-all
  for anything unlisted. Each integration answers four questions about one
  system: where it lives (its hosts), what credential it takes, how that
  credential reaches the request, and what it powers.
- **`types.Integration` gained `hosts`, `header`, `format` and `docs`**, so a
  row carries its own contract. The eight new categories derive nothing from
  their `type` — it is an open slug — which is what lets a system Wardyn has
  never heard of be added with no backend change, through the generic
  proxy-injection path that already ships.
- **A workspace's requirements can name an integration** (`integration:<id>`,
  alongside `secret:` / `egress:` / `write:`). Its hosts join the run's
  egress allowlist and its header credential is injected proxy-side, one
  grant per host. NOTHING IS AMBIENT: configuring an integration grants
  nothing until a run is granted it, and an operator with fifty configured
  and a workspace that names none gets a spec byte-identical to having none
  at all — except for model access specifically, where an `ai_provider`
  integration marked the site-wide `agent_runs` default folds into every
  run regardless (see "Model access resolves" below).
- **Preflight names an integration a workspace requires but nobody
  configured.** The runtime fold degrades silently on purpose — a workspace may
  state an intent before the integration exists, and a missing one must never
  brick a run — but "opens nothing, says nothing" is the wrong answer at the
  point an operator is asking what a run will actually get. The checklist now
  carries a row per required integration saying what it opens and whether a
  credential rides, including the case where the path opens and the credential
  does not. It stays amber rather than red: this is config state, like the
  backend row, not the credential absence the destructive styling is reserved
  for.
- **A Corporate-network egress redirect can take its token from an
  integration** (`token_integration_ref`, mutually exclusive with
  `token_secret_ref`). The integration owns the system and its credential; the
  redirect owns rerouting a public endpoint to it. It carries the integration's
  own header and format, so a feed authenticating with something other than
  `Authorization: Bearer` works through this seam and cannot through the other.
  The UI control for choosing one is not designed yet; the seam is usable via
  `PUT /site-config` and `wardyn site-config apply`.

- **A workspace is now a COMPOSITION.** It holds one or more sources — local
  directories, repositories, and ephemeral scratch dirs — instead of exactly
  one source with a kind. Multiples of a type are allowed; the floor is one
  (an ephemeral scratch dir, seeded structurally so the invalid state cannot
  be built). The base image stops masquerading as a source and becomes the
  environment the sources live in; an old `container`-kind workspace migrates
  to an ephemeral source plus a custom base image, which is what it always
  meant. `kind`/`source`/`ref`/`default_target` survive as read-only mirrors
  of a single-source workspace so existing SDK and CLI callers keep working.
  Migration `0029` backfills before it constrains and was proven forward
  against a real Postgres.
- **The requirements contract.** Every control a workspace can carry — a
  secret by name, an egress host, write access to a directory — is a row with
  one axis: **Required** rides along with every run that attaches the
  workspace, **Optional** is a per-run opt-in. `PUT /workspaces/{id}/requirements`
  writes it; launch and preflight fold it through the same function, so Review
  cannot predict something launch won't do. A workspace with no requirements
  declared resolves byte-identically to before. A `scan_seeded` requirement can
  never auto-grant a secret — the scanner reads untrusted repo content, so only
  an operator's direct declaration attaches a credential.
- **Integrations** — one surface for the systems outside Wardyn: model
  providers, git hosts, package feeds, and more. Rows are
  DERIVED from what already exists (stored secret names, site config, setup
  status), so an operator who never opens the page keeps identical behavior and
  one who does can adopt a row to edit it. Each row states what it powers,
  where its credential lives at run time, and — for capabilities that are
  impossible rather than unconfigured — why, as a fact with no control beside
  it. A **Tools** tab names the other half: a tool is what the image carries,
  an integration is what it connects through.
- **Model access resolves** instead of being configured per run: an explicit
  integration on the run, else the workspace's binding, else the operator's
  site-wide default — and below all three tiers, dispatch can
  still credential the run from a managed subscription or a global Bedrock
  config where either is configured and the run's agent can use it. See
  `docs/OPERATIONS.md` ("Model access resolves —
  it does not default to none") for the full precedence. `PUT`/`DELETE
  /integrations/{id}` and `POST /integrations/{id}/adopt` manage stored
  rows; the composer registry derives from the integration marked for
  Wardyn's own features, with `WARDYN_COMPOSER_CONFIG` still winning
  outright when set.
- **A single Add-workspace wizard** — Sources · Base image · Integrations ·
  Build · Requirements · Verify · Done — replacing three inconsistent entry
  points and the six-step import dialog. Custom image builds accept
  Dockerfile steps, and a credential-shaped line warns (naming the line and
  detector, never the value) without blocking.
- **A workspace detail page** at `/workspaces/:id`: the requirements editor,
  the candidates Wardyn noticed but hasn't given to any run, recorded sessions
  and their confined replays, and env-as-code.

- **Push branch-namespace confinement is now REAL, and ON BY DEFAULT.** The
  git-broker route parses the pkt-line command section of a
  `POST …/git-receive-pack` and refuses any ref outside
  `refs/heads/wardyn/<run-id>/` — other branches, the default branch, tags,
  `refs/pull/*`, and deletes outside the namespace all 403 before the
  installation token is minted, with a `brokered:git:branch-ns` deny row in the
  decision log. Only that (≤64 KiB) command section is buffered; the packfile
  still streams, and clone/fetch are untouched. Nothing to turn on: `agent-run`
  now checks each cloned repo out onto `wardyn/$WARDYN_RUN_ID/work` and sets
  `push.default=current`, so a stock run pushes inside its own namespace without
  the operator pinning the convention in task text. Set
  `WARDYN_GIT_BROKER_ENFORCE_BRANCH_NS=false` per proxy to opt out (for an image
  whose `agent-run` predates the run branch); unrecognized values fail closed.
- **The brokered git route is now the only route to those GitHub host names.**
  Dispatch subtracts the four broker-managed GitHub hosts from the egress
  allowlist of any run with git grants **and denies them** (deny beats
  `allow_all_egress` too), and `wardyn-git-helper` no longer mints a GitHub App
  token into a brokered sandbox at all — closing a gap an opaque CONNECT
  tunnel used to leave open. A run with no git grants is unaffected. See
  `docs/POLICIES.md` ("Brokered GitHub: the denies you did not write") and
  `docs/ENV.md` (`WARDYN_GIT_BROKER_REPOS`).
- **A brokered forge is now single-lane: neither `ssh_key` nor `git_pat` can
  ride beside a `github_token` grant for it.** Three seams enforce it: policy
  write refuses a policy declaring both grants for the same forge (`400`);
  dispatch denies that forge's SSH endpoint and withholds any already-stored
  grant from the sandbox; and the mint route refuses either kind for a
  brokered forge outright, closing a direct-POST gap that used to bypass
  `wardyn-git-helper`'s own refusal. A grant for a different host (ADO,
  GitLab, GHES) is untouched. See `docs/POLICIES.md` ("The `ssh_key` and
  `git_pat` lanes are closed too") for the write/dispatch/mint mechanics.
- **Token-side confinement: GitHub ref-ruleset verification, plus an opt-in
  mint gate.** `VerifyRefRuleset` asks GitHub which rules bind a repo outside
  and inside the run's push namespace. A `github_ref_ruleset` setup-checklist
  row grades the first repo a policy names, and
  `WARDYN_GITHUB_REQUIRE_REF_RULESET` (opt-in, default off) turns the same
  check into a pre-mint gate. Branches only — classic branch protection isn't
  visible to this check. See `docs/POLICIES.md` ("Bound the token itself: a
  GitHub ruleset") for the recipe, the bypass-actor rule, and the exact
  fnmatch semantics.
- **wardynd images are published to GHCR** (`.github/workflows/publish-image.yml`:
  main pushes → `:latest` + `:sha-<7>`, `vX.Y.Z` tags → the bare semver the Helm
  chart's default resolves to; `workflow_dispatch` `extra_tag` backfills
  pre-workflow releases), and a **kind-based `helm install` CI gate**
  (`helm-install-test`) proves the chart converges to a healthy control plane on
  every PR — not just that it renders. The image now defaults
  `WARDYN_DEFAULT_POLICY=/examples/policies/default.json`, fixing the crash-loop
  every default `docker run`/Helm install previously hit (the Go-relative
  default never resolved from the distroless WorkingDir).

- **`WARDYN_OIDC_OPERATOR_EMAILS` — a minimal viewer/operator gate**, the first
  authorization tier on the control plane, now covering 34 routes. Listed
  operators keep full access; every other signed-in human becomes a **viewer**
  — reads everything, can launch/kill runs, but is 403'd on configuring the
  deployment, secret writes, approval decisions, and sandbox attach. Additive
  (unset keeps prior behavior); an empty operator list with OIDC configured now
  **refuses to boot** (override: `WARDYN_ALLOW_OIDC_NO_OPERATOR_LIST`).
  Allowlist, not RBAC — run create/kill stay open to any signed-in human. See
  `docs/OPERATIONS.md` ("Who can change what").
- **Three per-process defects that used to make `replicas > 1` unsafe are now
  closed at the code level** — not because multi-replica is supported today,
  but so a future build doesn't need three separate durability projects to get
  there. Session recordings default to a Postgres-backed store visible to every
  replica (`WARDYN_RECORDING_STORE=fs` still selects the legacy per-pod
  directory; the Helm chart deliberately keeps `fs`). Run-watcher adoption is a
  Postgres lease plus a periodic cross-replica sweep, and the ground-truth
  token rotator is leader-elected via a Postgres advisory lock. See
  `docs/OPERATIONS.md` ("One replica, by construction") for the full mechanism
  and what remains per-process by design — the secret-masking registry,
  notably, still fails open across replicas.
  **Upgrade note:** nothing migrates existing on-disk recordings into
  Postgres — they stay on the `recordings` volume, but `Replay` now queries a
  table that has never seen those keys and 404s. Set
  `WARDYN_RECORDING_STORE=fs` to keep replaying pre-upgrade casts (the Helm
  chart already keeps this default for that reason).

- **A named Anthropic integration bakes the `claude-code` CLI into a
  workspace's recommended build.** `AgentToolsForIntegrationTypes` maps any
  `anthropic_*`-typed integration on the workspace to `claude-code`, and a
  generated `.devcontainer/Dockerfile` installs it via the same
  checksum-verified native-binary lane `deploy/images/claude-code/Dockerfile`
  offers under `CLAUDE_INSTALL=native`, running as root before any
  devcontainer feature so it depends on none. The tool set is also the
  build-cache key's input, so naming or un-naming the integration in a
  workspace's own contract invalidates that workspace's cached image and
  the next build reflects it. `codex-cli` is deliberately never baked — no
  Wardyn-verified public native-download contract, and its npm lane would
  need a Node runtime this build stage doesn't carry — so an OpenAI
  integration bakes nothing. A workspace whose repo ships its own
  `.devcontainer` bypasses the generator (and the bake) entirely: Wardyn
  builds that file as-is and never modifies it, on disk or in the image, so
  a repo that wants the CLI has to add it itself. Live-proven against a
  real daemon: a built image's `claude --version` reports a real binary,
  not the inert no-op an earlier attempt silently shipped.

- **Run creation starts from a workspace, not a blank policy, and Describe →
  Review is the default path.** The New Run dialog now opens on "which
  workspace?" before ever offering a task description or the manual wizard;
  picking a workspace seeds both paths from the same selection, and
  "Configure manually" survives as a footer escape hatch carrying the same
  pre-seed. An explicit "No workspace — ad-hoc run" choice reproduces the
  previous ephemeral-scratch behavior exactly. A workspace's Required rows
  apply automatically; Optional rows are opt-in toggles that now actually
  reach the run on the Describe path too, not only the manual wizard. Review
  gained "Edit prompt" (back to Describe with the prompt, selections and
  attachments intact, under the same audit session) and renders the run's
  mode as a neutral fact chip instead of a control that could still be
  flipped after the risk grade below was computed for the other mode.

### Changed

- **The toolchain-fidelity env is requirements-driven, not platform-wide.**
  Dispatch used to set `GOTMPDIR`/`GOCACHE` and the Maven/Gradle JVM proxy
  sysprops (`MAVEN_OPTS`/`GRADLE_OPTS`) for every run on every image. A
  workspace run now gets exactly what its sources' scans detected — the Go
  group only when a scan found Go, the JVM group only when it found
  Maven/Gradle, the union across attached sources — because what a sandbox
  carries follows from the workspace's actual requirements, never from a
  platform guess. Runs with no workspace context at all (ad-hoc, a bare
  `--image` override, scan and login runs) keep the full set: nothing was
  scanned and nothing declared, and "unknown" must not break the proven CI
  and ad-hoc lanes. The in-sandbox consumers were already env-driven no-ops
  when a key is absent.
- **A workspace's Model access binding names an Integration everywhere the UI
  touches it.** The workspace list chip, the detail page's Model access group,
  and its edit dialog all speak the Integration-based binding now
  (`llm_cred.integration_ref`) — the picker lists the AI-provider
  Integrations the server actually knows instead of a mode radio whose
  `api_key`/`bedrock` fields the server had already stopped storing, which
  made "Save" silently clear the binding. The New Run access step's
  "this workspace pins it" resolution matches by the named Integration id
  outright, replacing the old best-effort type matching, and the
  broken-bound-secret dot is gone from the workspace list — the credential
  lives on the Integration, and the Integrations screen is where its health
  shows. The shared workspace types also caught up with the three-tier wire
  (attachments, the base-image reference, the requirements overlay and its
  fold), so the New Run picker and preflight now read the same
  `effective_requirements` contract the create-run gate enforces.
- **Artifact registry overrides became egress redirects.** The ecosystem-keyed
  `artifact_overrides` map is now an `egress_redirects` list of
  `{from, to, token_secret_ref, ecosystem}`, which stops the shape from
  implying that only package registries can be redirected. Two tiers, and the
  UI now says which one a row gets: a known ecosystem (npm, pip, cargo, maven,
  go, nuget) gets both the network substitution and a generated tool config
  (`.npmrc`, `pip.conf`, …); a container registry or arbitrary host gets the
  network half only — the mirror substituted into the run's egress and the
  token injected proxy-side — and is marked `network only`, because there is
  no config file to write for it and pretending otherwise would be the bug.
  Migration `0030` rewrites stored documents; the request decoder still folds a
  legacy `artifact_overrides` body for one release, since `PUT /site-config` is
  a whole-document replace and an operator applying a config file saved in the
  old shape would otherwise silently erase their proxy and every redirect.
- **Network topology has exactly one home, and it is Corporate network.** The
  Integrations page used to carry Host proxy and Egress redirection as two of
  its four categories — the same stored rows the Corporate network step
  configures, with a second set of Test buttons and a second Add flow. Both
  categories are gone from that page, from its Add dialog, and from the
  derivation behind them. A redirect carries a proof obligation (every
  configured one must test *reached* before the step hands off) and that gate
  lives on the step; configuring a row where there is no gate and proving it
  where there is, is what produced the duplicate. The page keeps what it is
  for — named connections to systems outside Wardyn: model providers and git
  hosts — and its proxy-detected banner now offers a button that takes you to
  Corporate network (`/setup?step=…`) instead of a second editor. Retiring
  those panels also retired the last two Getting-started step bodies they were
  still borrowing (`HostProxyStep`, `ArtifactRepoStep`): net ~1,600 lines
  deleted.
- **The Integrations step lost its footer.** "Manage in Integrations" linked
  to the page the step already embeds, and "Skip this step" duplicated Next.
  The step is optional, so moving forward past it with nothing connected *is*
  the skip — it earns the Skipped badge and the checkmark exactly as the
  button did. Backing off it decides nothing.

- **The Corporate network step is a proof, not a form — and only the proof is
  required.** `Next: Integrations` unlocks only once a probe shows a sandbox on
  this host can reach the internet and every configured redirect tests clean —
  but nothing has to be configured, and on most hosts it's one click (Test
  connectivity, `Reached · direct`, Next). While the gate is locked, the
  footer's own button becomes the fix; a blocked probe can be retried against a
  URL you name, for internal-only and air-gapped hosts. See `docs/TRY-IT.md`
  for the `no_runner` exception and `docs/OPERATIONS.md` ("Testing it: two
  probes, not a courtesy button").
- **Probe verdicts say what was actually established, at every altitude.** An
  interception now renders apart from a plain connection failure — a reply
  that arrives but doesn't match the expected payload is a different problem
  than nothing answering — and a custom-URL pass never wears the "verified"
  treatment, since nothing was actually proven. A server-rejected custom URL
  now renders inline instead of vanishing into a toast. See
  `docs/OPERATIONS.md` ("Testing it: two probes, not a courtesy button") for
  the `reached`/`blocked`/`bypass`/`no_runner` state semantics.

- **Getting started is twelve steps, not thirteen.** The model-provider,
  host-proxy, SCM-provider, artifact-registry and credentials steps were five
  rail entries for one activity; they collapsed into one optional
  Integrations step. Corporate network came back as its own step, right
  before Integrations — the dependency their ORDER used to encode (you
  cannot reach a model provider through an unconfigured corporate proxy) is
  fixed by that placement itself, not a banner — and the source-library split
  gave Directories & repos and Base images their own steps under Your work.
  Readiness reads the same derived rows the Integrations page renders, so the
  funnel and that page cannot disagree.
- Workspace status collapsed to `pending_scan → scanning → scanned | error`
  and reads as one word in the console: Setting up / Usable / Scan failed.
- The run's Access step no longer asks how to authenticate; it shows what
  resolved and why, with a per-run override.

- **Plaintext HTTP on a specific non-loopback bind is now refused at boot**
  (was: warn-only). Loopback and unspecified binds (compose/`make setup`)
  are unaffected. Migration for TLS-terminating-proxy deploys on a specific
  IP: set `WARDYN_TLS_TERMINATED=true` (or `WARDYN_ALLOW_PLAINTEXT_LISTEN=true`
  to keep the old behavior explicitly).

### Removed

- **The verify pipeline.** `wardyn-verify`, its brokered upload route,
  `POST /workspaces/{id}/verify`, `PUT /workspaces/{id}/setup-commands`,
  `POST .../verify/suggest-fix`, `POST .../finalize` and four lifecycle states
  are gone. It executed an operator-approved command list and wrote "verified"
  onto a row nothing gated on. The environment proof that survives is the
  honest one: record a session, promote what it actually reached, replay it
  confined. Detected build commands are now documentation in AGENTS.md, and
  the emitted devcontainer no longer auto-runs them at create — nothing
  verified them. Finalize's one real job, writing those files into a local
  workspace, is now `POST /workspaces/{id}/env-as-code/write`.

- **The AI Run Composer's per-run "Use my Claude subscription" toggle and its
  `use_subscription` wire field.** A composed run's model access now resolves
  exactly like a manual run's, through `resolveRunIntegration`
  (`internal/api/llmcred.go`): an explicit `integration_id`, else the primary
  workspace's `LLMCred.IntegrationRef` binding, else the operator's
  `DefaultFor: agent_runs` default. Pin a workspace's model access or set the
  operator default once — there is nothing left to opt into per run.

### Fixed

- **`go build` works in every image, not just the full toolchain image.**
  `GOTMPDIR` needs a directory the go tool itself refuses to create, and only
  the full toolchain image pre-baked it, so the first Go command in a
  recommended-built or BYO image failed with `stat: no such file or directory`.
  Two runtime guards now create it from the env var alone whenever a
  workspace's scans call for Go. See `docs/OPERATIONS.md` ("Toolchain-fidelity
  environment") for the mechanism and the measured race window.
- **Scan-seeded secret rows now use storable names — one scanned source no
  longer wedges the Requirements save.** A scan honestly reports the env-var
  name the code reads (`AWS_DEFAULT_REGION`), but a `secret:` contract row
  names an entry in Wardyn's secret store, whose lowercase grammar can never
  hold that shape — the seeder wrote the raw name, so every later save of the
  contract failed validation with "invalid secret name", and the row could
  never have matched a stored secret anyway. Profile names are now mapped
  onto the storable grammar (`AWS_DEFAULT_REGION` → `aws-default-region`) at
  every profile→contract boundary — the server-side seeder (both scan lanes)
  and the UI's seeding, stored-check, and Add-secret prefill, which had the
  same mismatch (an uppercase row never showed "stored", and its Add button
  proposed a name the secrets API rejects). Rows keep the detected env-var
  name as their face with a "stored as" hint beside it, and a test now pins
  the invariant that broke: every row the seeder writes passes the same
  validation any PUT of that contract goes through.
- **"Recommended — built for this workspace" now works out of the box on the
  compose stack.** The default card required four hand-set knobs, and two
  latent bugs killed every real build regardless: the generated build context
  was staged unreachably for the host daemon, and a blanket capability drop
  broke rootfs extraction for any featureful build. The stack now ships a
  loopback registry sidecar with builds on by default, and both bugs are fixed.
  See `docs/OPERATIONS.md` ("Recommended builds on compose") for the four
  pre-wired pieces.
- **`make setup` asks which folder Wardyn may onboard.** `WARDYN_WORKSPACES_ROOT`
  had to be exported by hand on every setup run or local-directory onboarding
  failed against the sealed daemon. The containerized front door now prompts
  for it (default: the previous answer, else **sealed** — a bare Enter
  exposes nothing), refuses `$HOME` outright, remembers the choice in
  deploy/compose/.env, and an explicit env var still wins silently for
  scripts and CI.
- **The custom base-image card's "Tools & features" checklist is gone.**
  None of those checkboxes — including a default-checked "Claude Code CLI"
  toggle — ever reached the build (only the base image and build steps are
  sent), so the honest surface is the base + the steps editor, which is
  what remains.
- **An ephemeral-only workspace lost its Requirements tabs.** The hydrate
  pass derived a workspace's profile from its attached library sources — and
  an ephemeral-only composition has none, so it derived *nil* where the old
  scan had stamped the deterministic empty profile. The wizard's Requirements
  step read that as "No contract yet" and never mounted its tabs — on the
  default scratch-floor path Getting Started walks every new operator into.
  Ephemeral-only now derives scanned + the deterministic empty profile, the
  exact legacy semantic, pinned by a store test.
- **Verify's held-at-the-door approval never actually held.** Confined verify
  sessions ran `deny_with_review` on a rationale written for a session shape
  that no longer exists ("an unattended probe must fail fast") — every record
  session has been interactive since named sessions landed, so the operator
  was present, watching a live-approval strip built for holds that never
  happened: the probe was already denied by the time they clicked approve,
  and the approval helped only a manual retry. Confined sessions now run
  `wait_for_review` — the connection parks at the door, approve releases it
  in-flight (and writes the contract row), deny or timeout fails it.
- **A repo+dir workspace never scanned its directories.** The whole-workspace
  scan was a 3-branch switch where the repo branch won on every call: it
  launched the repo's governed scan and promised the local directories would
  be scanned "by a later call" — but the later call re-entered the same repo
  branch, so the dirs' profiles never landed and their secrets/egress needs
  never reached the requirements contract. Scanning is now per *source*
  (which is what made the bug structural rather than patchable): every
  attached directory scans host-side inline and every attached repo launches
  its own governed run, each fenced on the source's own `active_run_id`, and
  the workspace's profile is the merge of whatever its sources know. One
  scan click covers every source, including the mixed case.

- **A local-directory scan failure now says WHY when the daemon can't see the
  host.** On the compose stack wardynd runs sealed and sees only what
  `WARDYN_WORKSPACES_ROOT` mounts in — nothing, by default — so "local
  directory not found on this host" fired for directories that plainly exist,
  reading as a lie and pointing at no fix. The 422 detail (the wizard's failure
  headline, verbatim) now distinguishes: outside the configured root (names the
  root), no root configured in a containerized daemon (names the env var and
  `make setup`), or a plain host-mode miss. Compose passes the root into the
  daemon's environment so it can name it.
- **A credential header could never carry a port-qualified host, and the run
  paid for it.** `CompilePolicy` files a `host:port` allowlist entry under
  `allowedExactPort`, which `AllowedExactHost` never consults — so an injection
  rule for such a host is refused by `buildInjector`, and that refusal is a hard
  proxy startup failure rather than a missing header. Writing an integration
  that pairs a credential header with a wildcard or port-qualified host is now
  rejected with the reason, and the runtime skips such a host independently for
  rows stored before the guard.
- **An operator-authored HTTP header name is validated before it reaches the
  wire.** An `api_key` grant scope in a stored or inline policy carried its
  header name to the proxy with no validation at all; it is now checked at every
  write boundary and again at the injection sink, which fails closed and audits
  before reading the secret. Go's transport already rejected a malformed field
  name, so this is defense-in-depth and an earlier, readable failure — a 400 at
  write time instead of a broken run.
- **The connectivity probe no longer tests github.com, and reads the reply
  rather than the exit code.** Two ways it lied on exactly the networks it
  exists for. Plenty of organisations block GitHub outright, so a healthy
  corporate network reported "no internet" — tolerable for a diagnostic, not
  for something that now gates setup. And it discarded the response body, so a
  corporate block page (a well-formed HTTP 200) scored as `reached`, waving an
  operator through while nothing could get out. It now tries the endpoints
  Windows and Firefox use for their own connectivity detection — blocking those
  breaks the OS network indicator — and matches their known payloads, the way
  every captive-portal detector works. A reply that arrives but doesn't match
  is reported as interception, a state that was previously invisible.
- **`Test proxy` returned 400 on every click.** The endpoint takes no fields,
  so the UI POSTs no body, and the strict decoder read that as malformed. It
  also refused to run at all without a proxy configured — backwards, since "can
  a sandbox here reach the internet?" matters most where nothing is set up yet.
  It now runs either way and says which path it took.
- **A failed request is no longer reported as a `Blocked` probe verdict.** Both
  Test buttons caught every thrown error and rendered it as the state meaning
  "the network would not let this through", so a 403 from lacking the operator
  role, or a wardynd that restarted mid-click, told the operator their proxy
  was blocking them and sent them to debug a working firewall. That inverts the
  reason these buttons are permitted at all. Failed requests now surface as
  themselves and the control returns to Not tested.
- **The Integrations page's proxy-detected banner kept nagging even after a
  plain, non-secret `upstream_proxy_url` was configured** — its suppression
  check read only the secret-ref proxy field. It now counts either field.
  (The page itself derives no row for a proxy or redirect at all — that
  surface lives on Corporate network alone; see Changed, "Network topology
  has exactly one home.")
- **The Add-integration dialog's registry redirect could not save twice.** Its
  step body still wrote the deprecated `artifact_overrides` map while sending
  the whole document back, and the server refuses a body that sets both shapes
  rather than guessing which one wins — so the second save always 400'd, citing
  a field the operator never typed. Fixed to write `egress_redirects`; that
  step body has since moved out of this dialog entirely, folded into the
  Corporate network step's own editor (`corp-network-egress.tsx`).

- A confined replay of a multi-repo workspace silently omitted the clone host
  of every non-GitHub repo past the first (the helper read the single-source
  mirror field). GitHub sources masked it entirely, since they route through
  the broker and need no allowlist entry.
- `PUT /site-config` would have deleted every stored integration when an older
  client round-tripped a document written before integrations existed. The
  handler now refuses such a body and carries the stored rows forward itself.
- Preflight folded only the workspace tier of model-access resolution, so a run
  whose access came from an explicit integration or the operator's default
  previewed as having none.
- Ephemeral workspace targets were named in the sandbox environment but never
  created — nothing on the other side read the variable.

- **Opting a proxy out of push branch-namespace confinement is no longer
  silent.** `WARDYN_GIT_BROKER_ENFORCE_BRANCH_NS=false` used to produce no
  signal anywhere — no boot line, no distinct audit row — while the per-mint
  `branch_namespace` metadata was written identically either way, so a run on
  an opted-out proxy read exactly like a confined one and the posture was only
  discoverable by inspecting the sidecar's environment. `wardyn-proxy` now
  logs a one-shot `slog` WARN at boot when enforcement is OFF (the sibling of
  the `WARDYN_LLM_SCAN` kill-switch line, at WARN because this control is ON by
  default), and every push it then forwards unparsed carries the decision-log
  `rule_source` `brokered:git:branch-ns-off` instead of `brokered:git`, so the
  posture is provable per push in the append-only audit log rather than
  inferred. Enforcement itself is unchanged, and a garbage value still fails
  closed.
- **The minted GitHub installation token is registered with the proxy's secret
  mask registry.** Injector credentials were registered (`internal/egress/proxy/inject.go`)
  but the git-broker's token was not, so `httpError`'s `maskDecisionBytes`
  — the mask every sandbox-facing error string passes through — did not know
  the bytes. No live leak was found (the token is set as Basic auth on the
  outbound request only); this makes the redaction a property of the token
  rather than of the current call sites. Verbatim bytes only, the same honest
  residual the injector path carries.
- **Workspace scan/verify/record clone grants are scoped to the repo they
  clone.** `maybeGitHubReadGrant` synthesized a `github_token` grant with
  `"repos": []` while the git-broker allowlist it is reached through was keyed
  from the CLONE URL — two different answers to "which repo is this token
  for". The real minter refuses an empty repo list outright (a GitHub
  installation token is per-installation and the owner is derived from the
  first repo), so every GitHub HTTPS scan/verify/record clone would `502` at
  the broker route once a real GitHub App was configured. No test caught it
  because `broker.FakeGitHubMinter` did not reproduce that precondition; it
  does now, so the guard is enforced in tests exactly as in production. A
  `github.com` URL with no derivable `<org>/<repo>` now yields no grant at all
  rather than an unmintable one.

- **A workspace pinned to a subscription integration previewed as api-key at
  Review, then launch silently granted the subscription instead.** The
  compose pipeline resolved its run-level integration with an always-empty
  workspace ref, so the proposal skipped the workspace-binding tier entirely
  while `foldRunIntegration` read the real binding at launch — a Review↔Launch
  divergence. `primaryWorkspaceLLMRef` (`internal/api/compose.go`) now
  resolves the compose request's primary workspace against the onboarded
  workspace list the same way `referencedWorkspaces` does, so Review can no
  longer disagree with what Launch grants.
- **The wizard's Build step showed only a bare spinner — the real image-build
  output went solely to wardynd's own log, invisible to whoever triggered
  the build.** `handleBuildWorkspace`'s goroutine now threads a bounded
  per-workspace log ring (`buildTracker.Log`, 500 lines, oldest dropped)
  through `resolveWorkspaceImage` into the `api.ImageBuilder` call as an
  explicit `logSink io.Writer`; the wardynd docker adapter tees it with the
  existing slog sink so operator logs keep receiving every line unchanged.
  `GET`/`POST /workspaces/{id}/build` now carry `log` in the response, and
  `step-build.tsx` renders it in a scrollable pane that stays up through the
  done/failed states too — the failure line plus the log is the debugging
  story.
- **Editing an onboarded workspace through the "Edit source…" dialog could
  silently destroy it.** The legacy single-form edit dialog rendered blank
  for any multi-source workspace, and its save path submitted the
  deprecated scalar shape — which `decodeWorkspaceRequest` folds into
  exactly ONE source, collapsing `sources[]` and wiping
  Requirements/Profile/ApprovedEgress on save. `AddWorkspaceDialog` is
  retired; the "Edit workspace…" kebab item (workspaces.tsx and
  workspace-detail.tsx) now opens the same wizard used for onboarding,
  hydrated from the row (sources, base image, requirements) and landed on
  whatever step the workspace hasn't cleared yet, saving through the
  composition-shape `sources[]`/`base_image` PUT the wizard's own Base
  image step already used.
- **An outside click or Esc could strand a half-onboarded workspace
  mid-wizard with no way back.** Most steps (including Build) have no
  explicit Close button, and dismissing the dialog never deleted anything
  server-side, so a stray outside-click or Esc left the operator locked out
  of a workspace they'd started onboarding. `WorkspaceWizard`'s
  `DialogContent` now blocks outside-click and Esc dismissal once a
  workspace exists and the step isn't Done — the same condition its own
  footer note already warns about. The X button stays a deliberate
  one-click close either way, and the "Edit workspace…" fix above gives the
  operator a way back regardless.

- **The manual wizard's "Edit in wizard" path could launch a HIGH-risk run
  with no acknowledgment gate, because the manual path carried no risk grade
  at all — two paths sharing one spec, with opposite consent requirements.**
  `POST /runs/preflight` now returns the same deterministic grader verdict
  the AI Composer computes (the response carries the grade and its overall
  level, computed on the resolved spec with the ENFORCED confinement class
  folded in — grading previously ran against the pre-raise floor, so a run
  could grade "HIGH: Fence" while actually launching at Vault). The wizard's
  Review step now renders the same risk panel the Composer does, and a HIGH
  grade gates Launch behind the same explicit acknowledgment, reset on every
  fresh preflight so a stale ack cannot survive a spec change.
- **A workspace card could render 40+ junk "(secret) won't auto-grant"
  checkboxes.** Every environment-variable read the scanner found in scanned
  code (`HOME`, `MODE`, `USERPROFILE`, `NODE_ENV`, …) became an advisory
  secret need, crowding real needs out of the requirements cap. A closed set
  of platform/build env names is now filtered at the one boundary both the
  host-side scan and the in-sandbox scan cross, and a rescan of a previously
  junk-only source now heals: the scan-seeded subset rebuilds on every
  successful scan (an operator's own rows still win) instead of only when it
  had never been populated.
- **Three more findings from this release's own adversarial review, closed
  before ship.** A Git-PAT "Add secret" could wire the PAT as the model's
  Anthropic API key instead of a git credential — the shared handler no
  longer writes the LLM secret from any control but the dedicated
  model-access one. A workspace composed of more than one source (the
  three-tier model above) could not actually be launched from the console —
  both run-creation doors now iterate a workspace's sources instead of
  assuming a single one. And deleting an integration now says, correctly,
  that it removes the integration and its default-for mark but never the
  underlying stored secret, which another integration may still share.

### Security

- **The proxy's local credential-mint route no longer hands a live GitHub
  installation token to an unauthenticated in-sandbox caller.** Any process
  inside a sandbox with a brokered git grant could `curl` the route directly
  and receive the same live App installation token the credential helper
  would have minted — a bypass of the per-repo allowlist the git-broker
  exists to enforce. The route now refuses an unauthenticated in-sandbox mint
  of a broker-served grant. Affects 0.4.4 and earlier with
  `WARDYN_GIT_BROKER_REPOS` configured.

## [0.4.4] — 2026-08-02

The final solidification release before the K8s/corporate extension work: a
full-repo verified sweep (159 adversarially-confirmed findings applied across
backend, console, CLI/SDK, gates, deploy and docs) that fills the parity gaps,
hardens the defaults, and makes the remaining claims true.

**Operators upgrading:** no schema changes. Three deliberate breaking changes
below (CLI flag removal, SDK return type, Helm chart defaults) — each has a
one-line migration.

### Added

- **`/metrics`** — first observability surface: Prometheus text exposition
  (stdlib-only), admin-gated beside `/healthz`; runs by terminal state,
  approval decisions, egress denies, credential mints, sandbox launch latency.
- **`wardyn workspace` command family** (`create|list|get|delete|scan`) —
  `create` clears the run-create onboarding gate that previously made
  workspace-mount policies unusable from the CLI alone.
- **`wardyn run --dry-run`** (launch-parity preflight incl. `--workspace`
  seeding), **`run grants`**, **`run recording`** (download a session cast),
  `run --workspace <id>`, `--devcontainer-repo/-ref`, and POST /runs advisory
  warnings surfaced on stderr (SDK: `CreateRunResult.Warnings`).
- **Console:** guided workspace import reachable from `/workspaces` (no more
  walking back into Getting Started), env-as-code regeneration dialog,
  recording download, run exit codes, attach-session recordings listed on run
  detail, eBPF ground-truth health chip on Audit, kill-run dialog unified,
  SSO sign-in button live when OIDC is configured, truncation banners, poll
  failure escalation to an unreachable banner.
- **wardynd version identity** (`internal/version`, surfaced on `/healthz`) and
  a `GET /workspaces/{id}/env-as-code` endpoint (finalize's committable files,
  re-fetchable any time).
- **Server-side audit query predicates** (`since`/`until`/`action_prefix`/
  `actor_type`/`outcome`) and a `run_id` filter on `GET /approvals`.
- **Security hardening:** response security headers (CSP et al.) on the
  console; OIDC PKCE verifier lengthened to the RFC 7636 floor; the composer
  LLM transport refuses HTTPS→HTTP redirects and no longer inherits wardynd's
  full environment; proxy listener header caps; sandbox-facing request-body
  caps on every /internal route; MITM error strings masked; boot refusal on
  the published demo admin token bound to a routable address; opt-in
  recordings retention (`WARDYN_RECORDING_RETENTION_DAYS`, off by default).
- **Gates:** `tidy-check` and a pnpm-audit UI vulnerability gate joined
  `make ci`; helm-lint renders a non-default value matrix; image-pin and
  compose-config gates cover every compose file; the SPDX gate covers shell;
  UI `noUnusedLocals`/`noUnusedParameters` + `ui/e2e` typechecking; a
  stale-citation guard over Go comments; a RunPolicySpec field reference
  (docs/POLICIES.md) gated against the struct.
- **Docs:** `docs/OPERATIONS.md` (the four state stores, backup/restore,
  forward-only migrations, rotation honesty, the single-replica constraint),
  git credential routing matrix in ARCHITECTURE.md, threat-model
  authorization-gap section, WARDYN_AUDIT_SINKS schema.

### Changed

- **BREAKING — `pkg/client.CreateRun` returns `(CreateRunResult, error)`**
  (the run plus server advisory warnings). Migration: `res.AgentRun` is the
  old value.
- **BREAKING — the Helm chart refuses to render without auth** (set
  `auth.adminToken.secretRef.name` or `env.WARDYN_OIDC_ISSUER`) and its
  **NetworkPolicy ingress default tightened from all-namespaces to
  same-namespace** (cross-namespace clients now need an explicit
  `networkPolicy.ingress.from`). The chart also stopped crash-looping on its
  own defaults (recordings dir), sources the admin token and age key from
  Secrets, and pins `automountServiceAccountToken: false`.
- **An audit webhook sink configured with a `bearer_token` now requires an
  `https://` URL.** The token is a long-lived SIEM ingest credential replayed on
  every POST, so a plaintext endpoint leaked it continuously. wardynd refuses to
  start instead. A tokenless `http://` collector is unaffected.
- The compose wardynd healthcheck, decision-ingest idle touches (debounced —
  the reaper adds the same 30s as threshold slack, so `run.autostop`'s
  `threshold_sec` records configured + 30), and preflight/launch workspace
  parity (workspace_id seeding, credential-binding fold keyed on the grant's
  own secret, target-collision 422) were all tightened; `ci-run.sh`'s
  preflight preview now rides `wardyn run --dry-run`.
- Semantic status text now clears WCAG AA in BOTH themes (light info/cyan to
  the 700 family; dark danger/info to the 400 family with dark text on the
  danger fill), and the contrast gate proves the dark theme too.
- Approvals reads scale: a `(grant_id, requested_at DESC)` index serves the
  broker's per-mint lookup, and the console's unfiltered approvals list pages
  at the database instead of transferring the full decided history per poll.
- The missing-test inventory is back — `make test-gaps` regenerates
  `docs/TEST-GAPS.md` from the coverage artifacts (the prior copy was deleted
  as stale generated output; the regeneration wiring is the fix).

### Removed

- **BREAKING — `wardyn secret set --value`.** Values are stdin-only (argv
  leaked into shell history and `ps`). Migration:
  `printf '%s' "$VALUE" | wardyn secret set <name>`.
- **`POST /api/v1/runs/compose/telemetry`** (vendor-style funnel beacon in a
  self-hosted product; the compose session id already threads the audit
  trail) — now 404, and the console no longer calls it.
- The unreachable `ui/e2e/live` suite, `scripts/run-local.sh`, the runner
  capabilities `warm_pools` field, and a set of dead symbols
  (`store.Store.GetGrant`, proxy hold-for-review knobs, `shouldOpenSetup`,
  Fleet-era comments).

## [0.4.3] — 2026-07-29

A clarity release: no new features, no API changes. A repo-wide audit rewrote the
docs around a single quickstart, redrew the architecture diagram, and fixed the
places where the docs described something the code does not do. It also found two
CI gates that had been passing without checking anything.

**Operators upgrading:** nothing to do — no schema, config, or interface changed.

**Maintainers:** the CI merge gate collapsed five single-command jobs into one
`gates` matrix, so the required status checks on `main` must be renamed from
`govulncheck`/`staticcheck`/`gitleaks`/`licenses`/`license-headers` to
`gates (<name>)`. Until that is applied, a pull request waits forever on contexts
that no longer report. See [RELEASING.md](RELEASING.md), "Repo settings".

### Fixed

- **The nightly live e2e jobs were passing while their assertions failed.** Both
  steps pipe `make` into `tee` to capture evidence, but GitHub's implicit shell has
  no `pipefail`, so the step took `tee`'s exit code. These are the jobs that prove
  the L0 egress boundary, the metadata block, and the kill cascade.
- **`make dco` accepted any non-empty sign-off**, including `Signed-off-by: nobody` —
  the format assertion was lost when the check moved to git's trailer parsing.
- **The README claimed gVisor/CC2 is your default barrier.** `pick_policy` checks for
  a configured model first, so a gVisor host running a model gets a CC1 floor. All
  three outcomes are now stated.
- **`docs/ENV.md` documented an admin-token default that does not exist**, so a
  reader who set only `WARDYN_TOKEN` got an unauthenticated control plane: the real
  default is empty, and an empty token with no OIDC on loopback auto-enables no-auth
  local mode. Both the binary and compose defaults are now stated.
- **The compose README described the `make setup` menu backwards**, and told readers
  team/SSO was "coming soon" where the roadmap says it is not scheduled.
- **Example scenario commands now run as written** (`--policy`, `wardyn run list`,
  `wardyn deny`, and a policy file that actually parses).
- **The Helm docs quoted three different, all-wrong chart versions.**
- Three API payloads emitted `null` where they had emitted `[]`, one of them a live
  response body.

### Changed

- **The console's default sizing is back to 100%.** 0.4.0's 17.6px root font token is
  16px again, the sizing that shipped through 0.3.1. The `px`→`rem` text-utility
  conversion stays — those values were computed against a 16px base, so each reproduces
  its original size — and `--font-size` in `ui/src/styles/theme.css` remains the single
  knob for anyone who wants the larger console back.

- **Docs consolidated and de-duplicated (repo-wide audit).** The quickstart is told
  once (README → [docs/TRY-IT.md](docs/TRY-IT.md)); `docs/FRESH-START.md`,
  `docs/ADO-GIT-BROKER.md` and `docs/TEST-GAPS.md` are gone (the troubleshooting
  table moved into TRY-IT); pre-0.4 release notes live in
  [CHANGELOG-ARCHIVE.md](CHANGELOG-ARCHIVE.md); new [docs/README.md](docs/README.md) index.
- **Diagrams redrawn and gated.** The system-overview diagram has no edge
  crossings and is inlined in the README (the stale `architecture.png` is gone);
  `make diagrams` now label-checks the diagram side too and enforces a style guide.
- **Doc accuracy fixes.** The default-barrier claim now states the CC1 fallback on
  runsc-less hosts; ENV.md documents the real (empty) admin-token default and the
  no-auth local-mode consequence; the compose README's setup-menu note was inverted;
  example TASK.md commands run as written.
- **Build plumbing.** `make release-check` is now a strict superset of `make ci`;
  `make help` is self-documenting; the nightly uploads real e2e evidence; screenshots
  are freshness-gated and the README hero shot is script-captured.

## [0.4.2] — 2026-07-20

A follow-up to 0.4.1 from the same adopter, now running the containerized stack on a
corporate laptop end to end. The full report, open gaps included, is in
[docs/adoption/](docs/adoption/corp-network-onboarding-findings.md). One entry below is
a regression 0.4.1 introduced; two were bugs that had been silent for longer.

### Fixed

- **An unreachable corporate proxy no longer hangs an approved request.** The upstream
  `CONNECT` handshake had no read deadline, so a proxy that accepted the TCP connection
  and never answered left an *approved* egress sitting forever with nothing to act on —
  the reported "200 then hang" on hosts whose connectivity client binds to loopback
  only. The handshake is bounded; a stalled upstream is now a normal dial failure
  (deny + 502, logged).
- **The `make setup` UI fallback no longer reinstalls.** 0.4.1's host-build retry went
  through `make ui`, whose reinstall fetches platform-specific native binaries
  (`@tailwindcss/oxide-*`, `@esbuild/*`) that a partial mirror also refuses — so the
  fallback died with `node_modules` already complete. It now rebuilds from an existing
  tree when the lockfile matches. Ceiling: a *fresh clone* has no `node_modules`, so
  this fixes the second and later `make setup`, not a cold start.
- **The New Run wizard no longer claims you have no model access when you do.** With an
  operator-configured Bedrock model, the Access step still warned that the run's first
  model call would 404 while dispatch was going to supply model access automatically.
  The preflight now asks the same resolver dispatch uses; when the preflight is
  unavailable the old local check still applies, so a real gap is never hidden.

### Added

- **`wardyn setup proxy-relay <listen-port> <proxy-port>`** forwards a reachable port to
  a forward proxy bound to `127.0.0.1`, which no container can reach. Foreground and
  unsupervised on purpose (Wardyn owns no host daemons); fails immediately if nothing is
  listening. See
  [docs/adoption/loopback-only-forward-proxy.md](docs/adoption/loopback-only-forward-proxy.md).
- **The Host proxy step warns when a detected proxy is loopback-bound**, naming the
  symptom and the fix rather than leaving it to the first launch. Also in
  `wardyn setup status`.
- **`wardyn site-config get|apply`.** The corporate baseline (upstream-proxy ref,
  artifact mirrors, SCM hosts) lives in Postgres, so `make reset` took it with the
  volume and the stack came back healthy with egress silently unconfigured. The baseline
  is now a file you can keep, carrying secret *names* only; `reset-all` names what it is
  about to destroy and prints the capture command first.

### Changed

- **Writable workspaces name the VM-backed-host caveat.** A `writable: true` host mount
  was reported read-only under the Wall tier on macOS; on a native Linux host runc,
  gVisor, Kata and libkrun all wrote successfully, so the cause is the macOS→VM
  file-sharing layer, not the barrier. The Workspaces banner says exactly that, scoped
  to VM-backed runtimes, instead of claiming a tier "cannot write". The measured
  tier↔writability matrix is in the adoption doc.
- **The agent registers its workspace as a git `safe.directory`** — a host-owned
  bind-mounted checkout no longer fails every git command with "dubious ownership",
  which read like a Wardyn defect rather than a uid mismatch.

## [0.4.1] — 2026-07-20

A corporate-network onboarding fix release from two adopter field reports (kept in
[docs/adoption/](docs/adoption/)). Both are onboarding-trust bugs on the containerized
default path: one hard-blocked `make setup`, the other made the Getting-Started
checklist assert something it never checked. No interface changes.

### Fixed

- **`make setup` no longer dies on a registry that can't serve `pnpm`.** Behind a
  corporate allowlist mirror the image's default `ui-build` stage failed with a raw
  `npm error code E404` (or 403) and the stack never came up — the one command the docs
  tell a new user to run did not work. `scripts/up.sh` now catches the failed build and
  retries with the UI built on your host (`make ui` + `WARDYN_UI_STAGE=ui-prebuilt`).
  An explicit `WARDYN_UI_STAGE` is still honored and disables the fallback, and a
  working registry never enters the retry branch. Probing the registry first does not
  work: the 404 is on the *tarball* path, which a metadata-proxying mirror answers 200.
- **Staging a corporate CA no longer breaks the image build.** `ui-build` ran
  `update-ca-certificates`, which its `node:*-bookworm-slim` base purges — so any
  operator who followed `make doctor`'s own advice and staged
  `deploy/images/corp-ca.pem` hit a hard `exit 127`, every time. The stage relies on
  `NODE_EXTRA_CA_CERTS` alone now (npm/pnpm are its only TLS clients), and
  `deploy/images/README.md`'s snippet — which produced the bug — was corrected.
- **The Host proxy step tells the truth on the containerized stack.**
  `DetectHostProxy()` runs *in* the wardynd process, and in a distroless container every
  tier is structurally blind (container-only env, no `HOME`, no `git`, and the OS/PAC
  tier dispatches on the *process's* `GOOS`) — so it reported "no host-side proxy
  detected" on hosts unambiguously behind one. `make setup` now runs the same detector
  **on the host** (new `wardyn setup detect-proxy`, from a host-native binary the image
  cross-compiles) and seeds the result in, re-running on every `up` so it cannot go
  stale. Deliberately **not** done by forwarding `HTTP_PROXY` into wardynd's runtime
  environment: Go's `net/http` honors those names process-wide, which would silently
  reroute wardynd's own OIDC discovery, audit webhooks, GitHub App minting and AWS
  credential chain. A run's egress is unaffected either way.
- **An honest empty result when detection genuinely can't look** — the step now says
  detection ran inside the container, names what it therefore could not read, and
  carries a next step. The static lede that rendered above an empty result is gone.
- **A set-but-empty `HTTP_PROXY` no longer counts as a detected proxy.** `os.LookupEnv`
  reports ok for `export HTTP_PROXY=`; empty and whitespace-only values are now filtered
  in the one place all tiers route through.

## [0.4.0] — 2026-07-19

Wardyn remains **pre-alpha**: interfaces are not stable and this release changes several
defaults. Read "Upgrading from 0.3.1" below before pulling it onto a 0.3.1 host.

### Added

- **Container as an execution environment.** A workspace can be a container image (new
  `container` kind), and any workspace/container can carry an operator-owned
  model/harness credential binding (`none|managed|api_key|bedrock` — names and refs, never
  values). A run inherits the binding of the workspace it picks; it is folded into the run
  policy at create and injected proxy-side. `PUT /workspaces/{id}/llm-cred`, migration
  `0024_workspace_llm_cred`.
- **YAML policies.** `wardyn run --policy-file` and `wardyn policy create|update -f` accept
  JSON or YAML; `wardyn policy render -f <file>` converts and strictly validates offline.
  Commented examples in `examples/policies/`.
- **`wardyn subscription connect|status|disconnect`.** Capture a `claude setup-token` from
  **stdin only**, stored age-encrypted and injected proxy-side; the sandbox holds an inert
  sentinel. Idempotent (`--reconnect` to replace). `WARDYN_SUBSCRIPTION_TOKEN` seeds it
  headlessly. The setup-token is long-lived (~1 year) and non-rotating — documented, not
  hidden.
- **`wardyn setup status`** — the console's readiness checklist in the terminal, each unmet
  check naming the exact next command.
- **AI Run Composer on the container path** — the real `claude` in a governed one-shot
  sandbox with the managed subscription injected proxy-side, fail-closed when no
  subscription is connected.
- **Containerized AWS SSO login for Bedrock.** A device-code login for SSO-only orgs: no
  host `aws sso login`, no `~/.aws` mount. `deploy/images/aws-sso` keeps ~600 MB of AWS CLI
  out of normal runs. **Honest bound:** the SSO token and derived role credentials are
  **resident** in the sandbox (amber chip in the UI) and Wardyn cannot revoke a captured
  SSO session. Validated against a fake sso-oidc/portal built from real botocore models,
  not live IAM Identity Center.
- **Standard AWS environment is honored** as a fallback — `WARDYN_BEDROCK_REGION` /
  `_AWS_PROFILE` fall back to `AWS_REGION` / `AWS_DEFAULT_REGION` / `AWS_PROFILE`. Region
  alone cannot enable Bedrock, so this cannot switch the transport on by surprise.
- **Corporate-network image builds.** Corp-CA staging and `NPM_REGISTRY` /
  `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY` thread through every build; `UI_STAGE`
  (`ui-prebuilt` via `WARDYN_UI_STAGE`) consumes a host-built `ui/dist`. **corepack does
  not work around a mirror missing pnpm** — it fetches the same 404ing path.
- **Opt-in native agent-CLI install for corp networks** (npm stays the default):
  `CLAUDE_INSTALL=native` is checksum-verified against `downloads.claude.ai` (the only host
  it contacts) or a host-staged binary; `CODEX_INSTALL=native` is staged-only, because
  codex has no Wardyn-verified download contract.
- **Shared-host concurrency for the compose control plane.** Every explicitly named compose
  object is parameterized off `WARDYN_NS` (default `wardyn`, so the single-user default is
  byte-for-byte unchanged), and `ci-run.sh` takes a per-job project name, ephemeral ports
  and a project-scoped `down --volumes`. `make test-e2e-concurrent` is the live two-job
  acceptance test. Boundary: safe for **one trusted operator**, not for mutually
  distrusting tenants.
- **Fail-closed resource caps.** The daemon's `ContainerCreate` warnings are authoritative:
  any "…Limitation discarded" refuses the run. `WARDYN_ALLOW_UNENFORCEABLE_CAPS=1`
  overrides on a trusted host.
- **Run type in the New Run wizard** — "Agent run" vs "Governed command"
  (`task_mode=exec`, no agent, no LLM credentials), and bring-your-own base image promoted
  out of Advanced.
- **Harness-aware demo.** A fifth demo ("The agent in the box") appears on `/demos` once a
  model provider is connected. The four keyless egress-boundary demos stay LLM-free.
- **Enterprise onboarding and a corp-aware `doctor`.** The wizard exposes the Bedrock
  credentials the backend already read (never-resident `bedrock-api-key` first); `up.sh`
  wires operator Bedrock config into `deploy/compose/.env`; Rancher Desktop is detected and
  its non-bind-mountable socket remapped; `doctor` warns on a proxy with no corp CA staged
  and asserts the chosen docker socket is bind-mountable, not merely reachable.
- **Rootless and Podman: documented and probed.** Rootless Docker/Podman is supported at
  **CC1 only**; CC2/CC3 are refused fail-closed. `scripts/test-podman.sh` passes against a
  live rootless Podman 4.9.3.
- **Never-resident Azure DevOps git egress: a reviewed design only, not built.** The
  working lane is still the resident `git_pat` grant; the ceiling (ADO has no
  token-minting API, so the operator PAT's scope is the boundary) is in
  [ROADMAP.md](ROADMAP.md#named-gaps-without-a-milestone).

### Changed

- **`make setup` is containerized by default.** Host mode is an advanced escape hatch
  (`WARDYN_SETUP_MODE=local`); `WARDYN_SETUP_MODE=team` prints a notice and exits.
- **First-run setup is a mandatory gate** for a *new* console: every route except `/setup`
  and `/demos` redirects until it completes, and the side doors are gone. Team/SSO
  deployments are never gated.
- **The model/harness provider is optional.** Only the sandbox barrier is required; the
  `llm_provider` check is INFO and the step is skippable. Agent runs and the Composer need a
  provider; a governed command, a BYO container and an interactive run do not.
- **The model step asks about the harness first**, then that harness's credential path, each
  named for the credential rather than the mechanism, and each keeping its posture chip
  (green proxy-injected, amber resident).
- **The setup funnel is ordered by prerequisite, not by theme** — Host Proxy and Artifact
  Redirect move into Essentials ahead of the model step, and Credentials precedes
  Workspaces.
- **The AI Run Composer is marked Beta**, and its "Proposed setup" review leads with what
  blocks you (rationale and `model_notes` collapse behind disclosures).
- **Setup persists the chosen Docker socket** into `deploy/compose/.env`. It was set in the
  environment only, so any `docker compose up -d wardynd` outside `up.sh` drove compose's
  default socket — on a dual-daemon box, silently collapsing the barrier to Fence-only.
- **Operator-supplied egress domains are validated server-side.** A mid-label wildcard like
  `oidc.*.amazonaws.com` compiles to a hostname no request can equal; one predicate,
  `ValidDomainEntry`, now runs at the `validatePolicySpec` chokepoint every ingest path uses.
  **This can reject a policy 0.3.1 accepted — but only entries that never matched anything.**
- **A half-specified per-workspace Bedrock override is rejected at write time** (a model id
  is region-scoped, so region-without-model fails at invoke). Omitting the block still
  inherits everything.
- **The resident-secret disclosure is corrected.** The threat model claimed "two named,
  bounded exceptions" in three places that disagreed on which two; reading the code there
  are **eight**, including this release's AWS SSO token. §5.1a is now the single
  authoritative enumeration.
- **The compose banner's "production path is Kubernetes" claim is corrected**: the
  Kubernetes data plane is v0.5-planned and cannot create sandboxes yet.
- **CLI list output prints ids in full** — `run list` printed 8-character ids that
  `run kill` then rejected; same for approvals and policies.
- Personal paths and usernames are scrubbed from the tree, and the repo gates are back in
  truth with it (diagram manifest re-pointed after the `runs.go` split, `workspaces.tsx`
  split on two single-concern seams instead of allowlisted, image-pin gate resolving a bare
  `${VAR}` against the Dockerfile's own `ARG` default).

### Fixed

- **A workspace's model credential binding never reached the run's persisted grants — and
  the operator's Claude subscription was billed for it.** `persistRunGrants` snapshotted
  the spec before `applyWorkspaceCreds` mutated it, so a workspace bound to its own
  `api_key` fell through to the managed subscription and `managed`/`bedrock` could not
  displace a competing api-key grant. Credential resolution now runs **before** grants are
  persisted. Visible consequence: `api.anthropic.com` is contributed by the binding, so it
  no longer appears in `run.workspace.egress` `added_domains` (the sets dedupe — the
  effective policy is unchanged).
- **Per-workspace Bedrock bindings are actually applied** — region/model/profile thread
  through `resolveBedrockAuth`, the region's hosts join the run's egress, and the SSO region
  falls back to the *effective* region. Not claimed, because it needs live Bedrock:
  cross-region inference-profile resolution and the bearer-mode exchange.
- **The AWS SSO login pre-allowed three hosts the proxy could never match**
  (`oidc.*.amazonaws.com` and friends are mid-label wildcards), so login failed on the very
  hosts it claimed to allow and stored an empty account/role. Regional hosts are now derived
  from the effective SSO region; with none configured they surface as first-use approvals,
  deliberately not falling back to `*.amazonaws.com`. Net egress is narrower than 0.3.1.
- **The AWS SSO login sandbox had no `~/.aws` at all** while the pane auto-typed a command
  needing `sso_start_url`/`sso_region`. The pane now collects the org portal URL and the
  server seeds an all-or-nothing session block; a missing/non-https/whitespace-bearing start
  URL or a missing SSO region is refused with 400.
- **The `aws-sso` image was offered by the setup UI but built by neither setup path** —
  first use failed at pull against a `:local` tag. Both build loops now build it.
- **The first-run gate could lock an existing console out on a transient daemon blip.** It
  keyed on `!has_runs || !ready`; it now keys on the console being new. (0.3.1's claim that
  returning consoles are never gated was not true.)
- **The managed Claude subscription never actually worked for runs** —
  `detect_anthropic_mode` looked only under `~/.claude`, so a subscription materialized into
  `CLAUDE_CONFIG_DIR` fell through to apikey mode and surfaced "401 Invalid bearer token".
  `CLAUDE_CONFIG_DIR` is checked first now.
- **The composer's model-access verdict was gated on the wrong toggle** — the per-run "use
  subscription" opt-in gates the credential *mount*, which a managed subscription does not
  need. Mount gating and verdict are now separate.
- **The New Run wizard silently dropped bring-your-own-image and `task_mode`** — neither was
  forwarded onto the wire, so BYOI was lost end to end from the UI.
- **The subscription login URL arrived truncated** — `claude setup-token` hard-wraps its
  OAuth URL at the PTY width; the login PTY is forced to 512 columns.
- **The composer review printed two sentences twice** (the model-access line and, on the
  blocked path, the top risk rationale).
- **The resource-cap gate ran after `ContainerStart`, and its probe false-positived on
  Podman.** An untrusted container on an uncapped host ran for the duration of the check
  before rollback; the gate now sits between create and start. The probe trusted `docker
  info`'s `MemoryLimit`/`PidsLimit`/`CPUCfsQuota` booleans, which rootless Podman 4.9.3
  under-reports — those are now only an advisory `doctor` hint.
- `examples/policies/sandbox-claude.yaml`'s `github_token` grant had `repos: []`, which the
  git-broker rejects. Docker-tagged AWS SSO tests no longer burn a 30-second timeout each
  against an image nothing builds.

### Security

- **The dex host port was published on `0.0.0.0`.** It is now loopback-only and
  parameterized (`WARDYN_DEX_PORT`); dex only runs under the `sso` compose profile.
- Server-side egress-domain validation (see Changed) closes a defect class where an
  operator-authored allowlist entry could be silently dead. Adversarial review caught the
  first version of the predicate failing open in exactly the way it was written to prevent.
- `pkg/client`'s `HarnessLogin` method is deleted — zero callers, and both `client.go` and
  `docs/sdk.md` already listed harness-login as **not** SDK-covered.

### Upgrading from 0.3.1

- **`make setup` now brings up the containerized stack.** For host mode, pass
  `WARDYN_SETUP_MODE=local` explicitly.
- **Re-run setup on an existing compose deployment** (or hand-edit `deploy/compose/.env`):
  `WARDYN_DOCKER_SOCK` is now persisted there. With more than one Docker daemon, skipping
  this silently collapses the barrier to Fence-only.
- **A host that cannot enforce resource caps will now refuse to launch runs**
  (`resource caps not enforceable on this host`). Fix the host, or set
  `WARDYN_ALLOW_UNENFORCEABLE_CAPS=1` to get 0.3.1's uncapped behavior back.
- **A fresh console cannot be skipped past Getting Started.** Automation that drove a new
  local-mode console straight to `/runs` must complete setup or seed the completion flag.
- **Check your policies for mid-label wildcards** — such an entry never matched any
  request, so rewriting it changes what your policy *does*, not just whether it saves.
- **Check any per-workspace Bedrock binding** — half-specified overrides are rejected on
  write, and a complete one is now actually applied at dispatch.
- **Verify which runs your workspace credential bindings bill** — an `api_key` binding was
  previously ignored and billed to the managed subscription.
- Rebuild your agent images (`make agent-images-core` or `scripts/up.sh`) — the `aws-sso`
  image is new and is pulled by tag from no registry.
