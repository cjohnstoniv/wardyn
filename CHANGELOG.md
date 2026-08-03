# Changelog

All notable changes to Wardyn are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); Wardyn is **pre-alpha**
and does not yet follow semantic versioning (interfaces are not stable).

**v0.3.1 and older live in [CHANGELOG-ARCHIVE.md](CHANGELOG-ARCHIVE.md).**

## [Unreleased]

### Added

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

### Changed

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
- **The workspace Requirements step is four tabs** — Record · Egress · Secrets
  · Files & services — matching the approved design. Record leads, because the
  honest answer to "what does this workspace need?" is usually "drive it once
  and find out."
- The Integrations page's **Artifact mirror** category is now **Egress
  redirection**, with the same compact `from → to` rows, the `network only`
  chip, and per-row Test as the Corporate network step.

### Fixed

- **A failed request is no longer reported as a `Blocked` probe verdict.** Both
  Test buttons caught every thrown error and rendered it as the state meaning
  "the network would not let this through", so a 403 from lacking the operator
  role, or a wardynd that restarted mid-click, told the operator their proxy
  was blocking them and sent them to debug a working firewall. That inverts the
  reason these buttons are permitted at all. Failed requests now surface as
  themselves and the control returns to Not tested.
- **The Integrations page showed nothing for a proxy or redirect saved through
  the new Corporate network step** — its row derivation still read only the
  legacy `artifact_overrides` map and the secret-ref-only proxy field, so the
  two surfaces disagreed about the same stored config.
- **The Add-integration dialog's registry redirect could not save twice.** Its
  step body still wrote the deprecated `artifact_overrides` map while sending
  the whole document back, and the server refuses a body that sets both shapes
  rather than guessing which one wins — so the second save always 400'd, citing
  a field the operator never typed. It writes `egress_redirects` now.

## [0.4.5] — 2026-08-03

### Added

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
  providers, git hosts, artifact mirrors, the corporate proxy. Rows are
  DERIVED from what already exists (stored secret names, site config, setup
  status), so an operator who never opens the page keeps identical behavior and
  one who does can adopt a row to edit it. Each row states what it powers,
  where its credential lives at run time, and — for capabilities that are
  impossible rather than unconfigured — why, as a fact with no control beside
  it. A **Tools** tab names the other half: a tool is what the image carries,
  an integration is what it connects through.
- **Model access resolves** instead of being configured per run: an explicit
  integration on the run, else the workspace's binding, else the operator's
  site-wide default, else nothing (unchanged honest path). `PUT`/`DELETE
  /integrations/{id}` and `POST /integrations/{id}/adopt` manage stored rows;
  the composer registry derives from the integration marked for Wardyn's own
  features, with `WARDYN_COMPOSER_CONFIG` still winning outright when set.
- **A single Add-workspace wizard** — Sources → Base image → Requirements →
  Done — replacing three inconsistent entry points and the six-step import
  dialog. Custom image builds accept Dockerfile steps, and a credential-shaped
  line warns (naming the line and detector, never the value) without blocking.
- **A workspace detail page** at `/workspaces/:id`: the requirements editor,
  the candidates Wardyn noticed but hasn't given to any run, recorded sessions
  and their confined replays, and env-as-code.

### Changed

- **Getting started is nine steps, not thirteen.** The model-provider, host-
  proxy, SCM-provider, artifact-registry and credentials steps were five rail
  entries for one activity; they are now one optional Integrations step. The
  dependency their ORDER used to encode — you cannot reach a model provider
  through an unconfigured corporate proxy — is now a banner that says so.
  Readiness reads the same derived rows the Integrations page renders, so the
  funnel and that page cannot disagree.
- Workspace status collapsed to `pending_scan → scanning → scanned | error`
  and reads as one word in the console: Setting up / Usable / Scan failed.
- The run's Access step no longer asks how to authenticate; it shows what
  resolved and why, with a per-run override.

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

### Fixed

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

### Added

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
  Dispatch subtracts the broker-managed GitHub hosts (`github.com`,
  `api.github.com`, `codeload.github.com`, `*.githubusercontent.com`) from the
  egress allowlist of any run with git grants **and denies them** — deny beats
  `allow_all_egress` too — and `wardyn-git-helper` no longer mints a GitHub App
  token into a brokered sandbox at all (a GitHub host is refused whenever
  `WARDYN_GIT_BROKER_REPOS` is non-empty; a grant covering no repo is not
  brokered, and mints nothing either way — `MintInstallationToken` refuses an
  empty repo list). Previously the direct lane was closed only by
  shipped-policy convention: no git host is ever TLS-MITM'd, so a
  `github.com:443` CONNECT is an opaque tunnel the ref parser cannot inspect,
  and the helper printed a live token to stdout inside the sandbox. A run with
  **no** git grants is unaffected.
  Honest scope: this binds the brokered App lane — a `git_pat` push (opaque
  CONNECT) cannot be bound by a receive-pack parser and remains bounded by the
  operator who supplied the credential. `ssh_key` and `git_pat` used to carry
  that same honest-scope exception for the SAME forge; neither does now — see
  the single-lane entry below.
- **A brokered forge is now single-lane: neither `ssh_key` nor `git_pat` can
  ride beside a `github_token` grant for it.** Closes the gap the entry above
  used to carve out. Three seams: `validateGrantLaneExclusivity`
  (`internal/api/policy.go`) refuses a policy that declares both a
  `github_token` grant and an `ssh_key` or `git_pat` grant for the same forge
  (`400`, on every policy write — stored, inline, `WARDYN_DEFAULT_POLICY`, and
  the composer/profile clamps); `confineGitBrokerEgress`
  (`internal/api/runs_dispatch_gitbroker.go`) also denies that forge's
  `ssh.<forge>` SSH-over-443 endpoint — spelled as the bare host, so the deny
  covers every port, not just 443 — alongside the four managed HTTPS hosts, on
  every brokered run regardless of which grants it holds; and
  `handleInternalMint` refuses either kind for a brokered forge before opening
  the broker transaction. For a policy stored before the rule shipped,
  `dropBrokeredGrants` withholds that forge's `ssh_key` **and** `git_pat`
  grants from the sandbox env at dispatch entirely, so the credential is never
  minted (not merely denied a route), each with a `slog` warning and a
  `run.ssh.brokered_forge` / `run.git_pat.brokered_forge` audit event. An
  `ssh_key` or `git_pat` grant for a *different* host (ADO, GitLab, GHES — the
  lane `git_pat` exists for) is untouched by any of it. The `git_pat` half
  corrects a stated justification that did not hold: a brokered `git_pat` was
  called "already dead twice over", counting `wardyn-git-helper`'s in-sandbox
  refusal as one death — but that only binds a caller that asks *git* for the
  credential, and the proxy's own mint refusal (`isBrokeredGitGrant`) matches
  `github_token` grant ids only, so a direct POST to the mint route was
  answered with the PAT. That left one barrier, a name-keyed deny; and a GitHub
  `git_pat` is typically a *user* PAT, broader than the repo-scoped
  installation token beside it. The `ssh_key` half reverses an earlier decision
  of this same doc-reconciliation effort, on the owner's call — see
  `confineGitBrokerEgress` for why. Same standing caveat as the four HTTPS
  denies: it is a NAME deny, so a raw-IP CONNECT reaches `allow` under
  `allow_all_egress` (measured) — see `docs/POLICIES.md`.
- **Token-side confinement: GitHub ref-ruleset verification, plus an opt-in
  mint gate.** `VerifyRefRuleset` (`internal/broker/ruleset.go`) asks GitHub
  which rules are in force on a repo outside and inside the run's push
  namespace — `creation`+`update`+`deletion` required outside, neither inside
  — and reads each backing ruleset's `current_user_can_bypass`, requiring
  `"never"`, so an App exempted via bypass still grades unconfined. A
  `github_ref_ruleset` setup-checklist row grades the first repo a policy
  names (never `fail`, only `warn`/unknown, and cached so the wizard's polling
  cannot turn into a rate-limit), and `WARDYN_GITHUB_REQUIRE_REF_RULESET`
  (opt-in, default off) turns the same check into a pre-mint gate that refuses
  a `github_token` mint when the repo is unconfined or unverifiable. Branches
  only — a `target: "branch"` ruleset leaves `refs/tags/*` open — and classic
  branch protection does not surface in the endpoint this reads, so a repo
  protected that way still grades unconfined. The recipe to create the
  ruleset is in `docs/POLICIES.md` ("Bound the token itself: a GitHub
  ruleset"); Wardyn never creates it (needs repo-admin it deliberately does
  not request).
- **wardynd images are published to GHCR** (`.github/workflows/publish-image.yml`:
  main pushes → `:latest` + `:sha-<7>`, `vX.Y.Z` tags → the bare semver the Helm
  chart's default resolves to; `workflow_dispatch` `extra_tag` backfills
  pre-workflow releases), and a **kind-based `helm install` CI gate**
  (`helm-install-test`) proves the chart converges to a healthy control plane on
  every PR — not just that it renders. The image now defaults
  `WARDYN_DEFAULT_POLICY=/examples/policies/default.json`, fixing the crash-loop
  every default `docker run`/Helm install previously hit (the Go-relative
  default never resolved from the distroless WorkingDir).

- **`WARDYN_OIDC_OPERATOR_EMAILS` — a minimal viewer/operator gate** (flag
  `-oidc-operator-emails`), the first authorization tier on the control plane,
  now covering 25 routes. List the operators and every other signed-in human
  becomes a **viewer**: reads everything and can launch/kill runs, but is
  403'd on configuring the deployment (managed harness credential, policies,
  workspaces, `PUT /site-config`), writing/deleting secrets, deciding an
  approval, and attaching to a running sandbox — both the ticket mint and the
  WebSocket itself, since the socket falls back to session-cookie auth when no
  ticket is presented. Additive — unset (the default) keeps
  today's behavior exactly, and the admin token and local mode are always
  operators (one shared credential, no human to key a role off). Configuring
  OIDC SSO with the operator list left empty now **refuses to boot** (flag
  `-allow-oidc-no-operator-list` / `WARDYN_ALLOW_OIDC_NO_OPERATOR_LIST`
  overrides, for a deployment that really does want every signed-in human
  admin-equivalent). This is one allowlist, not RBAC: run create/kill stay
  open to any signed-in human by design
  (`ROADMAP.md`, `threatmodel/THREAT-MODEL.md` residual #14).
- **Three per-process defects that used to make `replicas > 1` unsafe are now
  closed at the code level** — not because multi-replica is a supported
  configuration today (no shipped topology runs more than one: compose pins
  `container_name`, the chart pins `replicas: 1`), but because a future build
  no longer needs three separate durability projects to get there. Session
  recordings default to a Postgres-backed store (migration 0028,
  `internal/recording/pgstore.go`) visible to every replica;
  `WARDYN_RECORDING_STORE=fs` still selects the legacy per-pod directory, and
  the real "fully off" recipe is now two variables
  (`WARDYN_RECORDING_STORE=fs` **and** `WARDYN_RECORDING_DIR=""`). Note that
  this is the BINARY's default: the Helm chart deliberately pins `fs`, because
  it is single-replica by policy and `pg` retains every cast forever by
  default — so a `helm install` does NOT get the Postgres store unless you set
  `env.WARDYN_RECORDING_STORE=pg`. Compose does get it. Run-watcher
  adoption is a Postgres lease plus a periodic cross-replica sweep (migration
  0027, `internal/api/reconcile.go`): a run orphaned by a pod that never comes
  back is adopted by any live replica within roughly 65-150s instead of
  stranding forever. The ground-truth token rotator is leader-elected via a
  Postgres advisory lock (`cmd/wardynd/gt_rotator.go`), with a standby taking
  over within one ~30s backoff of the leader's session ending. See
  `docs/OPERATIONS.md` ("One replica, by construction") for the full
  mechanism and what remains per-process by design.

### Fixed

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

### Changed

- **Plaintext HTTP on a specific non-loopback bind is now refused at boot**
  (was: warn-only). Loopback and unspecified binds (compose/`make setup`)
  are unaffected. Migration for TLS-terminating-proxy deploys on a specific
  IP: set `WARDYN_TLS_TERMINATED=true` (or `WARDYN_ALLOW_PLAINTEXT_LISTEN=true`
  to keep the old behavior explicitly).

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
