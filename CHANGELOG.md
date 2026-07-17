# Changelog

All notable changes to Wardyn are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); Wardyn is **pre-alpha**
and does not yet follow semantic versioning (interfaces are not stable).

## [Unreleased]

### Added

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

### Changed

- Bad `WARDYN_*` environment values (e.g. `WARDYN_ENVBUILD=treu`) now fail loud
  (exit 2, naming the variable and value) instead of silently falling back to
  the default.

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

### Security

- **NAT64-smuggled metadata targets are now blocked** in the composer's egress
  transport (a `64:ff9b::a9fe:a9fe`-style literal previously reached
  `169.254.169.254` past the SSRF guard).
- Base images are now digest-pinned across Dockerfiles, `go-github` is
  collapsed to a single v88 major, and `GO-2026-5932` (unmaintained
  `x/crypto/openpgp`, reached only via `filippo.io/age`, no called path) is
  documented in `SECURITY.md`.

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
