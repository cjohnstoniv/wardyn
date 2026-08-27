# Changelog

All notable changes to Wardyn are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); Wardyn is **pre-alpha**
and does not yet follow semantic versioning (interfaces are not stable).

**v0.3.1 and older live in [CHANGELOG-ARCHIVE.md](CHANGELOG-ARCHIVE.md).**

## [Unreleased]

## [0.6.6] — 2026-08-28

A follow-up to 0.6.5 for the `k8s` runner: on a cluster where the runs namespace cannot reach the
control plane, the setup connectivity probe could never pass, and nothing on the console said why.
Reported by the same enterprise adopter on managed Kubernetes after verifying every 0.6.5 item on
their deployment.

### Fixed

- **The connectivity probe timed out before an exec-mode run could finish.** Every exec is
  wrapped by `wardyn-rec`, which uploads the session cast to the control plane through the
  proxy after the task exits. That upload waited up to 60s when the control plane was
  unreachable, the probe waited only 50s, so on such a cluster the probe killed its own run at
  +50s every time — reported as "did not finish within 50s" under the proxy heading, with the
  task long since done. The recorder's upload is now bounded (5s connect, 20s total; delivery
  stays non-fatal), the probe's budget is 90s (runner backstop 120s), and a run that started but
  never reported back is its own `timed_out` verdict, whose detail carries the sandbox agent's
  state at the deadline and points at `WARDYN_CONTROL_PLANE_URL`. The recorder bound ships
  inside the agent images: rebuild any image pinned through `WARDYN_AGENT_IMAGES` from 0.6.6 (or
  pull the published `agent-base:0.6.6`) — a pre-0.6.6 image keeps the 60s upload tail, which
  the 90s budget covers only for a fast task.
- **A k8s exec container that never starts no longer hangs the run.** `Wait` polled a `Waiting`
  ephemeral container forever; a hard start failure (`CreateContainerConfigError`,
  `ErrImagePull`, …) now fails the run with `exec_started: false` in its `run.complete` event,
  the probe reports it as `not_run` naming the reason, and `AgentStatus` surfaces the waiting
  reason.
- **The probe run is labelled `base`**, the image key it actually dispatches, instead of
  `claude-code`; the "Agent image toolchains" setup row names both the claude-code harness
  image and the probe's `base` image.

### Added

- **`warning` on a passing probe when its recording never landed.** Egress can work while the
  proxy pod cannot reach the control plane — every run then completes and silently loses its
  session recording. The probe now checks that its own cast arrived and, if not, says so with
  the `WARDYN_CONTROL_PLANE_URL` to check.
- **Console Ingress read timeout guidance.** The probe is one HTTP request of up to 90s;
  ingress-nginx's default 60s `proxy-read-timeout` turns the verdict into a 504. The chart's
  values and README show the annotation to set.

## [0.6.5] — 2026-08-28

A patch for the `k8s` runner on managed, multi-tenant Kubernetes — where a platform team owns RBAC
and namespaces, a baseline default-deny NetworkPolicy already exists, the Postgres DSN Secret is
operator-owned, and packages come from an allowlist mirror. Reported by an enterprise adopter on
managed Kubernetes; every item below was verified against the code before it was fixed.

### Security

- **`golang.org/x/crypto` 0.54.0 → 0.55.0 (GO-2026-6303).** The SSH gateway's
  `ssh.NewServerConn` reaches the code path where the source-address critical
  option was not enforced for non-public-key auth callbacks; `govulncheck`
  reports it as reachable and would block the merge gate. Fixed upstream in
  0.55.0; `x/net` and `x/text` move with it as indirect dependencies.
- **The chart's NetworkPolicy governs the control-plane pod only.** Its `podSelector` matched
  `app.kubernetes.io/name` + `instance` — the two labels the shared helper puts on every pod the
  release creates, and that sandbox pods can carry through caller-supplied labels. Any pod in the
  release namespace carrying those two labels inherited the control plane's ingress rules (every non-http inbound port
  denied) and, because NetworkPolicy allows are additive, its egress allowances. The selector now
  also pins `app.kubernetes.io/component: control-plane`, which the pod template already carried; the
  Deployment's own immutable `spec.selector` is untouched, so upgrades apply cleanly.

### Added

- **`k8s.rbac.create`** (default `true`). `false` renders no Role/RoleBinding/ClusterRole/
  ClusterRoleBinding and nothing in `k8s.runsNamespace`, for a platform that provisions runner RBAC
  out of band and refuses cluster-scoped objects from tenants. The `serviceAccount.create=false`
  without a name refusal stays either way.
- **`ingress.*`** — an optional Ingress for the console's `http` port (class, annotations, hosts,
  TLS). Off by default; the render is unchanged until enabled. The UI-sandbox gateway keeps its own
  hand-authored Ingress on its own hostname by design.
- **`secrets.ageKeySecretRef`** — the age identity from its own Secret, independent of the DSN
  Secret, for a DSN a managed-Postgres operator owns and no one can add a key to. Naming it alongside
  `ageKeyFromSecret`/`ageKey` is refused at render: two Secrets, one identity, and booting under the
  wrong one is unrecoverable.
- **`defaultPolicy`** — the default RunPolicy as JSON text (`--set-file defaultPolicy=my.json`),
  rendered into a ConfigMap, mounted read-only, with `WARDYN_DEFAULT_POLICY` pointed at it and a
  checksum annotation that rolls the pod on change. Until now the only chart-level choice was one of
  the files baked into the image, whose shipped floor is CC2 — unadvertised on any cluster with no
  `k8s.runtimeClasses` pinned.
- **`WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY=1`** — an acknowledgement, distinct from
  `WARDYN_K8S_ALLOW_UNENFORCED_NETPOL`, for the one canary shape a tenant cannot fix: the baseline
  phase's pod ran and could not reach the API server because the namespace already carries a
  default-deny NetworkPolicy the platform team owns. Boot proceeds, the log says loudly that
  enforcement is acknowledged rather than proven, and the setup page shows it as a `warn` row. A
  canary pod that never started still refuses boot with no override. The refusal message now names
  the acknowledgement next to the exemption it already named.
- **A `confinement_floor` setup row** that warns when the default policy's floor is a class this
  runner does not advertise — every run on the default policy would be refused before launch — and
  names the two remedies (lower the floor via `defaultPolicy`/`WARDYN_DEFAULT_POLICY`, or pin a
  RuntimeClass).
- **A `not_run` verdict for the setup connectivity probe**, distinct from `blocked`: the probe
  sandbox never started (an image pull, a confinement class this host cannot enforce), so nothing was
  learned about the network. The console says so instead of "fix the proxy".
- **OIDC public clients.** `WARDYN_OIDC_CLIENT_SECRET` is optional; without it the token exchange
  runs as a public client (`client_id` in the body, PKCE S256 — which every login already sent).
- **`GOPROXY` build arg** on every Go builder stage, plumbed through `make` and Compose like
  `NPM_REGISTRY`; empty is identical to unset.
- **The release pipeline can be rehearsed.** `release.yml` gains a
  `workflow_dispatch` with `dry_run` (default true): it builds every image, the
  CLI cross-builds and the chart package, runs the UI-lockfile scan and its
  zero-npm assertion, and pushes, signs, attests and uploads **nothing**. The
  SBOM *merge* needs a pushed digest, so a dry run stubs the per-image SBOMs
  and skips it.

  This pipeline could previously only be exercised by tagging, so its bugs were
  unobservable until a real tag pushed — which is why 0.6.2 shipped images with no
  provenance and 0.6.3 shipped an SBOM that understated its own contents. Both
  would have failed a dry run. `make release-check` was green every time, because
  it validates the repository, not the workflow.


### Added

- **The member-mode (m′) desktop envelope now exists.**
  `docs/DESKTOP.md` has documented member mode in full — `WARDYN_MEMBER_MODE`,
  an MDM-injected admin token the developer never reads, four member-mount
  bounds — while the tree contained **zero** matching lines under
  `deploy/desktop/`. The only shipped variant was "SSO instead of local mode",
  which sets OIDC and stops, so the m′ profile rendered as-is produced a member
  who **cannot mount their own project directory** — the one power m′ exists to
  add.

  `deploy/desktop/wardyn.env.m-prime.example` is a second COMPLETE envelope, not
  a commented variant block: `scripts/test-desktop-profile.sh` parses only
  uncommented `^VAR=` lines, so a commented m′ would have been invisible to every
  syntax and `docs/ENV.md` parity assertion — shipping unchecked while the suite
  printed PASS.

  It carries `WARDYN_ADMIN_TOKEN` **only as a pointer to `secret.env`**, never as
  a value: compose falls back to the *published* literal `demo-admin-token`, and
  on a loopback bind that default warns and boots, so an envelope that omits the
  token hands every developer operator rights and m′'s whole invariant is false
  on every device.

### Fixed

- **The setup connectivity probe blamed the proxy for its own failures.** It pulled
  `agent-claude-code` — an image the project deliberately stopped publishing in 0.6.2 — and dispatched
  at the default policy's CC2 floor, so on a stock managed cluster it failed at the image pull or at
  `no confinement substrate can enforce class "CC2"`, and both surfaced as **Blocked** under the
  proxy heading with "fix the proxy above". It now runs the published `agent-base` image (override
  key `base` in `WARDYN_AGENT_IMAGES`) at the strongest class the runner actually advertises — the
  probe tests egress, not the floor — and a sandbox that never started is `not_run`, never `blocked`.
  `agent-base`'s `agent-run` stub honours `WARDYN_TASK_MODE=exec` so it can carry the probe;
  `make setup` builds `wardyn/agent-base:local` on the from-source path and the Compose stack maps
  the `base` key to it.
- **A failed `run.complete` read as a clean exit.** The probe decoded the failure event's missing
  `exit_code` as `0` and reported `reached` for a run whose watcher had errored.
- **`email_verified` absent is no longer "false".** With `WARDYN_OIDC_EMAIL_DOMAINS` set, an
  id_token with no `email_verified` claim at all — the norm for Entra ID — denied every login with
  the message for a claim the IdP had set to `false`. Absent is its own `email_verified_absent`
  outcome: the operator gets a server-side warning naming the claim, the issuer and the variable; the
  user is told the provider sent no claim and to ask for App Roles instead of "verify your email".
  The sign-in copy also named a variable that does not exist (`WARDYN_OIDC_ALLOWED_EMAIL_DOMAINS`).
- **The setup barrier picker says it is a browser-local default.** Its instruction read as if it
  set the server's floor; it never did (the footnote below it already said so).
- **`NPM_REGISTRY` was bypassed by the npm self-upgrade.** The agent image Dockerfiles ran
  `npm install -g npm@<version>` before `npm config set registry`, so behind a mirror that does not
  proxy the public registry the build failed on its first install. The registry is set first.

Not in this patch: an operator-configurable model-provider base URL (an internal OpenAI-compatible
gateway as a first-class provider) — the supported path today is the EgressRedirect header-injection
lane, documented in `docs/OPERATIONS.md`; a Gateway-API `HTTPRoute` variant of `ingress.*`.
- **`scripts/test-desktop-profile.sh` checked only one envelope.** It read a
  single hardcoded `wardyn.env.example`, so any second variant shipped with no
  syntax check, no ENV.md parity check and no policy-path check. It now loops
  over `wardyn.env*.example` (not `*.env.example`, which does **not** match
  `wardyn.env.m-prime.example`) and asserts the loop ran more than once, so the
  next filename cannot silently reopen the hole. New m′ assertions cover the
  member-root **width** — `/` or a home directory leaves the dotfile deny-list as
  the only thing between a member and the operator's `~/.ssh`, and an
  `.env.example` copied fleet-wide by MDM is exactly where that propagates.

- **The BYOI wrap produced images that could not pass their own contract
  selftest.** `FinalizeBase` COPYed the `wardyn-git-helper` binary onto `PATH`
  but wired nothing to it, so git never called it. Any run whose policy declares
  a `github_token` eligible grant then failed `agent-run --selftest` with *"a git
  grant is present but the credential helper is not wired — brokered git would
  silently no-op"*. `examples/policies/demo.json` declares exactly that grant and
  is the desktop tier's own managed ceiling, so this broke the whole BYOI lane.

  The wrap now installs a root-owned `/etc/gitconfig` (COPY, never `RUN` — a BYOI
  base may carry no shell and no git, and the stage must stay `FROM`+`COPY` for
  `assertWrapSafeBase`). Its `--secret-file` path is `$HOME`-relative rather than
  the agent images' hardcoded `/home/agent`, because a BYOI base has its own user
  and home while `provision_git_helper_secret` always writes
  `${HOME}/.wardyn/git-helper.secret`; hardcoding it would have left the
  caller-auth gate silently falling open on every BYOI image.

- **The selftest failed closed on a base image with no git at all.** Absent git
  is not an unwired helper — there is no git to no-op — and
  `selftest_check_bins` had already ruled git "not required for this task mode"
  two blocks earlier. Two halves of one selftest disagreeing is what kept the
  `desktop-envelope` CI job red on **every run since it was added** — it has
  never once been green. Both halves are now consistent; where git absence
  genuinely matters (harness mode, or exec mode with repo wiring)
  `selftest_check_bins` still requires it.

- **Both documented install paths were broken.** `install.sh` resolved its
  version from `releases/latest`, which EXCLUDES pre-releases — and RELEASING.md
  mandates `--prerelease` on every Wardyn release, so that endpoint returned
  HTTP 404 and the installer died on every run. `curl -fsSL …/install.sh | sh`,
  the front door 0.6.3 shipped as its headline feature, did not work.

  The README's two Helm blocks used the same endpoint and failed the **opposite**
  way: the empty result went into `helm install --version ""`, which helm accepts
  as **unpinned** and silently resolves to the newest chart — the exact outcome
  the prose two lines above the block warns about. All three now resolve from
  `releases?per_page=1`, and the Helm blocks guard the empty case on the `helm`
  command itself, where it cannot be pasted past.

  Nothing caught either one: the root `install.sh` had no lint, no `sh -n` and no
  test, though `release.yml` ships it in the cosign-signed `SHA256SUMS`.
  `scripts/test-install-sh.sh` now covers it, in `make test-scripts`.

- **The README handed out an unsigned installer.** It curled `install.sh` from
  `main`, so the cosign-signed copy in the release assets was never the one
  anyone executed. It now points at the pinned `releases/download/` asset;
  RELEASING.md step 1b sweeps that version with the other four.

- **`docs/EXPORT.md` recorded an export-control obligation that does not exist.**
  It listed a BIS/NSA notification as *"PENDING — not yet sent"*, open since
  0.6.2. EAR §742.15(b)(1) places publicly available 5D002 encryption source code
  outside the EAR outright, and BIS's final rule of 29 March 2021 narrowed the
  email notification to §742.15(b)(2) — source code performing **"non-standard
  cryptography"** only. Wardyn implements no cryptographic algorithm of its own
  and modifies none, so it is not triggered. The page now says so, and names the
  condition that would re-open it.


## [0.6.4] — 2026-08-27

### Fixed

- **The `wardynd` SBOM reported zero npm packages, and 0.6.2's notes claimed
  otherwise.** Scanning the pushed image instead of the source tree fixed the
  OS-package blindness and did nothing for this one: `wardynd` embeds the console
  as a Vite bundle, which strips every `package.json`, so there is no manifest in
  the image for any scanner to find however you point it. The released
  `sbom-wardynd.cdx.json` for 0.6.3 carries 1,070 components — 114 Go modules, 4
  Debian packages, and **zero** of the 611 npm packages actually in the bundle.

  The lockfile is the only place those versions still exist, so the release now
  merges a `syft dir:ui` scan into the image SBOM, and **fails the job if the
  result still reports zero npm packages** — the check that would have caught the
  original mistake instead of shipping it.


## [0.6.3] — 2026-08-27

A CI-permission fix for 0.6.2, which shipped its images but not its provenance.

### Added

- **`install.sh` — a one-line install that needs no clone.**
  `curl -fsSL https://raw.githubusercontent.com/cjohnstoniv/wardyn/main/install.sh | sh`
  pulls the signed images, mints this box's secret-store key locally, and starts
  the stack. Docker is the only requirement. `WARDYN_NS` / `WARDYN_PORT` and the
  other port knobs let a second install sit beside an existing one, and the script
  detects a conflicting stack and says so rather than surfacing a raw daemon
  conflict.

  Until now the documented install was `git clone && make setup`. Pulling instead
  of building removed the wait; it did not remove the clone. This does.

- **Standalone `wardyn` CLI binaries** (linux/darwin × amd64/arm64), static and
  covered by the release's signed `SHA256SUMS` — for CI, air-gapped hosts, or
  anyone who wants the client without the stack.

### Changed

- **The README leads with the two paths a user actually takes** — install on this
  machine, or `helm install` onto Kubernetes — and building from source moved to
  `CONTRIBUTING.md` where it belongs. The Helm chart's examples are OCI-first and
  version-pinned; an unpinned `oci://` install silently follows the newest chart.

### Fixed

- **`actions/attest-build-provenance` needs `attestations: write`.** The 0.6.2 tag
  run pushed and cosign-signed all five images, attested their SBOMs, and
  published the Helm chart — then failed on every image at the provenance step
  with "Resource not accessible by integration". `id-token: write` covers cosign's
  OIDC exchange; writing to the repository's attestations API is a separate scope.
  Because `release-assets` depends on `images`, it was skipped, so 0.6.2's Release
  carries none of the SBOMs, notices or signed checksums.

  0.6.2's images are correct and remain published. This release is the same code
  with the workflow permission added — the tag was not rewritten, because moving a
  published tag is worse than spending a patch number.


## [0.6.2] — 2026-08-27

### Security

- **Shared subscription credentials are refused outside a single-user posture.**
  Wardyn injected one operator's live Anthropic OAuth token, proxy-side, from a
  server-global provider with no binding to the human who launched the run. On a
  desktop that is the operator using their own subscription; on a multi-user
  deployment it is that subscription serving other people's runs, which the harness
  vendor's terms prohibit — each end user must authenticate with their own
  credential — putting the **operator** in breach, not Wardyn. A new boot-time
  predicate (`subscriptionInjectPosture`) permits it only when the runner is not
  k8s, no OIDC issuer is configured, and either local mode is on or
  `WARDYN_ALLOW_SHARED_SUBSCRIPTION` waives that last clause for a genuinely
  single-user demo box. Enforced at four depths: the credential providers are not
  constructed at boot, neither dispatch lane authors a grant, the integration
  selector will not mount the operator's `~/.claude`, and the injection sink
  refuses to resolve — the sink being the one place every producer converges, and
  placed ahead of the call that would otherwise rotate the operator's own
  credential file. The Helm chart refuses the three desktop-only variables
  outright.
- **Record Mode no longer writes a shared-subscription grant into a stored
  profile.** `wardyn record save` on a subscription run produced a durable,
  shareable, policy-id-addressable grant on one person's live credential. A
  least-privilege profile carrying that is mis-sold by its own name.
- **`make setup` no longer touches credentials.** Both staging prompts are gone;
  you connect a subscription in the console, which signs in inside a sandbox and
  stores the token age-encrypted instead of copying your `~/.claude`.
  `scripts/stage-claude-creds.sh` survives for demo recordings behind the same
  override. `docs/CI.md`'s subscription section is deleted rather than softened:
  CI is the intermediation case.
- **`wardynd:latest` is signed.** `publish-image.yml` pushes it on every merge to
  main and had no cosign step, so the one image `deploy/desktop/install.sh` pulls
  was the one nobody could verify.

### Added

- **`agent-base`**, a published agent image carrying the full runner contract and
  no coding agent. `agent-claude-code` is no longer published: it bundles a
  proprietary CLI whose own package declares `SEE LICENSE IN README.md` while
  shipping neither that file nor a licence, so a puller cannot read the terms they
  are bound by. It remains a local build recipe (`make agent-images`), which is
  also the honest arrangement — you install that CLI under your own agreement with
  its vendor. Existing `0.5.0`/`0.6.0`/`0.6.1` tags stay published; retracting
  released versions breaks existing pulls.
- **Per-digest SBOMs and build provenance**, cosign-attested, scanned from the
  pushed image rather than the source tree — the old source scan saw no OS
  packages at all, which is where the GPL and the CVEs live. (It does *not* fix
  the npm blindness; see 0.6.4.)
- **Release assets that exist**: the per-image SBOMs, `THIRD-PARTY-NOTICES.md`,
  `LICENSE`, `NOTICE` and a cosign-signed `SHA256SUMS`, with a job that fails if any
  of them did not land. Previous releases carried demo videos or nothing.
- **[`docs/VERIFY.md`](docs/VERIFY.md)** — the consumer-side verification
  procedure, previously present only in a maintainer runbook.
- **[`security/vex/wardyn.openvex.json`](security/vex/wardyn.openvex.json)** —
  GO-2026-5932 as a machine-readable `not_affected` /
  `vulnerable_code_not_present` statement, so downstream scanners stop re-raising a
  finding `govulncheck` already disproves on every push.
- **[`LICENSING.md`](LICENSING.md), [`TRADEMARKS.md`](TRADEMARKS.md),
  [`PROVENANCE.md`](PROVENANCE.md), [`AUTHORS`](AUTHORS),
  [`docs/EXPORT.md`](docs/EXPORT.md)** — the explicit free-for-commercial-use
  grant, the naming policy Apache-2.0 §6 deliberately does not supply, the honest
  account of the squashed root commit and the AI-assistance position, a definition
  for the copyright holder named in 990 file headers, and the 5D002
  self-classification.
- **[`THIRD-PARTY-NOTICES.md`](THIRD-PARTY-NOTICES.md) + `licenses/texts/`** — 83
  Go modules and 90 bundled UI packages with verbatim licence texts, generated and
  CI-verified against drift, and shipped inside every image at
  `/usr/share/doc/wardyn/`.
- **`deploy/images/THIRD-PARTY-GPL.md`** — the GPL/LGPL corresponding-source offer,
  machine-derived from the published images' SBOMs (279 packages across five
  images; even distroless conveys one).

### Removed

- **`make sbom` and CI's `sbom-stub` job.** They scanned the source tree, which
  sees no OS packages and no bundled UI — for `wardynd` that is zero npm packages
  reported while the image ships the entire compiled console. It was downloadable
  from every main-branch CI run as `wardyn-sbom`, so the failure mode was someone
  trusting a manifest that misrepresents the product. The per-digest attested
  SBOMs replace it, and Trivy already scans all five images on every PR, so
  "does our tooling still run" stays covered.

### Changed

- **`make setup` pulls the published images instead of building them**, falling
  back to a build when any is missing — a version whose images are not pushed yet,
  an air-gapped host, an unreachable registry, or a working tree ahead of the tag.
  `WARDYN_BUILD_LOCAL=1` forces the build.
- **Every published image now carries `LICENSE`, `NOTICE`,
  `THIRD-PARTY-NOTICES.md` and the verbatim licence texts** at
  `/usr/share/doc/wardyn/`, plus `org.opencontainers.image.*` labels whose
  `licenses` field states what is *actually* in the image rather than what Wardyn's
  own code is licensed under. Apache-2.0 §4(a)/(d) make these conditions of the
  grant, and publishing an image is distribution.
- **The Go licence gate is an allowlist.** `go-licenses` defaults to
  `--disallowed_types=forbidden,unknown`; the explicit `forbidden,restricted`
  override added `restricted` but silently dropped `unknown`, so a dependency with
  no detectable licence passed a gate that would have caught it out of the box.
  Both gates now read one shared `licenses/ALLOWED-LICENSES.txt`.
- **The UI licence gate cannot pass vacuously** — it rejected an empty licence
  expression as fully vouched-for, and reported success having examined zero
  packages when `node_modules` was absent. Its wildcards are gone (a `BSD-*`
  allowlist admits BSD-4-Clause), and `--self-test` pins the residue logic against
  19 expressions.
- **Trivy scans every published image**, not the single one that 0.6.2 stops
  publishing. `check-image-pins` fails if the release and scan matrices drift.
- **The bundled OFL fonts ship with their licence.** 14 `.woff2` files reached
  `ui/dist` and no OFL text did. The shadcn/ui-derived console primitives are
  attributed (MIT, © 2023 shadcn) rather than carrying no copyright line at all.

## [0.6.1] — 2026-08-25

First patch on 0.6. Three CI jobs that had never run before the v0.6.0 push went
red on it; two were real, pre-existing defects. Plus the security sweep and a
documentation regression.

### Fixed

- **The desktop tier could not boot on older Docker Compose.**
  `deploy/desktop/docker-compose.yaml` `include:`d the base compose file and then
  re-declared `services.wardynd` to add one volume — a service-name collision that
  newer Compose merges and older Compose refuses outright
  (`services.wardynd conflicts with imported resource`). The mount moves into the
  base file as `${WARDYN_MANAGED_DIR:-…}:/etc/wardyn:ro`, the same
  variable-with-harmless-default idiom that file already uses for
  `WARDYN_WORKSPACES_ROOT` and `WARDYN_BEDROCK_AWS_DIR`; the desktop file is now
  `include:`-only, so nothing can collide at any Compose version.
  `wardyn-desktop.sh` exports the variable, and every non-desktop deployment
  leaves it unset and mounts nothing.
- **`make compose-config` now validates the desktop entrypoint too.** It only ever
  parsed the base file and the CI overlay, and the desktop guard was a text grep
  that never asked Compose to resolve the `include:` — which is why the collision
  above was invisible to every daemon-free gate and surfaced first in CI.
- **The agent images' bundled `npm` carried a CRITICAL.** `node-tar` 7.5.11
  (CVE-2026-59873, gzip-bomb DoS) ships inside npm's own vendored `node_modules`,
  so no application-level pin reaches it — and the current
  `node:22-bookworm-slim` still ships it. `claude-code` and `codex-cli` now
  install `npm@11.19.0`, which vendors the patched 7.5.19. `aws-sso` is
  debian-based with no npm and is unaffected.
- **Five development-scope advisories pinned forward** via `pnpm.overrides`, all
  at patch level with no direct-dependency bump: `brace-expansion` 2.1.2/5.0.7,
  `undici` 7.29.0, `postcss` 8.5.23, `mermaid` 11.16.1, `dompurify` 3.4.13. These
  are build/test tooling and the docs diagram gate — nothing in `ui/src` imports
  any of them, so the shipped bundle is unchanged. `make npm-audit` is unchanged
  and still `--prod --audit-level=high` by design: development-scope tooling is
  outside its scope by construction, not by suppression, and there is no ignore
  list.

### Changed

- **The Quickstart no longer implies a model is required.** Connecting a model was
  sequenced as step 2 of getting started with no qualifier, while the code has
  reported it as optional and explicitly non-blocking since 0.4 (`setup status`
  renders it INFO, never a gap to clear). The model step now follows a working
  `wardyn run`, is introduced as optional, and says outright that skipping it is a
  supported end state. The run example now explains that `--task-mode exec` means
  no agent and no model, and that `--agent` names a sandbox image rather than
  asserting an AI runs the task.
- **Wardyn describes itself as a governed-sandbox control plane for any workload**
  on the surfaces that still said "for coding agents" — the Helm chart's
  description and keywords, and the GitHub repository description. Coding agents
  remain the flagship use; they were never the whole product, and the console's
  own copy already said so.
- `ROADMAP.md` names the underlying wart: `POST /runs` requires an `agent` field
  even for `task_mode=exec` runs that have no agent, which is a wire-contract
  change to fix.

## [0.6.0] — 2026-08-23

### Added

- **Members onboard their own workspaces from the console.** The Workspaces
  screen and the New-Run wizard's Add-a-workspace dialog now work for a member
  session against the member-scoped routes: create and scan your OWN
  workspaces, with the admin-set mount boundary shown in place — the daemon
  tells the console the member's local-dir root (`member_local_dir_root` on
  `GET /me`), and the path field says so. The writable checkbox does not
  exist for members (the request never carries `writable`; the server-side
  allowlist is the boundary either way). The mock at
  `docs/design/ui-batch2-mock.md` is the design source of truth for every
  string.
- **Workspace reassignment (offboarding).** `POST /workspaces/{id}/reassign`
  (admin-only) moves a member-owned workspace to operator ownership
  (`owned_by=''`), audited `workspace.reassign` with `from_owner`. Members get
  the same constant 403 as every admin-only workspace route — deliberately
  more existence-blind than a 404 split.
- **No impersonation in the audit trail.** When an admin acts on a
  member-owned workspace, the audit actor stays the ADMIN's identity, and the
  event carries `workspace_owner` naming the member — cross-user admin access
  is queryable (`?actor=` plus `workspace_owner` ≠ actor), pinned by a guard
  test at every workspace write site.
- **A failed run says why, in the console.** The run page's FAILED state
  renders the `failure_hint` the backend has stamped since migration `0044` —
  bare server text beside the state badge, nothing when there is no hint.
  The unlisted-host copy in the New-Run wizard now describes what the proxy
  actually does (refused-and-raised, approve once, retry gets through), and
  the egress panel names the agent CLI's telemetry endpoint for what it is.
- **Desktop install lane.** `deploy/desktop/` gains `install.sh` (managed
  dir, per-device age key minted at install — never distributed via MDM),
  `com.wardyn.daemon.plist` (launchd), and `wardyn-desktop.sh`
  (`docker compose --env-file … -p wardyn-desktop up -d --no-build
  --pull always`, healthz wait, idempotent `wardyn site-config apply`).
  `wardyn_pick_docker_host` now recognizes a Colima socket the way it does
  Rancher's — without it, confinement silently collapses on Colima Macs.
- **Desktop honesty gates.** `scripts/test-desktop-profile.sh` joins
  `make test-scripts` (which CI runs), and a `desktop-envelope` CI job boots
  compose with the example profile and asserts the managed policy file is
  exactly what the daemon serves, an unpoliced run resolves to the ceiling,
  and profile synthesis stays clamped. The one scripted macOS smoke run is an
  operator step documented in `docs/DESKTOP.md` — run once, paste output; CI
  does not cover it and the doc says so.
- **ROADMAP truth.** The desktop slice moves to 0.6; interactive
  tool-approvals→console is marked deferred to 0.7 with its reasons (the
  prompt-tool contract is non-interactive-only, the hook alternative fails
  open on timeout, and a self-service member who could approve can already
  attach).
- **Per-user API tokens.** A signed-in human mints `wdn_…` bearer tokens for
  themselves (`POST /me/tokens` returns the plaintext exactly once; `GET`/
  `DELETE /me/tokens`); an admin can list and revoke anyone's (`/tokens`).
  Only the SHA-256 is stored (migration `0045`). A token authenticates **as
  the human who minted it** — it publishes the same context the OIDC session
  does, so grants, RBAC and ownership bind identically and a member's token
  can never reach an admin route. Audited `token.create`/`token.revoke`.
- **`wardynd -rotate-age-key <path>`: age-key rotation as a maintenance
  mode.** With the daemon stopped, mints a new age identity, re-encrypts every
  stored secret in one transaction (any row that fails to decrypt aborts the
  whole rotation), swaps the key file atomically and exits. The `wardyn` CLI
  never sees the key. Audited `secret.rekey` (count only, no names).
- **Hash-chained audit log** (migration `0047`). Every new event carries
  `prev_hash`/`row_hash` (SHA-256 over the previous hash and the row's
  immutable fields, computed by Postgres under a transaction-scoped advisory
  lock so chain order equals commit order). The head hash rides the audit-sink
  stream so an external SIEM can detect truncation; `GET /audit/chain/verify`
  (admin) walks the chain and reports the first break. Tamper-*evident*, not
  tamper-proof — a database owner can rewrite the whole chain; the threat
  model says so.
- **`env_secret` grant kind.** Injects a named stored secret as a sandbox
  environment variable at dispatch. Resident for the run's lifetime and not
  revocable mid-run — its own threat-model row — so it is admin-only unless
  `WARDYN_ALLOW_MEMBER_ENV_SECRET` opens it to members. The grant-pairing
  table is now closed: an unknown grant kind is refused instead of falling
  through unclamped.
- **`git_pat` per-run lease.** A `git_pat` approval decided with
  `decision_scope=run` re-mints for the rest of that run under the one
  decision; mints are stamped `lease` in `credential.mint`. The scope is
  compared as stored — a legacy approval never silently becomes a lease.
- **Second-human egress approval.** `WARDYN_EGRESS_SECOND_HUMAN=1` refuses an
  egress decision by the run's own creator (`authz.denied`,
  `reason: second_human_required`). The shared admin token has no per-human
  identity and bypasses the rule — that bypass is audited
  (`approval.second_human.bypass`) and documented as break-glass, not hidden.
- **`auth.failed` audit event.** Admin-token 401s and rejected OIDC session
  cookies used to fail silently; they now emit a content-free `auth.failed`
  (reason, source IP, path) behind a process-local token bucket so a scanner
  cannot flood the append-only log.
- **`WARDYN_AUDIT_SOURCE`** stamps a static `source` field on every event a
  sink serializes — one SIEM index can tell instances apart. Sink payloads
  only, never Postgres. OPERATIONS.md gains Splunk HEC / generic-webhook
  recipes.
- **`WARDYN_REQUIRE_OPERATOR_SET_EGRESS`** (default off) applies the
  scan-seeded provenance guard that already protected secrets to egress
  domains: a run may not carry egress the operator never set.
- **Setup status grades the permissioning posture** — the fail-open
  enforcement switches are scored as a check, informational and non-blocking.
- **`If-Match` on `PUT /permissions/enforcement` and site-config apply.**
  Both whole-replace surfaces return an `ETag`; a stale `If-Match` is refused
  with 412 before the write reaches the store. Omitting the header keeps
  today's behaviour.
- **SSH admin override is bounded-stale, not permanent** (migration `0046`).
  Every OIDC login re-stamps `role`/`role_checked_at` on the principal's
  registered keys; the gateway refuses the override once the stamp is older
  than `WARDYN_SSH_ROLE_TTL` (default 24h). Keys registered before 0.6 carry
  no stamp and never gain the override until their owner logs in again. A
  direct-SQL admin-key registration must stamp `role_checked_at` too — the
  API path already does; a bare `role='admin'` insert is correctly refused as
  never-checked, and `docs/SSH.md` now says so explicitly.
- **Session revocation.** `POST /sessions/revoke` (admin; `wardyn sessions
  revoke --sub … | --all`) invalidates every current console session for one
  principal or for everyone, effective immediately (migration `0049`).
  Sessions are stateless cookies, so revocation is a per-principal cutoff
  time the middleware checks on every request — fail-closed when the store
  errors. It also revokes every unrevoked API token the target holds: a
  `wdn_` bearer is that human's session in another form, so "revoke a human
  now" covers both in one call. Audited `session.revoke` (with
  `tokens_revoked`); a request presenting a revoked cookie surfaces as
  `auth.failed` with `reason: revoked_session`.
- **`wardyn support-bundle`** gathers version, healthz, setup status, a bounded
  audit tail and the compose config — secret values redacted, including
  commented-out lines — into a tar.gz for a support ticket.
- **OIDC token exchange retries transient IdP errors** (5xx/timeout, at most
  three attempts with backoff) and distinguishes them from a configuration
  error on the error page.
- **linux/arm64 images.** `wardynd`, `wardyn-proxy` and the agent images build
  for `linux/amd64,linux/arm64` (pure-Go cross-compile; QEMU only for runtime
  stages); every base-image pin is an index digest, enforced by
  `scripts/test-image-pins.sh`. The release lane signs and attaches an SBOM
  for the agent images too, the agent CLI is pinned to an exact version, and
  CI runs a trivy scan (CRITICAL fails; accepted CVEs live in `.trivyignore`).
- **Managed desktop envelope.** `deploy/desktop/wardyn.env.example` +
  `docs/DESKTOP.md`: a local daemon per laptop under an MDM-managed policy
  file, developer = operator. The ceiling is stated verbatim — the developer
  is not the adversary in this tier — and the operator-unclamped inline-policy
  path is named, not hidden.
- **Member-owned workspaces (backend, migration `0048`).** A member may now
  create, update, delete and scan their own workspaces (`owned_by`); a foreign
  member's workspace answers the byte-identical 404 a missing one does. A
  member's `local_dir` mounts are allowed only under operator/MDM-set roots
  (`WARDYN_MEMBER_WORKSPACE_ROOTS`, or a per-member
  `WARDYN_MEMBER_WORKSPACE_ROOTS_MAP` that replaces the shared list),
  canonicalized at bind time (symlink and `..` escapes refused, `$HOME`
  dotfiles denied), and writable only under `WARDYN_MEMBER_WRITABLE_ROOTS`
  minus `WARDYN_MEMBER_WRITABLE_DENY` — both unset means no writable member
  mount at all. `WARDYN_MEMBER_MODE=1` refuses to start alongside local mode.
  The console flow and the reassign action follow in the next stage.
- **The `wait_for_review` hold window and concurrency are configurable.** A
  policy may set `first_use_hold_seconds` and `max_holds` instead of living
  with the built-in 30s/16; absent or zero keeps today's defaults. Note the
  cap is per held connection, not per distinct host — N concurrent connections
  to one unknown host consume N slots.
- **A denied CONNECT is distinguishable from one waiting on approval.** The
  proxy's 403 now carries `X-Wardyn-Egress: denied|approval-pending` plus
  `X-Wardyn-Host` (the refused host), so an agent — or a person reading its
  logs — can tell a hard deny from a first-use hold without grepping the
  audit log.
- **Audit list: per-principal `?actor=` filter and an uncapped NDJSON
  export.** `GET /audit` takes `?actor=` alongside the existing filters, and
  `GET /audit/export` streams the full filtered result as NDJSON — the "give
  the auditor everything for this principal" request stops being a pagination
  exercise.
- **Per-sink SIEM delivery-drop counter on `/metrics`.** A webhook sink that
  exhausts its retries now increments `wardyn_audit_sink_drops_total{sink=…}`
  instead of failing silently — the number a pilot's monitoring should alarm
  on.
- **A run that dies before its agent starts carries a `failure_hint`.**
  Dispatch-side failures (unresolvable image, lost sandbox, inspection
  refusal) used to land as a reason-less FAILED badge with the cause buried in
  the audit log; the run row now carries the one-line reason (migration
  `0044`; console rendering lands with the UI lane).
- **Enterprise-POC documentation set:** `docs/DATA-FLOW.md` (vendor-
  questionnaire-ready data-flow and sub-processor statement),
  `docs/AUDIT-ACTIONS.md` (the audit action vocabulary, curated from every
  emit site), an honest audit-retention/erasure section in OPERATIONS.md,
  OFL-1.1 font attribution in NOTICE, and six newly-disclosed residuals in
  the threat model.

- **UI sandboxes: a governed relay from your browser to one declared port
  inside a run's sandbox** (`docs/UI-SANDBOXES.md`). A run's policy may declare
  `ui_apps` — a name, a loopback port and a path, operator-authored, never a
  command string — and `wardynd` relays exactly those ports, over the same
  exec lane (`socat` on `Runner.ExecStream`) the SSH gateway's `-L` forward
  already uses: no pod/container-IP dial, no `NetworkPolicy` change, no new
  network path out of the sandbox. Off by default; it exists only when
  `WARDYN_UI_SANDBOX_LISTEN` names a **second address**, and boot refuses one
  equal to `-listen` — what the relay serves is the sandbox's own JavaScript,
  and the separate browser origin is what keeps it away from the console's
  session. Access is a single-use, 30s, owner-or-admin attach ticket (the same
  one the browser terminal mints) redeemed for a path-scoped, `HttpOnly`
  session cookie; the listener has no other credential and never falls through
  to the console session or admin bearer. Forwarded requests are stripped of
  every `wardyn_*` cookie plus `Authorization` and any `?ticket`, and responses
  are stripped of `Set-Cookie: wardyn_*`. **Nothing inside a relayed app is
  recorded** — no keystrokes, no screen, no page content; the audit trail is
  `ui.auth`/`ui.start`/`ui.open`/`ui.close`, deliberately distinct from
  `session.attach` so a relay session never appears in the recording picker.
  Deployment: `uiSandbox.*` in the Helm chart (its own port, and its own
  hostname — the README says why), and a loopback-only compose mapping on
  `WARDYN_UI_SANDBOX_PORT` that stays inert until the gateway is enabled.
  On the console, a run's "Attach from your terminal" card gains a third lane
  beside the Wardyn CLI and SSH: off, no-apps-declared, or one row per declared
  app with an Open button that mints an attach ticket and opens the relay's own
  origin in a new tab (`window.open(…, "noopener")`, never an iframe — an
  iframe is precisely the same-origin risk the second listener exists to
  avoid). The policy detail sheet shows `ui_apps` read-only; there is no
  in-console editor in 0.6. Boot also refuses the second listener on a routable
  address with no TLS posture — `-ui-sandbox-listen 192.168.1.5:8081` behind a
  loopback `-listen` previously served the 8h `wardyn_ui_sess` relay cookie in
  cleartext on a LAN interface, exactly the class the console's own guard
  already refused. The rule now lives beside the posture both listeners share
  (`refusePlaintextListen`), so the loopback/unspecified carve-outs and the
  `WARDYN_ALLOW_PLAINTEXT_LISTEN` escape hatch cannot drift apart
  ([docs/ENV.md](docs/ENV.md)).
- **`wardyn/agent-vscode` image variant** (`make agent-image-vscode`,
  `deploy/images/vscode/`): the claude-code image plus a pinned,
  sha256-verified `code-server` bound to `127.0.0.1:8080` and a
  `/usr/local/bin/wardyn-ui-vscode` launcher. The launcher path is the whole
  BYOI contract — any image can serve a declared app by shipping one, and an
  image without it gets a clean 502 naming the missing path, never a hang.
  ~+228 MiB over the base image, and not part of `agent-images`.
- **Permissioning: capability grants for a user, a group, or everyone.** An
  admin can now grant — or deny — one member, one IdP group, or every signed-in
  human a specific Wardyn capability, on four kinds: `egress_host` (which hosts
  they may decide an `egress_domain` approval for, and which may survive on
  their own `inline_policy` allowlist), `secret` (which stored secrets that
  policy may reference, and which names `GET /secrets` lists back), `workspace`
  (which onboarded workspace they may launch against), and `image` (which custom
  sandbox image they may name at all — the one kind that *widens* what a member
  can do; `devcontainer_repo` stays unconditionally admin-only). Rows live in
  `capability_grants` with a per-kind enforcement switch in
  `capability_enforcement` (migration 0042), managed through `GET /permissions`,
  `POST /permissions/grants`, `DELETE /permissions/grants/{id}` and `PUT
  /permissions/enforcement` (all admin-only), with `GET /me/capabilities` as the
  member-safe read of the caller's own effective set. Resolution is deny beats
  allow beats the switch, with admins, the admin token, and local mode exempt,
  and no cache (a new grant applies on the next request). **Every switch ships
  off**: a deployment upgraded from 0.5 with no rows written behaves
  byte-for-byte as it did before. The doctrine — *a capability bounds what the
  MEMBER chose, never what the ADMIN pre-authorized* — is why a stored policy, a
  workspace's requirements, scan-seeded hosts, and the model provider's own
  egress are never narrowed. See [docs/OPERATIONS.md](docs/OPERATIONS.md) →
  "Capabilities: what one member, or one group, may do".
- **An OIDC session now carries the group snapshot its capability grants match**
  (`internal/auth/oidc`): the union of the ID token's `roles` and `groups`
  claims, lowercased/deduped/sorted/printable-ASCII, capped at 2048 payload
  bytes and dropped from the alphabetical end so the truncation is deterministic
  and the signed cookie stays under the ~4096 bytes a browser silently discards
  whole. Membership is a login-time snapshot; grants themselves resolve per
  request. **No forced re-login**: a pre-0.6 cookie has no groups field, stays
  valid, and is reported distinctly as `groups_snapshot_stale` rather than as
  "holds no groups".
- Ground-truth heartbeat and `/healthz` now publish `dropped_unmapped`
  alongside the existing `dropped_total` and `observed_total`, and the
  `/healthz` idle state names which of its two causes it is ("kernel events
  observed but none correlated to a run" vs. the plain "no kernel events
  observed"), so "the sensor saw nothing" and "the sensor saw plenty and
  correlated none" stop reading as the same `observed_total: 0`.
- **SSH gateway admin override.** A registered public key now carries the
  role it was registered under (`role` column, migration
  `0043_ssh_key_role.sql`), and `sshAuth` authorizes a connection if
  `run.created_by == the key's principal` **OR** `key.role == admin` — an
  admin's own key now reaches any run over SSH, not just the browser
  terminal. The override is stamped at registration time, not checked live:
  it is honestly weaker than the web terminal's `requireOperator` gate,
  which re-reads the session's role on every attach, so a demoted admin's
  already-registered key keeps the override until that key is deleted and
  re-registered (or revoked) — there is no expiry or background sweep. Every
  override connection is audited distinctly (`ssh.auth` success carries
  `override:true` whenever the owner check did not match), and a member's
  key never satisfies the check regardless of registration age. **Upgrading:
  a key registered before 0.6 is backfilled as `member` and never gains the
  override** — the stamp is written only at registration and nothing
  re-stamps it, so an admin who registered a key under 0.5 must
  `DELETE /me/ssh-keys/{fingerprint}` and register it again to receive one.
  See [docs/SSH.md](docs/SSH.md) → "Bounds" and `threatmodel/THREAT-MODEL.md`
  residual #15.
- **One command from a bare host to a real Kubernetes cluster.** `make
  kind-quickstart` ([`deploy/kind/quickstart.sh`](deploy/kind/quickstart.sh))
  builds `wardynd` locally, stands up a `kind` cluster with a version-pinned
  Calico CNI and the k8s runner substrate on, `helm install`s the chart, and
  waits for a healthy control plane — the exact path CI's `helm-install-test`
  and `conformance-k8s` jobs already prove, now runnable by an operator in one
  command, printing the URL and admin token it minted. Both host port mappings
  bind `127.0.0.1` explicitly rather than `0.0.0.0`, so a leftover compose stack
  on the same ports fails loudly at cluster-create instead of silently
  absorbing the quickstart's traffic; the healthz proof names *who* answered,
  because both stacks publish `127.0.0.1:8080` and a 200 says nothing about
  which one replied. `make kind-down` tears it back down. The chart README now
  leads with this path before the full production install walkthrough, and
  [docs/README.md](docs/README.md) links the Helm deployment lane at all.
  Day-2 operations on Kubernetes are documented from commands run against a
  live cluster ([docs/OPERATIONS.md](docs/OPERATIONS.md)).
- **`GET /readyz` — a real readiness probe.** `/healthz` reports "ok"
  unconditionally with no Postgres check, but the chart used it for readiness,
  so a dead database read healthy and never left the Service's endpoint list.
  `/readyz` pings the store with a 3s timeout (503 on failure) and is what the
  chart's `readinessProbe` now targets; liveness and startup stay on `/healthz`
  so a transient DB blip does not restart-loop an otherwise-fine pod. The probe
  path is a chart value (`readinessProbe.path`), pinnable back to `/healthz`
  for images at or below 0.5.0, which predate `/readyz` and would otherwise
  stall every rollout at "not ready".
- **`/metrics` can see a dead store and a backed-up audit spool.** Every
  existing counter only moves on success, so a Postgres outage looked identical
  to an idle control plane on the scrape surface. Two gauges close it:
  `wardyn_store_up` (the same bounded ping `/readyz` makes) and
  `wardyn_audit_spool_lines` — a failed durable audit write spools to local
  JSONL for a background drain loop to replay, and a spool that never returns
  to 0 means that loop is not working, a condition that previously had no
  operator-visible signal at all.
- **`wardyn ssh <run-id>` — no more copy-pasting the connect string.** It
  reaches a run over the SSH gateway directly, exec'ing the local `ssh(1)`
  binary against the address read off the gateway's own `/healthz` — the same
  one the console's SSH card surfaces. It is a deliberately separate command
  from `attach`, not a flag on it: `attach` carries the admin bearer over a
  WebSocket, `ssh` carries a registered public key over the real SSH protocol,
  and collapsing the two would silently swap which credential a run session
  used. `--print` emits the raw command and `--config` an `ssh_config` Host
  block, both identical to what the run-detail card renders for the same run;
  the card now names the shortcut inline, above the raw command it replaces.
  A by-hand lane exercises the gateway against a Pod on the k8s substrate as
  well as against Docker (`make test-e2e-ssh-k8s`) — a manual proof, not a CI
  job: it runs against a cluster `make kind-quickstart` leaves behind, so a
  green result is evidence only for the tip someone actually ran it on. See
  [docs/SSH.md](docs/SSH.md).
- **`wardyn logs <run-id> [-f]`** tails a run's audited event trail (dispatch,
  egress, credential mints, completion) by reusing the existing audit-events
  pipeline. There is no raw agent stdout/stderr capture for exec-mode runs, so
  the command is honestly scoped to what actually gets audited. It reports an
  unknown or unauthorized run id immediately — with or without `--follow` —
  rather than exiting 0 on nothing or polling forever, and a followed run's
  tail runs until the run's terminal audit rows are drained, not merely until
  the run's state flips.
- **`approvals list`/`get` gain run and host visibility.** `approvals list
  --run <id>` filters by run — the SDK's `ListApprovals` now actually sends
  `?run_id=`, dead since decision scopes shipped it server-side — a `HOST`
  column is parsed from `requested_scope`, and a `HOLD` column flags a live
  `wait_for_review` egress hold with its remaining window. `approvals get <id>
  --run <run-id>` fills the gap left by there being no `GET /approvals/{id}`.
- **The default ceiling policy is viewable — UI, CLI and API.** `GET
  /policies/default` exposes the ceiling every policy-less run gets (the same
  one a member's inline policy is clamped against), previously unexposed on any
  surface. Reachable as `wardyn policy default`, the SDK's `GetDefaultPolicy`,
  and an expandable card on the Policies screen.
- **A Permissions screen, and inline why-denied moments for members.** Admins
  get a seventh sidebar entry showing the doctrine, each of the four capability
  kinds with a live sentence naming what it currently does or does not enforce,
  the grant table and an add form — a grant renders amber with no success
  toast, and an unenforced kind is labelled "Advisory until enforced"
  throughout, so the screen never implies a bound it is not applying. Members
  get three inline deltas driven by `GET /me/capabilities`: an `egress_domain`
  approval whose host they were not granted disables both decisions with the
  reason beside them; New Run *annotates* — never hides — the workspaces a
  member cannot launch against, because hiding would make the refusal
  undiscoverable; and the Secrets list says outright that it is showing only
  the names that member holds. All of it is advisory: the server remains the
  enforcement point.
- **A live safety meter while you author a policy.** `POST /policies/grade`
  runs the same `composer.Grade` verdict `preflight` computes for a launch,
  against a bare, unsaved spec — member-accessible, strictly decoded like
  every other policy write. The policy panel calls it debounced and paints a
  4-segment meter (Safest · Guarded · Elevated · Weakest); a parse failure
  dims it, and the title makes clear it grades the document, not the
  resolved run Preflight grades.
- **A Preflight button on the New-Run screen.** A secondary button beside
  Launch now sends the exact payload Launch would (one shared
  `buildRunInput` projection, not a hand-copied one) and renders the
  server's verdict inline — field-path 400s verbatim, or the risk grade,
  member-clamp warnings, and enforced confinement class via the same
  `RiskBadge`/`ConfinementChip` the run page uses.
- **A confined replay now carries an explicit Clean/Caught verdict.**
  `CleanReplay` stamps each `CONFINED` record-loop replay clean or caught —
  false on truncation, any deny, any pending, or an allow released only by
  a live mid-replay approval. The confined chip renders "Replayed clean",
  "Replayed — caught N" (warning tone), or "Replayed — not clean" with its
  cause named; the guided action approves just the hosts you select and
  replays again in one click, and a workspace's session list gains a
  roll-up line for whether its loop has ever closed clean.

### Changed

- **`POST /runs` and `POST /runs/preflight` now decode their request bodies
  strictly**, matching `POST /policies`: an unknown field (including a typo'd
  one nested inside `inline_policy`) is now a 400 naming the field, where it
  was previously ignored silently. Compat note: this can break an external
  SDK/CLI client sending a field newer than an older server understands —
  previously tolerated version-skew now hard-fails instead of degrading.
- **`image` moves from admin-only to grantable, and `workspace` becomes
  gateable** (`denyMemberRequest`, formerly `denyMemberCustomImage`): a member
  naming a custom image now needs the `image` kind enforced *and* an exact-ref
  grant (unenforced still refuses, exactly as 0.5 did), while naming a workspace
  stays allowed until an admin enforces `workspace`. A member's `inline_policy`
  is narrowed, after the existing operator clamp, to the hosts and secrets that
  member personally holds — dropped with a warning, never rejected, so the run
  still launches on its admin-authored egress. The warning appears twice: on
  the preflight/Review dry-run *before* launch, and again on the `201` of the
  launch itself, where the console raises it as a toast and `wardyn run`
  prints it to stderr. `GET /secrets` likewise lists a member only the names
  their own grants cover, once `secret` is enforced.
- **BREAKING — `pkg/client.ListApprovals` gains a `runID uuid.UUID`
  parameter**, positionally between `state` and the variadic `ListOpts`:
  `ListApprovals(ctx, state, opts...)` becomes
  `ListApprovals(ctx, state, runID, opts...)`. Pass `uuid.Nil` for "every
  run" — the previous behaviour. The method never sent the `?run_id=` filter
  the server has supported since decision scopes shipped; adding it as an
  option would have left the filter as easy to forget as it already was.
  Every SDK caller must update to compile.
- **Upgrading a Kubernetes install: `helm upgrade --reuse-values` is still not
  the path across 0.5 → 0.6.** `--reuse-values` replaces the new chart's
  `values.yaml` with the previous release's, so the value blocks 0.6 added
  (the UI-sandbox gateway, the readiness-probe path) are absent from the map
  the templates read. The chart now reads every one of them through a
  `default dict` and its `values.yaml` leaf default, so that upgrade renders
  instead of dying on a nil map — but it renders with the new defaults and no
  way to see them. Use `-f your-values.yaml`, or `--reset-then-reuse-values`
  (Helm ≥ 3.14), which starts from the new chart's defaults and layers the
  previous release's overrides on top. [docs/OPERATIONS.md](docs/OPERATIONS.md)
  → "`helm upgrade`, and why `--wait` is not optional" carries the recipe and
  the two Helm sharp edges it steps around.
- **`/runs/new` and `/policies` now author policy through one shared
  panel.** The wizard's bespoke Confinement/Network cards, presets,
  Unlisted-host dialog and Record radio are gone, along with `/policies`'
  separate editor; both screens render the same spec textarea, template
  chips (Minimal, Model provider, Package registries, CI baseline,
  Allow-all) and helper rail. On `/runs/new`, editing the spec detaches a
  chosen saved policy, and the Barrier selector up-clamps to the active
  floor with a reason line on every disabled tier.
- **The demos catalog moves into Getting Started; `/demos` now redirects
  there.** The Getting Started demo phase lists the whole catalog — split
  into Egress demos and Secrets demos sections — replacing the old frozen
  five-step subset; `/demos` redirects to `/setup?step=sealed-box`, the
  same pattern `/integrations` already followed. `DemoDetail` is the one
  renderer for both sections now.

### Fixed

- **The default agent image pulls the daemon's own version tag, not a
  floating `:latest`.** A daemon at vX.Y no longer silently picks up whatever
  image was pushed last; the fallback resolves to the matching version tag.
- **A sandbox orphaned by a crash before its ref was recorded is swept.** The
  boot reconciler only knew sandboxes by their stored ref; a crash in the
  window before `SetSandboxRef` left a live, credentialed container nothing
  would ever revisit. The reconciler now also sweeps by the run-ID label the
  runner stamps on every sandbox it creates.
- **Agent-CLI telemetry is suppressed inside the sandbox**, so a pilot's
  first-use egress approval prompt is for the code host — not for the
  harness's own metrics endpoint.
- **The compose stack survives a host reboot** — `postgres` and `wardynd`
  carry `restart: unless-stopped`.
- **The UI license gate fails closed.** `scripts/check-ui-licenses.sh` was a
  denylist (an unknown new license passed); it is now an allowlist.

- **Secrets: the Value field masks at entry, with a reveal toggle.** A
  write-only store no longer puts the plaintext on screen while it is typed
  (`-webkit-text-security`, so multiline PEM values keep working; Firefox
  ignores it and degrades to plaintext — cosmetic masking, not a security
  boundary). The reveal state resets each time the dialog opens, so the next
  Add/Rotate never inherits the previous one's plaintext.
- **An exec-mode run's page stops calling it an agent.** A run launched with
  no agent and no model was chipped "autonomous — the agent drives", badged
  "agent exit 0", and watched "anything the agent tries". `task_mode` is
  request-scoped and lives only in the `run.create` audit event, so the run
  page derives it from the trail it already holds: an exec run now chips
  "exec — shell command, no agent harness", the exit chip drops the word (it
  is true in every mode), and the idle hint says "anything this run tries".
- **The recording banner derives its tier from the runner, not from a
  New-Run preference.** The Recorded-sessions warning card guessed the
  confinement tier from the operator's persisted New-Run default in
  `localStorage` — an unrelated setting — falling back to a hardcoded CC1, so
  a capture that actually ran under Vault was captioned "Fence — the weakest
  barrier". The backend launches every recording under the runner's
  *strongest* class, and the card now reads that: the open-recording-allows-
  all-egress line is stated on every tier, and the weakest-barrier line is
  added only when the session genuinely runs under CC1.
- **Ground-truth's control-plane counter could freeze on a live sensor.** The
  ingest sidecar built its container→run index from a `docker ps` snapshot
  (running containers only) and replaced it wholesale on every refresh, while
  the Tetragon export tails with lag — a run whose container exited before the
  tail caught up resolved unmapped, was dropped, and moved no counter at all.
  The index is now fed by `docker events` (a container is known at CREATE,
  before its first exec) and merged rather than replaced, with entries
  outliving their container by 15 minutes so a lagging tail still correlates.
- **A persistent-Postgres install with the default ephemeral age key now
  refuses to render, instead of crash-looping on its second restart.**
  External-DSN installs pair with the default ephemeral age identity —
  regenerated every boot — so a second restart cannot decrypt what the first
  boot encrypted. That combination previously installed cleanly and only failed
  later, unrecoverably. The chart refuses it outright
  (`secrets.allowEphemeralAgeKey=true` is the explicit "I accept losing every
  stored secret on restart" opt-out, the same shape as `allowMultiReplica`),
  and the README's own external-DSN install commands — which walked operators
  into the same trap — now pass `secrets.ageKeyFromSecret=true` throughout.
- **The egress canary names the ambient-NetworkPolicy trap — and stops
  recommending a fix that would have widened every sandbox's egress.** A
  pre-existing default-deny `NetworkPolicy` in the runs namespace, unrelated to
  Wardyn, blocks the canary's baseline-reachability phase and produced an
  `INDETERMINATE` boot refusal with no documented cause. The error and the
  chart docs now name it directly, and the originally-suggested remediation (an
  allow-rule for `wardyn.managed=true`) is corrected: that rule is additive and
  both the agent and proxy pods carry the label, so it would have widened every
  run's egress past its per-run deny+proxy-only policy and flipped the canary
  to "CNI does not enforce". The documented fix is to exempt Wardyn's pods from
  the *ambient* policy's own `podSelector`, or to use a clean namespace.
- **`make reset` warns before destroying the corporate baseline it shares with
  `make reset-all`.** Plain `reset` took the upstream proxy, artifact mirrors
  and SCM host configuration down with no warning and no capture command. Both
  paths now print one shared hint — and only while `wardynd` is actually
  running, because the capture command runs inside it, so the hint is withheld
  exactly when it could not work.
- **`make doctor` is read-only again, and honours `WARDYN_PG_PORT`.** Its
  socket-mountability probe silently pulled `alpine:3.20` from the network; it
  now runs `--pull=never` and skips outright when the image is not already
  local. Its Postgres port check respects the same `WARDYN_PG_PORT` override
  its api/registry/ssh siblings already did.
- **A typo'd `site-config apply` key fails on the host instead of deleting the
  setting.** `apply` replaces the whole stored document, so a misspelled field
  was silently dropped by a lenient decode and the real setting erased with it.
  The CLI now decodes strictly and reports how many `Integrations` entries a
  round-trip silently dropped, rather than exiting 0 as though nothing were
  lost.
- **`wardyn setup wall|vault` stops exiting 0 when nothing was enabled.** An
  unsupported host, a plan-only run, a declined confirm, or a non-interactive
  empty stdin all silently "succeeded" at doing nothing. Every such path now
  exits 1 naming the reason, and a successful `--run` re-probes `docker info`
  for the runtime actually in effect rather than trusting the install script's
  exit code alone.
- **A paste over 32 KiB no longer kills the web attach terminal.** The attach
  WebSocket had no explicit read limit, so `coder/websocket`'s default 32 KiB
  cutoff closed the whole session — rather than truncating — the moment an
  operator pasted a long patch or log. The limit is now explicit at 1 MiB.
- **A long attach session's recording is truncated, not thrown away.** The live
  asciicast was buffered unbounded in the daemon's heap and written once at
  close, so a long session hit the recording store's 64 MiB cap and was
  rejected *whole* — the entire session's evidence lost at the moment it should
  have been persisted. The buffer is capped at 8 MiB and an over-long session
  is saved truncated (audited with `truncated:true`) rather than not saved at
  all. A redundant per-keystroke `agent_runs` UPDATE went with it: a 30s
  keepalive already covers liveness.
- **`ci-run.sh` survives a failed refetch and a job cancel.** A failed post-run
  refetch used to truncate the already-captured `run.json` via shell
  redirection before the fetch ran; it now refetches to a temp file and
  replaces the real one only on success. A hard cancel (SIGTERM) skipped
  teardown entirely because cleanup ran only on the `EXIT` trap — TERM/INT now
  route through it too — and the run's own terminal recording is collected into
  the CI output directory instead of being dropped with the recordings volume.
- **`--dry-run` preflight stops flagging a false "missing model access" blocker
  for exec runs.** The preflight checklist called the same model-access
  resolver as the real launch path unconditionally, but launch itself exempts
  `task_mode=exec` — so every CI exec job's dry-run preview showed a blocker
  launch would never raise.
- **A duplicate policy name is a 409, not a raw 500.** Creating a policy with a
  name already in use returned a blanket server error carrying the raw database
  message; it now maps to a 409 whose message the create-policy dialog surfaces
  directly.
- **A member can start a demo again.** Redacting setup status for members
  zeroed the confinement-classes field along with genuine diagnostic detail —
  but the demo screen's Start-button readiness check reads that same field, so
  no member could start a keyless demo regardless of the runner's real state.
  The field survives redaction now; the driver name and per-class substrate
  detail still do not.
- **A credential approval that simply expired no longer wedges the run
  permanently.** `ensureApproval` kept re-finding the same aged-out `EXPIRED`
  approval on every retry, and minting maps `EXPIRED` to a denial — so a run
  blocked on an approval nobody reached in time was stuck forever. Both lookups
  skip expired rows, so the next attempt raises a fresh pending request; a
  genuine human denial still stays terminal.
- **A malformed verify-egress approval audits its own no-op.** Approving a
  request with an empty or malformed host in `requested_scope` silently wrote
  nothing to the workspace's requirements contract — a green UI with no durable
  effect. The guard now emits an audit event on the miss, matching the
  merge-failure path beside it.
- **A member's empty Audit feed, the link into it, and its truncation now tell
  the truth.** A member's unfiltered `/audit` is always empty by design — the
  server scopes non-admins to `?run_id=` of a run they own — but the copy read
  this as "you have no runs yet". The copy is fixed, the run-detail Audit tab's
  link into the full feed carries `?run_id=` so the filter is actually
  reachable (and survives a reload via URL state), and the tab notes when it is
  capped at the server's 1000-row default.
- **The console stops polling the expensive setup-status endpoint twice, and
  warns before an SSO session dies mid-work.** The top bar's barrier chip ran
  its own poll of `/setup/status` on top of the app shell's separate poll of
  the same endpoint — which runs a full run list plus a host sweep that shells
  out. The two collapsed into one, then the heartbeat need itself moved to the
  cheap, unauthenticated `/healthz`. A warning now appears before an SSO
  session's ID token expires, instead of a silent 401 wiping the console back
  to sign-in.
- **Console honesty fixes.** The setup footer's gate action button is
  operator-gated, matching its sibling inline Test buttons. Container-login
  success text names the provider actually connected instead of always claiming
  a Claude subscription. The welcome screen stops reading an unreachable daemon
  as a real "needs setup" state and shows the same "Checking…" state the rest
  of the app uses. The operator-only refusal text names *admin*, not a role
  that does not exist. The confined-review card honours approvals recorded on
  the requirements contract, not just the legacy `approved_egress` list, so a
  host approved live during a confined replay stops rendering blocked behind a
  duplicate-writing Approve button. The live-hold badge stops treating any
  pending `wait_for_review` approval as an active hold — the proxy's real hold
  times out in 30s while the approval can stay pending for up to 24h — and a
  failed poll stops rendering as "all clear".
- **Sandboxes get lowercase proxy env too.** `curl` and most HTTP clients
  (post-httpoxy) deliberately ignore the uppercase `HTTP_PROXY` for plain-
  `http://` URLs, so an in-sandbox `http://` fetch bypassed the proxy
  outright and failed DNS instead of being inspected. `http_proxy`/
  `https_proxy` now join `HTTP_PROXY`/`HTTPS_PROXY` in every run's
  environment.
- **The compose stack's RBAC env now actually reaches the container.**
  `WARDYN_OIDC_ROLE_MAP` and `WARDYN_OIDC_DEFAULT_ROLE` were never
  plumbed into `wardynd`'s environment in `docker-compose.yaml` — the
  admin/member RBAC path the README describes was silently inert on
  compose, the only deployment with the gap (desktop env and the Helm
  chart both already carried them). Both variables now reach the
  container.
- **`AWS_CLI_INSTALL=staged` now actually reaches the aws-sso image
  build.** The Makefile never forwarded the build-arg to
  `DOCKER_BUILD_ARGS`, so an offline/strict-allowlist
  `make agent-images AWS_CLI_INSTALL=staged` silently fell back to
  downloading the AWS CLI installer over the network instead of using
  the staged one. The arg now joins the other install-mode args on all
  four image builds.

### Security

- **A multi-dot host spelling could slip past an explicit egress deny.** The
  proxy's host normalizer stripped exactly one trailing dot,
  so under `allow_all_egress` a CONNECT to `evil.com..:443` failed to match an
  explicit deny key for `evil.com` and sailed through. All trailing dots are
  now stripped before any policy comparison, and a regression test pins every
  multi-dot spelling to the same decision as the bare name.
- **`credential.mint` is written inside the mint transaction.** The audit row
  for a brokered credential mint committed separately from the mint itself, so
  a crash between the two could leave a minted credential with no audit trace
  (or the reverse). The row now commits atomically with the mint; the same
  pass closed the sibling seams — a decided `always` egress approval whose
  workspace write-back was lost to a crash is now healed by a boot-time
  reconcile (the API layer cannot share a transaction across the decision and
  the workspace write, so the window closes at the next daemon boot rather
  than shrinking to zero), and the audit spool's crash-recovery keeps the
  good head of a torn tail instead of discarding it.
- **Secrets masked in audit `Data` even when JSON-escaped.** The audit masker
  compared raw secret bytes, so a value containing quotes/backslashes appeared
  unmasked in event payloads once JSON-encoded. The masker now also matches
  the JSON-escaped form of every registered secret.

- **`wardyn-ssh-host-key` was listable and overwritable through the generic
  secrets API.** The SSH gateway's ed25519 host key sat in the broker's
  reserved set but not in `internal/api`'s, so the half of the guard facing
  the operator never applied: `GET /secrets` listed it, `PUT`/`DELETE`
  overwrote or removed it — a junk value regenerates the host key at the next
  boot and breaks every pinned fingerprint, which is the warning `ssh(1)`
  prints for a man-in-the-middle — and an `api_key` grant could name it. It
  is now reserved on both sides, the same omission `wardyn-ui-session-key`
  had. The two hand-written maps live in packages that cannot see
  `cmd/wardynd`'s platform-key constants, so the test that would have caught
  this lives in `cmd/wardynd`, where all three are visible, and fails if any
  daemon-GENERATED key is missing from either set. The operator-PROVIDED
  GitHub App pair stays deliberately out of it: those must remain `Put`-able.
- **A member's dropped secret pairing is now audited, not just warned about.**
  `filterMemberGrants` drops an `inline_policy` grant that pairs a stored secret
  with a host the operator never eligible-listed; that drop previously produced
  a clamp warning and no audit event, so a deliberate exfil *attempt* left no
  operator-visible trace (a gap [ROADMAP.md](ROADMAP.md) named). It now records
  an `authz.denied` event with reason `grant_pairing_not_eligible`, aggregated
  one event per reason with the affected values beside it — never on a preflight
  dry-run, where a stream of denials for a policy nobody launched would be
  indistinguishable from denials that actually bounded a run. Capability drops
  audit the same way (`capability_egress_host`, `capability_secret`), and a
  capability refusal at launch or at an approval decision audits as
  `capability_workspace`, `capability_egress_host`, or `byoi_member`.
- **`k8s.enabled` refuses `serviceAccount.create=false` with no explicit
  name.** That combination let the k8s-runner RBAC role — `pods/exec`,
  `secrets` create/delete, `networkpolicies` create/delete — silently bind to
  the namespace's `default` ServiceAccount, and therefore to every other pod in
  the namespace using it. The chart fails the render for that exact
  combination; an explicit `serviceAccount.name` still renders fine.
- **The `?ticket=` attach lane audits its own refusals.** This route is the
  only path to a live terminal that bypasses `humanOrAdminAuth`, and it
  recorded no denials at all — a scan against it with guessed tickets left no
  trace, unlike the SSH gateway's `ssh.auth`. Both refusal shapes now emit
  `session.attach`/`failure` with the source IP.
- **Plaintext run credentials no longer live for the daemon's lifetime.**
  `wardynd` held every run's plaintext secrets in memory indefinitely, so a
  long-uptime daemon accumulated the credentials of every run it had ever
  dispatched with no eviction path in production. A background sweep evicts a
  run's secret corpus one hour after it goes terminal, on a 15-minute ticker,
  and fails closed: a store error or an unresolvable run id keeps the secrets
  rather than guessing them safe to drop.
- **A member could read a colleague's run telemetry on a shared workspace.**
  `GET /workspaces/{id}/observed-egress` aggregated denied-egress targets from
  every run that had touched the workspace with no per-caller filter, leaking
  telemetry from the very runs `/runs/{id}` itself 404s that member out of. The
  endpoint applies the same owner-or-admin scoping as the runs list.
- **The react-router pnpm-audit suppression is gone — the advisory it covered
  was already patched.** `GHSA-qwww-vcr4-c8h2` patches at both 7.18.2 and
  8.3.0, not only at the 8.x major the suppression's comment claimed; a stale
  "no patch exists on 7.x" note kept `ignoreGhsas` alive well past the release
  that refuted it. `react-router-dom` moves `^7.18.1` → `^7.18.2` and the
  suppression is deleted outright — `make npm-audit` passes against the real
  advisory set with **nothing** ignored. The 7 → 8 major stays a named gap in
  [ROADMAP.md](ROADMAP.md), now on its actual merits: every stable 8.x
  peer-depends on React >=19.2.7 against this console's 18.3.1, and
  `react-router-dom` has no 8.x release at all — a React 19 decision for a UI
  owner, not an advisory deadline.

## [0.5.0] — 2026-08-18

### Security

- **Go-live hardening: 29 confirmed ship-blockers closed across two adversarial
  review waves**, each with a regression test proven to fail on the pre-fix
  commit. The load-bearing ones: a member's `inline_policy` `llm_inspection`
  block is now clamped under the default ceiling (its `detector_sidecar_url`
  could otherwise become an un-allowlisted egress channel), and its secret
  corpus is referenced by name and resolved only at dispatch — never stored in
  a policy row, written to the append-only audit log, or copied into a
  compose/profile proposal; the git-broker's value-returning mint lanes refuse
  the GitHub App private key and the other reserved platform secrets; a
  **disabled** integration no longer grants a run model access; an explicit
  `WARDYN_LOCAL_MODE=true` no longer silently disables a configured OIDC/RBAC
  deployment (boot refuses the contradiction); a client-settable `run.Task` can
  no longer forge the operator-reserved harness-login path; SSH `ssh.auth`
  success is audited only after signature verification, not at key-offer time;
  an empty-ceiling `github_token` repo list is deny-all for a hand-authored
  spec; a tokened corp-mirror redirect is dialed to its real port, not always
  443; the exec-less (krun/CC3) sandbox path now applies the same fail-closed
  resource-cap gate as the exec path; and the host ground-truth sensor no
  longer forwards uncorrelated host-wide kernel events to the audit log/SIEM by
  default.
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

- **An interactive run can start on a seed, at boot.** The run's `task` —
  previously ignored for an interactive run — is now its optional boot seed,
  interpreted per `interactive_start`: with `"agent"` the sandbox starts the
  agent CLI on that prompt in a persistent tmux session the moment it boots
  (supervised: the agent reads and plans, then parks its first tool approval
  in the pane until you attach — set `seed_auto_tools` to let it use tools
  unsupervised before you join); with `"shell"` the seed runs as a startup
  command before the terminal is yours. Attaching joins the live session.
  Empty task = today's idle sandbox, unchanged. Server-launched runs
  (record/verify/login) are excluded from seeding by construction. The seed
  travels as env into the sandbox and is consumed at boot — **needs an image
  rebuild** (`make agent-images-core`); an older image ignores it and comes
  up idle. The CLI gains this with zero new flags: `wardyn run --interactive`
  with a task now seeds.
- **Autonomous Claude runs can park every tool action on a human:
  `tool_approvals: "hold"`.** Instead of `--dangerously-skip-permissions`,
  the run's claude executes under `--permission-mode manual` with an
  in-sandbox relay (`wardyn-toolgate`, a stdio MCP permission-prompt tool)
  that raises each gated tool use as a `tool_call` approval — the exact
  command or edit as the decision context — and blocks until an operator
  approves or denies it in the console (the run cockpit's approval strip now
  shows tool holds beside egress holds). Deny and expiry both refuse the
  action and the run continues; a relay that cannot reach the control plane
  denies rather than proceeds. Default stays `"auto"` (the sandbox is the
  boundary); `hold` is per-run, Claude-only (codex has no external approval
  contract), and read-only commands the harness itself deems safe still run
  without asking.
- **Egress approvals carry a decision scope: `once`, `run`, `until`, or
  `always`.** `POST /approvals/{id}/approve` and `/deny` accept
  `decision_scope` (plus `decision_expires_at` for `until`) on an
  `egress_domain` approval; `wardyn approve`/`wardyn deny` gain `--scope`/
  `--until`, the SDK gains `DecisionOpts` (`pkg/client`), and the console's
  approval queue gains a scope picker (Once / This run / Until… / Always)
  beside Approve/Deny. Omit the field and nothing changes — the default
  stays `run`, today's original behavior, held in the proxy's per-host cache
  for the rest of the run. `once` releases a single connection (one CONNECT
  tunnel on HTTPS; one request on plain HTTP) and is spent on first use;
  `until` is the same, bounded by a `decision_expires_at` up to 30 days out
  and enforced by the run's own proxy sidecar; `always` is
  **operator-only** and persists the host onto the target workspace's
  `approved_egress`/`denied_egress` (migrations
  `0039_approval_decision_scope.sql`, `0040_workspace_denied_egress.sql`,
  `0041_run_workspace_ids.sql`) so every future run against that workspace
  inherits the decision instead of re-raising it — deny beats allow, as
  everywhere else in the proxy. New `PUT /workspaces/{id}/denied-egress`
  (full-replace, mirrors `approved-egress`) is the only way to undo a
  permanent deny, including one that broke a workspace's own credential
  injection.
- **Runs have a name.** `POST /api/v1/runs` accepts `title` and `description`,
  both persisted on the run (migration `0038_run_title.sql`) and returned by
  every read. Runs that share a title are **grouped** on the Runs board — which
  now groups by title rather than by state; the triage the state sections
  provided survives as the state facet, attention-first group ordering, and
  per-state counts in each group header. `wardyn run` gains `--title` /
  `--description`. Both fields are **optional on the wire and required in the
  console**: the site-config probe, harness login and workspace record/verify all
  create runs with no human to name them, so a server-side requirement would
  break them. Untitled runs — including every run created before this — display
  by their task exactly as before.
- **An interactive run can open straight into the agent.**
  `interactive_start:"agent"` makes the attach shell launch the image's agent CLI
  in the prepared workspace, once, on first attach; `"shell"` (the default) keeps
  today's bare terminal. Request-scoped like `task_mode` — carried to the sandbox
  as `WARDYN_INTERACTIVE_START` and consumed by the image's attach `~/.bashrc`,
  so it covers the console terminal and the SSH gateway alike (both go through
  the same `Runner.Attach`). **Needs an image rebuild** — `make agent-images-core`
  — to take effect; until then an older image ignores the variable and degrades
  to a shell, which is the previous behavior.
- **The Integrations page's Tools tab**, the integration→tool "carries"
  chips in the workspace wizard (base-image, build, and verify steps), and
  the client-side mirrors of the bake conditions. Tools are what the image
  carries; the Integrations surface now speaks only to connections.

- **The `artifact_mirror`/`host_proxy` derivations.** The Integrations surface
  no longer synthesizes rows from `EgressRedirects`/`UpstreamProxySecretRef`:
  that is network topology, it already has a surface (Corporate network), and
  showing it twice made one config look like two. Nothing about how a run
  redirects or chains through the corporate proxy changes.
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
  (Fulcio/Rekor via the Actions OIDC token), and publishes a CycloneDX SBOM via
  the existing `make sbom` target as a downloadable workflow artifact
  (deliberately not auto-attached to the GitHub Release — RELEASING.md's
  release step is manual by design; attach it by hand if wanted). linux/amd64
  only today.
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

### Changed

- **New run asks for what the run mode actually needs.** An interactive agent run
  no longer shows a Task box: the server ignores `task` for one, so the prompt
  the operator typed there was never read by anything. It asks what to start with
  instead. A batch run asks for the task; a shell command asks for the command
  and no longer offers "Interactive" at all — that combination silently dropped
  the command, because the server ignores `task_mode` for an interactive run.
  Launch is now disabled until the form is complete, and says what it is waiting
  for; previously the screen had no client-side validation at all.
- **The Getting Started funnel is 10 steps, not 12.** The Directories & repos
  and Base images steps are gone; "Your work" is one step (Workspaces). The
  connection step renders the same two Settings cards rather than embedding the
  whole Integrations page.
- **A fresh install opens on Getting Started.** `/` redirects to `/setup` when
  the server reports no runs and this browser has never finished the funnel.
  Every other route stays directly reachable — this is not the old first-run
  gate, which redirected everything until setup was complete.
- `examples/policies/composer-dev.json` → **`claude-llm.json`** and
  `composer-dev-subscription.template.json` → **`claude-subscription.template.json`**.
  Same ceilings, names that no longer point at a deleted feature.
- `WARDYN_COMPOSER_CONFIG` is no longer read, written or passed through
  (`scripts/up.sh`, `deploy/compose/`). Nothing in the binary had consumed it
  since the composer was cut.

- **Integrations are now base components: one `kind` field plus
  `secrets[]`/`egress[]`/`config{}`.** The stored Category/Type split
  is gone — `kind` is one of the closed set (`anthropic_api_key`,
  `anthropic_subscription`, `bedrock`, `openai_api_key`,
  `github_app`, `git_host`)
  whose row carries its whole contract: each secret names its store ref and
  its **delivery** (`proxy_header` — never resident), `egress` is where the
  system lives, and `config` keys are validated per closed kind (an unknown
  key 400s by name; bedrock's lane key is now `auth_lane`). Stored
  pre-base-component rows are **folded forward at read time** (one-way,
  write-new — old documents stay readable; writes emit only the new shape),
  and legacy `artifact_mirror`/`host_proxy` rows are dropped from this
  surface by the fold — their configuration lives under Corporate network.
  `GET /api/v1/integrations` and `PUT /api/v1/integrations/{id}` speak the
  new shape only.

  **Migration note.** Two write-time rules are stricter than what the old
  shape stored, so a legacy row may need one edit before it re-saves:
  - a **generic**-kind secret row must state a `delivery` (the row IS the
    contract); a closed kind may still omit it, meaning its own bespoke
    transport carries that secret;
  - `delivery.mode` may only be `proxy_header`, and a row may carry **at most
    one** such secret. The resident modes are refused rather than stored:
    Wardyn has no generic lane that materializes a named secret into a sandbox
    path or env var (the resident lanes that do exist — `git_host`'s SSH key,
    Bedrock's AWS env — are per-provider and declare no delivery at all), and
    the proxy injects one credential header per host, so a second
    `proxy_header` secret would be silently dropped at dispatch. Split it into
    its own integration.

- **The run-time integration fold is now one base-component fold with two
  exceptions.** An api-key AI provider and a generic connection take the same
  path: the row's proxy-header secret becomes one `api_key` grant, and its
  `egress` joins the run's allowlist. `anthropic_subscription` and `bedrock`
  keep their own transports (an OAuth mount/inject lane; SigV4 via
  `WorkspaceBedrockRef`) because their credential genuinely is not an HTTP
  header. Injection is **role-agnostic** — a secret's declared delivery is what
  makes it presentable, never the name of its role — and still applies only
  where a workspace, a redirect, or a run actually NAMES the integration:
  configuring one grants nothing by itself.

- **A Bedrock integration's `region`/`model` now win over the boot flags** on
  the Integrations surface, matching what dispatch already did
  (`resolveBedrockAuth`: a selection wins only the fields it sets, with
  `WARDYN_BEDROCK_*` as the fallback). A wizard-completed Bedrock row on a
  deployment that never set those env vars reported `needs_setup` forever
  while its runs authenticated fine.

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

- **BREAKING — generic integration kinds.** `PUT /api/v1/integrations/{id}` now
  answers 400 for any kind outside the closed set (`anthropic_api_key`,
  `anthropic_subscription`, `bedrock`, `openai_api_key`, `github_app`,
  `git_host`), and the error names what is accepted. Generic kinds — package
  feeds, container registries, cloud providers, data stores, MCP servers, work
  tracking, observability, "other service" — were the operator-extensibility
  surface behind the Integrations catalog, and that catalog is gone. **A row
  stored under an earlier release is not destroyed:** it still deserializes,
  still sits in `SiteConfig`, and is still injected into a granted run by
  `internal/api/integrations_run.go`. It simply cannot be edited through the API
  any more.
- **BREAKING — `azure_openai` as an integration kind.** Its one capability
  powered the AI Run Composer, which was also removed; no agent tool can be
  pointed at an Azure OpenAI deployment. An `azure-openai-key` left in the
  secret store is untouched and inert.
- **BREAKING — `pkg/client`'s `CreateRunRequest.ComposeSessionID`**, with its
  server-side UUID validation and `run.create` audit stamping. The only thing
  that ever produced a real value was the composer, so the field had become one
  that accepted any UUID and correlated it to a conversation that can no longer
  exist.
- **The `/integrations` page** (and `/integrations/:id`). Both redirect to the
  new `/settings`. Connections are four cards there — Host, Model provider, Git
  host, Your SSH keys — each a radio group over concrete lanes, replacing a
  catalog of seven kinds plus a generic escape hatch and a 931-line Add dialog.
- **The integration verification-probe framework**: `POST
  /api/v1/integrations/{id}/test`, `IntegrationProbe`, `IntegrationProbeStatus`
  and the in-memory probe cache. Settings states what is STORED and says so
  plainly rather than dialing the provider; a real run is the real test. A
  `probe` key on a row stored under an earlier release is ignored, not rejected.
- **`POST /api/v1/integrations/{id}/adopt`.** Adoption promoted a derived row
  into a stored one so the catalog could edit it. A `PUT` onto a derived id used
  to answer 409 pointing at that route — a dead end once it was unregistered —
  so the write IS the adoption now, carrying the same audit event.

### Fixed

- **The live e2e suite no longer calls a deleted route.** `test/e2e/live`
  (`-tags docker`) still POSTed `/api/v1/runs/compose`, so its composer sub-test
  would have 404'd on the next run. It is daemon-gated and not part of
  `make ci`, so nothing caught it.
- **The Settings Git host card validates the host before storing a credential.**
  Without it a shape-invalid host stored its secret under `git-pat-<slug>` and
  only failed later when the `scm_hosts` write was rejected — leaving a
  credential saved under a name nothing would ever read.

- **~180 additional go-live findings** across the first-run/setup funnel, the
  new-run flow, workspaces, integrations, approvals, recordings, and the
  audit/policy/secrets screens — broken promises, misleading copy, dead ends,
  and **WCAG 2.1 AA accessibility** gaps (keyboard operability, `aria-current`
  /`aria-pressed`/`aria-label` on custom controls, theme-invariant contrast on
  the terminal player, and destructive-action confirmations). Documentation and
  threat-model claims were reconciled against the shipped code throughout.
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
