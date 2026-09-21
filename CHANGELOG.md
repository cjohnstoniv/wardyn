# Changelog

All notable changes to Wardyn are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); Wardyn is **pre-alpha**
and does not yet follow semantic versioning (interfaces are not stable).

**v0.3.1 and older live in [CHANGELOG-ARCHIVE.md](CHANGELOG-ARCHIVE.md).**

## [Unreleased]

## [0.7.9] — 2026-09-21

### Fixed

- **The egress proxy offered HTTP/2 it could not speak, on every install with a corporate CA.**
  `NewServer` built one `*tls.Config` for the corporate CA pool and shared it between the sidecar's
  control-plane client and the proxy's own transports. Enabling HTTP/2 on the first of those edits
  that config in place, adding `h2` to the protocols offered, so the egress transport — which had no
  HTTP/2 support — began offering HTTP/2 to every TLS peer. A peer that accepted the offer answered
  with HTTP/2 frames the HTTP/1.1 reader could not parse, and the request failed. This is what broke
  the AWS SSO lane behind `upstream_proxy_url` (`mode: sso-inject-proxy`) on a corporate-CA estate,
  where every re-originated request failed. Each transport now gets its own copy (#360).
- **The egress proxy could not talk to a TLS peer that speaks HTTP/2.** It now offers
  `h2,http/1.1` and uses HTTP/2 when a peer selects it. A peer that speaks HTTP/2 without
  negotiating it is recognised from its first bytes: that host is remembered for the run, and the
  request is resent over HTTP/2 rather than failing (#360).
- **An HTTP/2 answer was recorded as a dial failure and retried until the SDK gave up.** When a
  request cannot be completed over HTTP/2 either, it now carries its own
  `builtin:upstream-protocol-mismatch` rule source with a cause that names the mismatch instead of
  raw frame bytes, and it is answered with a 400 so SDKs stop retrying (#359).
- **The Agents tab and member Getting Started now render a chip for the `not_applicable` model-access
  state instead of nothing at all.** `not_applicable` — the admin-token principal's own answer under a
  per_user row, "this caller is a mechanism, not a person" — had no entry in `MODEL_ACCESS_CHIP_LABEL`
  and was excluded from both surfaces outright, so it read as unknown rather than as a deliberate
  answer. It now has its own neutral label (`Model access · Not applicable`), rendered on the Agents
  tab beside `ADMIN_OWN_CHIP_NOTE`, and beside member Getting Started's `llm_ready` fallback chip —
  still with no action and no sign-in CTA, since there is no person here to sign in as (#292).

- A request the egress proxy resends over HTTP/2 is rebuilt from its own source when it has one,
  so a write still finishing from the failed attempt can never interleave with the resend (#368).
- **A refocus that arrives while `usePoll` has a read in flight is no longer dropped.** The in-flight
  guard correctly stops a burst of focus events from stacking requests, but the refocus it swallowed
  was never retried, so a person returning to the tab mid-read got no refresh and kept seeing a stale
  screen for the rest of the interval — up to five minutes on the setup gate. The hook now coalesces:
  a refocus during an in-flight read is remembered and fires exactly one follow-up read when that read
  settles, however many refocus events arrived while it was outstanding (#314).
- **The compose file's writable-member-mount comment was wrong; `/srv/src` genuinely had no bind.**
  0.7.2 documented (and repeated in its own CHANGELOG entry) that
  `WARDYN_WORKSPACES_ROOT`'s `:ro` compose bind was what refused a writable member mount. It is
  not: that bind only bounds what wardynd's own container can see (a read), while
  `internal/runner/member_mount.go`'s writable check is pure root/deny-list matching, and the
  sandbox bind itself is created by the HOST dockerd straight from the source path — never through
  wardynd's mount namespace at all. A new test,
  `TestCreateSandbox_MemberMountWritable_ReadOnlySourcePermissionIrrelevant`
  (`internal/runner/docker/driver_member_mount_test.go`), proves it by binding writable against an
  unwritable source. The compose comment and the 0.7.2 entry are corrected in place rather than
  adding the `WARDYN_MEMBER_WRITABLE_ROOTS` volume the false claim implied was missing. Separately,
  `/srv/src` — the Linux member root `deploy/desktop/wardyn.env.m-prime.example` names alongside
  the macOS `/Users/Shared/src` — really had no bind: compose cannot expand a CSV into volume
  lines, so only whichever one path `WARDYN_WORKSPACES_ROOT` is set to gets bound. A commented
  example line now sits beside the existing bind, and `docs/ENV.md` states the limit (#135).
- **The decision-log line printed to stdout is now written under its own mutex.** A line over
  `PIPE_BUF` was not an atomic OS write, so two concurrent egress decisions on the request path
  could interleave into a corrupted stdout record. `decisionSink.mirror` now serialises the write
  with a dedicated `outMu`, held only around the write itself.
- **The first-use "approval pending" refusal body now spells it the same way as the header.** The
  JSON body wrote `approval_pending`; `X-Wardyn-Egress` wrote `approval-pending`. The body now
  matches the header's spelling, which is the wire contract `attach-bashrc` reads.
- **A panic in the audit webhook flush loop no longer takes the control plane down.** The one
  detached `go` statement that skipped the panic-safe wrapper (`buildAuditFanout`'s sink `Run`
  loop) is now started with `goSafe`, containing a panic instead of crashing the process. Its
  deliberate `context.WithoutCancel` lifetime — so the flush survives past request-tree
  cancellation on shutdown — is unchanged (#261).
- **A reaped never-dispatched run now carries a failure reason, not a blank chip.** `reconcileFinalize`
  finalizes stranded runs that were never dispatched, but only `failAndRevoke` used to write a
  `failure_hint` — so a reaped run rendered a FAILED badge with no reason. It now writes the
  reconciler's reason as the failure hint, best-effort, gated strictly on the transition landing on
  FAILED so a run reaching a successful terminal state through the same path gets no hint (#256).

### Changed

- The egress proxy offers HTTP/2 to TLS peers and uses it when a peer selects it, on every lane
  including the git and PAT brokers. It previously spoke HTTP/1.1 only (#360).
- `docs/OPERATIONS.md` states that `upstream_proxy_no_proxy` CIDR entries match destinations written
  as IP literals, never a hostname that resolves into the range (#361).
- **The Recordings screen pages instead of stopping at 1,000.** It fetched the whole run list in one
  shot (capped at `LIST_LIMIT`), so an install past 1,000 runs silently lost every recording beyond
  that window, with only a passive "truncated" note and nothing to press. `listRuns()` now takes an
  optional `limit`/`offset` and, when both are given, returns `{ runs, truncated }` off the server's
  own `?limit=&offset=` paging and its `X-Wardyn-Truncated` header — every other caller is unchanged.
  The screen fetches 100 runs at a time; a "Load 100 more" text link (matching the Runs board's own
  "Load N more") appears while more is known to exist, and a failed page keeps what already loaded
  with a Retry that resumes from the same offset. No total is ever shown — the server doesn't send
  one (#296).

### Security

- **`composer.Clamp` hands back a spec that owns its memory.** The clamped spec began as a shallow
  copy of the proposal, so every field the operator ceiling had no opinion on reached the caller as
  the caller's own backing array or pointee: `allowed_domains`, `denied_domains`, `allowed_methods`,
  `ui_apps`, `workspace_repos`, `tool_rules`, `workspace_mounts`, each eligible grant's `scope`
  bytes, and — whenever the proposal's sizes already sat inside the ceiling — the very
  `*ResourceLimits` the clamp exists to bound. A later in-place write through either side would
  have moved a ceiling the clamp had already enforced, in the widening direction, with nothing to
  notice; no caller mutates one today, which is a property of today's callers rather than of the
  function. `Clamp` now reallocates every reference field on the way out (`llm_inspection` and each
  mount's `read_only` pointee included, though the clamp replaces or drops those before they can
  reach a caller). What the clamp permits is unchanged — no allowed-or-denied outcome moves.

## [0.7.8] — 2026-09-19

### Added

- **Every `builtin:dial-failed` refusal now names why, and which hop.** `egress.DecisionLog` carries
  two new fields: `cause` — the masked, topology-redacted sentence naming the failed STAGE (a TCP
  dial, the origin's TLS handshake, the operator's own upstream-proxy CONNECT exchange) plus the
  underlying error, so "dial failed" on an `x509: certificate signed by unknown authority` is no
  longer the specific lie that cost this operator an hour — and `via`, the hop CLASS attempted
  (`direct` or `upstream-proxy`, never an address). Both ride through the SAME mask + topology-redact
  pass `Proxy.httpError` already applies to the sandbox-facing body, because `auditScope` hands a
  run's own creator this whole row, not just the operator. Rendered beside the decision chip in the
  run's Audit tab.

- **The configured LLM gateway's own guard refusal is no longer misfiled as a dial failure.**
  `vetTrustedHost`'s refusal of an operator-misconfigured internal model gateway (resolves to
  loopback/link-local/this proxy's own control-plane network, or does not resolve at all) used to
  share `builtin:dial-failed` with genuine network-lost-it dials, which also excluded it from
  `wardyn_egress_denies_total` alongside failures the network actually caused (an accepted residual,
  F065-gatewayvet). It now carries its own `builtin:gateway-vet-failed` rule_source and counts as an
  ordinary denial, like any other guard refusal.

- **A refusal on the run's own AWS SSO portal or a Bedrock endpoint now answers in a shape an AWS SDK
  can parse.** `Proxy.httpError` writes `text/plain`; on a TERMINATED (MITM) connection that body *is*
  the API response, so an AWS SDK hands it to its JSON parser — the operator's own
  `JSON Parse error: Unexpected identifier "llm"` is literally the first token of `"llm upstream
  error: …"`. The four sites this can happen at (`llm upstream vet failed` in `llm_routes.go` and
  `mitm.go`, `llm upstream error` in `llm_routes.go`, `llm credential refresh failed` in `mitm.go`) now
  detect the AWS lane (`isAWSLane`: the run's own `portal.sso.<region>.amazonaws.com` MITM entry, or a
  Bedrock endpoint — never a configured LLM gateway, and never Anthropic/OpenAI/a corp artifact
  mirror, which all keep today's plain-text body byte-for-byte) and answer with the modelled shape
  `writeSSOUnauthorized` already proved for a spent credential hold: `application/json`,
  `x-amzn-errortype`, `{"__type":…,"message":…}` carrying the same masked, topology-redacted sentence
  the decision log's `cause` field carries.

### Changed

- **Compatibility: an AWS-lane refusal's HTTP status changed from 502 to 500 or 401.** A dial-shaped
  or gateway-vet-shaped refusal on the run's own SSO portal or a Bedrock endpoint now answers 500
  `InternalServerException` (a modelled, RETRYABLE server error — the same bounded, backed-off retry
  either SDK already gives any 5xx, so a genuinely transient dial failure still gets retried, just
  against a body it can parse instead of one that crashes its deserializer) rather than the flat 502
  every refusal used to get. A credential-refresh failure on the same lane answers 401
  `UnauthorizedException` instead — modelled and NON-retryable, `writeSSOUnauthorized`'s own precedent
  for a spent hold — because retrying changes nothing when the credential itself cannot be resolved.
  Keeping 502 (a generic transport error both SDKs retry on an unparseable body) is what produced the
  reported operator's retry storm, ~20 attempts in seconds. Every non-AWS-lane host (Anthropic, OpenAI,
  a corp artifact mirror, a configured LLM gateway) keeps the unchanged 502 plain-text body.

- **The shipped confinement floor is CC1, and an unspecified run now defaults to the strongest
  installed class, not the floor.** `examples/policies/default.json`'s `min_confinement_class` moved
  CC2 -> CC1: on a stock, CC1-only install the old CC2 floor refused every default-policy run before
  it launched (`runner "docker" cannot enforce confinement_class CC2 (available: CC1)`), and the k8s
  Helm chart carried a render-time guard (B12b-F7) purely to catch the same trap early — both existed
  because the floor doubled as the default. They no longer need to: a run naming no `confinement_class`
  now resolves to the strongest class the runner actually advertises at or above whatever floor
  applies (the admin floor, when one is set, is unchanged and still fails closed exactly as before).
  An explicit request, the refusal path and the CC3 blast-radius override are unchanged to the byte.
  An *unspecified* request under an admin floor is not: it now resolves to the strongest installed
  class at or above that floor, where before it took the floor itself. Two server-authored lanes move
  with it — a source scan and an AWS SSO sign-in capture now dispatch at that same class rather than
  below the floor, and on a host that cannot meet an admin floor the sign-in is refused up front
  instead of failing in the driver after superseding the person's existing sandbox.
  **Residual:** on a host with gVisor or Kata installed, a person may now deliberately
  request a weaker installed class than the old CC2 floor allowed — an explicit choice only, never the
  default, which still always resolves to the strongest available. Note the console today always sends
  an explicit class, so a console launch records `requested`; the `defaulted` row is what an API or CLI
  launch that names no class produces. And because the advertised set is
  live-probed rather than read from a static field, a runtime that disappears between two runs lowers
  the *default* for the next unspecified request rather than refusing it — which is why the
  `run.create` audit row now carries `confinement_source` (`requested`/`defaulted`), the one place that
  distinguishes "the caller asked for this class" from "this is what today's runner had to offer." See
  `threatmodel/THREAT-MODEL.md` residual #47.

- **`WARDYN_AWS_SSO_PROXY_INJECT` (the Phase B kill switch) is now reachable the way
  `docs/OPERATIONS.md`'s "Turning the lane off" already told operators to use it.** It was a named env
  var read at boot, but reachable only through the Helm chart's generic `.Values.env` passthrough (no
  `values.yaml` entry of its own) and absent from the Compose stack's explicit `WARDYN_*` env list
  entirely — an operator following either install path's own documented rollback recipe for a corp-MITM
  or SDK surprise had no chart value or compose var to set. The chart gains `awsSSOProxyInject`
  (`deploy/helm/wardyn/values.yaml`), wired into the Deployment behind the same precedence `trustedCA`
  already established: a raw `env.WARDYN_AWS_SSO_PROXY_INJECT` still wins, so
  `scripts/kind-sso-walk.sh`'s existing `--set env.WARDYN_AWS_SSO_PROXY_INJECT=…` posture pin needed no
  change. Compose gains the `WARDYN_AWS_SSO_PROXY_INJECT` passthrough beside its sibling `WARDYN_*`
  vars. Both empty by default — byte-identical to today, wardynd's own compiled default (`on`) applies.

- **The console offers only the barrier classes a run can actually use, and an untouched pick
  defers to the server instead of guessing one.** Availability now comes from `/setup/status`'s
  `runner.confinement_classes` everywhere — New Run's Barrier control moved off `/healthz` (a wire
  mirror with no other consumer), matching the Getting-started/Settings barrier matrix it already
  shared. Every selector disables an installed-but-below-floor or plain-uninstalled class with its
  own reason, and where exactly one class qualifies for a run there is nothing to ask, so the control
  collapses to a sentence rather than a picker. An inconclusive read (`unreachable`) never blocks
  launch and no longer leaves every tier guessably selectable either. The per-browser
  `wardyn-default-confinement` localStorage default is gone (its HIGH-4 downgrade guard was already
  inert): the default is a server fact now, so New Run resolves its own from the host instead of
  reading a stale browser preference, and Settings' Host card lost the private per-mount override
  state that used to write to it.

- **A console launch now records `confinement_source: defaulted` when the Barrier control was never
  touched.** New Run used to send an explicit `confinement_class` on every launch — including runs
  where the person never touched the Barrier control — so the server's own strongest-installed-
  at-or-above-the-floor default (`runs_policy.go`'s `strongestAdvertisedAtOrAbove`) never actually
  applied to a console launch, and the run-create audit's `confinement_source` field could only ever
  read `requested` from this surface. An untouched pick (a clone's carried-over class still counts as
  touched, B4b) now omits `confinement_class` from the request entirely.

### Fixed

- **The AWS sign-in tab's URL no longer carries junk after the device code.** Clicking "open AWS
  sign-in" opened a tab whose `user_code=` had escape bytes appended — the login PTY's extractors
  excluded whitespace and quote/bracket characters from a URL but not control bytes, and a tmux
  redraw's cursor-addressed escapes butt directly against the URL with no delimiter between them.
  Both `extractAuthUrl` and `extractDeviceVerificationUrl`
  (`ui/src/app/components/screens/settings/login-pty-extract.ts`) now exclude C0/DEL control bytes
  from the URL body, and the device-URL extractor prefers the longest `user_code=` candidate seen in
  the buffer rather than the first, so a CSI landing mid-code (now itself a match boundary) can no
  longer return a silently truncated code instead of visible junk.

- **`wardyn attach` no longer needs the shared admin token for a member's own run.**
  It dialed the attach WebSocket directly with whatever bearer was configured, and that
  route falls through to `requireOperator` for a bare bearer — so a member got a blanket
  **403** and the shared admin token was the only credential that ever worked. The CLI now
  mints a single-use, 30s-TTL attach ticket first (`POST /runs/{id}/attach-ticket`,
  owner-or-admin — the same door the console's own embedded terminal already uses) and
  dials with `?ticket=` instead, so a member's own token (`WARDYN_TOKEN`) now works for a
  run they created. The ticket is minted immediately before the one dial and never cached:
  a fresh mint per attach attempt, matching the ticket's single-use, 30s TTL contract
  (`internal/api/attach_ticket.go`). An admin-token caller (e.g. CI) is unaffected — the
  mint endpoint authorizes an admin on any run — and when no ticket can be minted at all
  (an older control plane with no `attach-ticket` route to answer), the CLI falls back to
  dialing directly with the configured token exactly as before this ticket lane existed.
  (`cmd/wardyn/attach.go`; no server change — `internal/api`'s ticket lane already served
  the browser.)

  **Audit shape change:** a non-owning member's `wardyn attach` used to be refused by the
  WS route's `requireOperator` gate with a blanket **403** (`authz.denied`), before the run
  was even loaded — no existence check at all. It now mints first, through the same
  owner-or-admin door the browser uses, and for a run the caller does not own that mint
  itself now refuses with the byte-identical **404** `authz.denied` (reason `not_owner`) a
  nonexistent run would (`getRunAuthorized`'s no-existence-oracle rule) — the WS is never
  dialed. Anyone alerting on the old blanket-403 `authz.denied` row for CLI attach should
  also watch for this 404 shape.

- **The run-detail "Attach from your terminal" card no longer claims the CLI needs the
  admin token.** Its copy said `wardyn attach` "needs your admin token in
  `WARDYN_ADMIN_TOKEN`" — false as of the fix above, and a member reading that card had no
  reason to believe the command would work for them at all. It now names both `WARDYN_TOKEN`
  (a member's own) and `WARDYN_ADMIN_TOKEN`, and that it mints the same one-time ticket the
  page's own terminal does. (`ui/src/app/components/screens/run-detail-ssh.tsx`)

- **The browser terminal sometimes needed a double click to type, sometimes only accepted a click in
  one area, and sometimes never let you click back in.** Three separate causes, each with a pinning
  test:
  - A holder whose socket died silently (a dead peer on a quiet shell) could hold the writer slot
    forever — nothing in the attach pump bounded it, since the existing 30s write timeout only ever
    engages while output is flowing. The attach WebSocket now probes an otherwise-idle connection with
    a WebSocket ping and frees the slot if the peer never answers (`internal/api/attach.go`).
  - Toggling focus mode remounts the terminal (by design), and the old socket's holder slot was
    released only after its recording was persisted and its `session.detach` audit was written — a
    window in which the new socket's handshake was admitted read-only against its own vanishing
    predecessor. The slot now frees the instant the old pump ends, before that tail
    (`internal/api/attach.go`).
  - The terminal never called `.focus()` at all; focus depended entirely on xterm's own click-to-focus
    on its inner canvas, so a click on the container's padding or the space below the last row landed
    nowhere. A click anywhere in the terminal, or a socket becoming writable, now focuses it
    (`ui/src/app/components/attach-terminal.tsx`).
An operator's blocking field report: on a private-endpoint Kubernetes estate reaching AWS through a
corporate proxy, the first model call starved and the agent showed
`API Error: SyntaxError: JSON Parse error: Unexpected identifier "llm"` — the giveaway that the
proxy's own plain-text 502 body ("llm upstream vet failed: …") was handed straight to a JSON parser.
The audit trail showed a tight loop of `policy:allowed` → `builtin:dial-failed`, with no way to tell
*why* the dial failed. It cost the operator an hour.

- **`builtin:upstream-proxy` is an allow, not a refusal.** The console's rule_source table folded it
  into the generic "Refused by the built-in guard" bucket, which reads as a denial that never
  happened — it is recorded once per run, at proxy construction, to audit the deliberate SSRF-guard
  relaxation for the operator's configured upstream hop. It now renders as "Corp upstream proxy in
  path" (an allow).

- **The setup gate is a daemon decision now, not a console id list.** `/setup/status` rows carry a new
  `blocking` bool (`SetupCheck.Blocking`), and the console's hard gate (`setupGateActive`) redirects into
  the funnel only when a row is marked, never on grade or id alone. 0.7.7 twice tried to name the rows
  that must not gate — first `llm_provider`, then `bedrock_provider` — and both times it was enumerating
  a family rather than naming the property: the next lapsed sign-in graded `harness_credential_aws`, an
  id neither list contained, and an admin whose own AWS SSO session had expired was pulled off New Run
  onto Getting started in the middle of repairing it. A console cannot know which rows describe the
  install and which describe the person reading them; the daemon can, and now says so.
  Exactly three rows are marked: `runner`'s `fail` (no live confinement class — runs cannot launch),
  `confinement_floor`'s `warn` (every run on the default policy refused before it launches), and
  `sso_rbac`'s `warn` (OIDC configured with no role mapping — every SSO user is an admin, and the
  funnel's People step is where that is fixed). Every other row — `age_key`, `tls_cookie_posture`,
  `site_config`, `scm_provider`, `host_proxy`, `claude_subscription_staging`, `github_ref_ruleset`,
  `k8s_egress_containment` (its `warn` **and** both `fail` arms), the install-wide
  `harness_credential`, and the per-person `llm_provider`, `bedrock_provider` and
  `harness_credential_aws` — keeps its grade on every surface that renders it, and never confiscates
  the console again, whatever status it carries.
  Two consequences worth stating plainly: a `fail` row now stops gating (the unenforced-CNI arm of
  `k8s_egress_containment`, a posture an operator turns on deliberately), and an install that has a
  runner, a meetable floor and a role mapping sets none of the three — so on a healthy Kubernetes
  install the funnel gate no longer fires at all, where before 0.7.8 any `warn` held it. The rows are
  still there to read; what they stopped doing is taking the console away.

### Known gaps

- **The corp-proxy estate's actual cause is still unidentified, and this release does not claim to fix
  it.** The field report diagnosed Phase B's MITM lane as ignoring `SiteConfig.upstream_proxy_url`;
  a new test now proves that lane *does* CONNECT through the corporate proxy, by hostname, and that a
  `upstream_proxy_no_proxy` entry is the one thing that sends it direct. So the premise is disproven
  and the real cause is one of: a bypass entry covering the AWS suffixes, the corp proxy refusing
  `CONNECT portal.sso…:443`, or the proxy image lacking that estate's TLS-intercept CA
  (`WARDYN_TRUSTED_CA_FILE` — the sandbox images bake one, the proxy image does not unless staged).
  What 0.7.8 ships is the instrument: every `builtin:dial-failed` now carries the failed stage, the
  underlying error and which hop was attempted, so the next run names its own cause instead of costing
  an operator an hour. `WARDYN_AWS_SSO_PROXY_INJECT=off` remains the sanctioned stopgap.

- **An observer learns it may type on reconnect, not in place.** A holder whose socket dies is now
  reaped within the ping budget instead of holding the writer slot indefinitely, and a remount no
  longer collides with its own release — so the next attach gets the slot. But the `attach-mode` frame
  is still sent once per connect: a terminal already sitting open as a read-only observer does not
  flip to writable live, it flips when it reconnects. True in-place promotion needs the holder registry
  to track observer sinks rather than one writer, which is a larger change than this release took.

- **The terminal's ordinary-use test corpus is not written.** The three reported faults ship with pins
  that fail without their fix, but the wider corpus this campaign scoped — paste, selection, two tabs
  on one run, a reconnect after a daemon bounce, the SSH lane joining the same session — is deferred,
  as is k8s `Driver.Attach`, which has no test at all.

- **Two console surfaces still describe the old setup gate.** The Getting-started Review step groups
  rows by grade rather than by the new `blocking` flag, so a blocking `warn` reads under "Worth a
  look" while a non-blocking `fail` reads under "Blocking"; and demo 02c's narration still says every
  door leads to Getting started, which a healthy install no longer does.

## [0.7.7] — 2026-09-18

The 0.7.6 field report, in one journey: an admin on a Kubernetes estate whose AWS SSO session had
lapsed clicked Launch on New Run, the run never started (the walk found both shapes: refused at the
click, or launched and failed at dispatch when the refresh token was already retired), and the console
loaded Getting started. This release answers both halves.

Verified on the kind AWS SSO walk against images rebuilt from the commit under test (walk-4 on
`e73f2c3d`, the tree this release descends from — only one live spec, case J, and this changelog change
after it; the walk's `MANIFEST.json` records the tip and the rebuilt images): a member whose own sign-in
has lapsed clicks Launch, the sign-in dialog opens itself over New Run, and the same click's run
launches after the device flow (live case L); an admin with no usable session of their own loads New
Run and stays there (L0); a session retired at the portal is refused at the click with no run created,
and the dialog opens where the person is (J, rewritten for the click-time refusal and run, with the
hold spec's two runnable negatives, against the same install); the mid-run hold and its resume are
unchanged (K, K(resume)).

### Fixed

- **A lapsed AWS SSO session no longer sends an admin to Getting started.** The setup funnel's
  redirect read every `warn` on `/setup/status` as an install defect — including the two
  model-provider rows, `llm_provider` and `bedrock_provider`, which under a `per_user` Bedrock row are
  graded through the CALLER's own session — so an admin whose own sign-in had lapsed was pulled off
  New Run (or any page) the moment the shell's next status read landed (on the owner's estate, seconds
  after they had clicked Launch), on an install that had never marked onboarding complete. The model provider is optional and
  per person; the gate no longer reads either row. Both keep their grade everywhere they are
  rendered. (The 0.7.6 field report; the kind walk's admin had been landing in the funnel on every
  load for the same reason.)
- **Launch with a lapsed sign-in is one dialog, then the run.** `POST /runs` still refuses a run whose
  per-person session is missing or spent before any run exists, and the refusal now names its class
  on the wire — `"reason":"model_credential"`, the failure audit row's own word; every other error
  body is byte-identical. The real launch now also REDEEMS an expired session whose refresh token
  still lives, right there at the click (Review's preflight stays a dry run): a renewal AWS refuses is
  refused before any run exists, with the class — it used to be admitted and fail at dispatch, on the
  run's own page, after the person had been told it launched. New Run answers that refusal by opening
  the AWS sign-in dialog itself and launching the run again — the form as it then stands — the moment
  the capture lands: no trip to Getting started, no second click. Escape leaves the sentence and the rail's own sign-in control; a member under a shared row
  reads the sentence and no dialog, because the repair is the admin's; a relaunch refused again (a
  pin contradiction the same identity cannot repair) shows the sentence and waits. Launch stays a
  server decision — nothing is pre-checked on the console's cached status. Review's preflight 422
  carries the same class.

### Known gaps

- **A renewal AWS does not answer is refused without the dialog.** When the token in hand has lapsed
  and AWS is throttling or unreachable at the click, the launch is refused with *"launch again in a
  moment"* and no sign-in dialog — the sign-in is still good, and a device flow repairs nothing about
  an outage. A run whose renewal fails at dispatch (a token that lapses between the click and the
  sandbox) still fails on its own page — the sentence alone when AWS did not answer, the 0.7.6 sign-in
  door when the session is spent; neither launches it again from there — "Start a run like this one"
  in the run header is the way back (New Run, prefilled).
- **A relaunch armed by the dialog survives leaving New Run with the dialog open** (the dialog is the
  shell's, so Back does not close it): a sign-in completed afterwards launches the run that click asked
  for. It lands on that run only when the launch carries no advisories; a launch WITH advisories holds
  for an "Open run" nobody is there to click, so the run is on the Runs board and nothing says so — and
  a relaunch refused again after leaving shows its sentence nowhere.
- **The hermetic suites prove the dialog opens itself (Playwright) and that Escape launches nothing
  (vitest); the relaunch after a completed sign-in is proven on the kind AWS SSO walk only** (live
  case L) — no mocked spec completes a device flow.

## [0.7.6] — 2026-09-18

The 0.7.5 field report (an operator's handoff from running 0.7.1–0.7.5 on a private-endpoint Kubernetes estate) consolidated one journey — a person getting
AWS SSO working and running a Claude Code agent — into eight findings. This release answers seven of
them; the eighth (a mid-run credential lapse holding the run instead of killing it) ships behind a
kill switch, sequenced last — see Known gaps.

Verified on the kind AWS SSO walk against images rebuilt from the commit under
test, on two fresh installs (walk-9 on the spec set that ships here, then walk-10 on
`3eaa42db`, the tree this release descends from — only documentation and the release commit's
own version strings change after it; each walk's `MANIFEST.json` records the tip, zero dirty
files, the rebuilt images with host and node digests agreeing, and the kill-switch
posture, `on`). A never-signed-in member is told on the Runs board and on New Run
(findings 2 and 1); a lapsed member signs in from the strip itself without leaving
the page or reloading it; a start held unscheduled names scheduling rather than a
pull, and a start on an unpullable image ends in seconds with the registry's own
words (finding 6); a session retired at the portal mid-run HOLDS the model call and
the SAME run continues after the sign-in (finding 4); a dispatch refused over the
model credential carries the sign-in on the run's own page (finding 3); an admin
whose own session is live sees no strip; killing a held run cancels its sign-in
request. One live case is deferred with its measurements — a hold nobody answers
timing out — because the two attempts on this release asserted the decision before the
budget's next observer wrote it; the case is drivable with a post-expiry prompt and is deferred for
walk time, not for a product limit (see TEST-GAPS). The proxy's own tests pin the timeout path.

### Added

- **A global model-access banner.** An actionable model-access state — not signed in, lapsed, or
  lapsing within 24 h — now rides a strip on every screen, in focus mode and on the run cockpit,
  carrying the sign-in itself in a dialog; a lapse cannot be dismissed, and the first-run state and —
  for a non-operator — a dead shared credential can be set aside for the session (an operator's own
  dead shared credential is actionable and undismissable: their sign-in is the repair). Suppressed on
  Getting Started, which is the door itself; withheld on Settings and Providers for an OPERATOR only,
  since those pages already mount the same sign-in for them — a member keeps the strip there, because
  the Settings card's AWS button is admin-only. Read once per session plus a 5-minute
  visibility-aware poll; no new endpoint; one new wire field, `deadline`. (Finding 2.)
- **A run refused for a model credential now offers the sign-in, not directions to it.** The dispatch
  refusal's audit row carries `reason: model_credential` (and the run's declared `mechanism`), the
  console grades that ending `credential`, and the run's failure block shows the server's own sentence
  with the AWS sign-in beside it — in a dialog, on the run page. The button appears only where a
  sign-in this person can complete repairs the state: a member under a shared credential gets the
  sentence and no button, a refusal whose renewal merely did not complete ("launch again in a moment")
  gets no button either, a refusal of a run declared on another provider's lane gets none, and a run
  somebody else created gets none. Relaunch is the header's existing "Start a run like this one".
  (Finding 3.)
- wardynd's own outbound calls (OIDC discovery/JWKS, audit webhooks, GitHub App token minting, AWS SSO
  `CreateToken` renewal, Entra directory sync) can now follow a corporate proxy through
  `WARDYN_DAEMON_PROXY_URL` (+ `WARDYN_DAEMON_NO_PROXY`), without the process-wide blast radius of
  `HTTPS_PROXY` — the standard variables also get read by the Kubernetes client and by every library
  that calls `http.ProxyFromEnvironment`, so a wrong `NO_PROXY` there can take the control plane's own
  API access with it. (Finding 8.) Unset leaves `http.DefaultTransport` untouched — byte-identical to
  today. `KUBERNETES_SERVICE_HOST`, the `WARDYN_AWS_SSO_ENDPOINT_OVERRIDE` host, and the
  `WARDYN_OIDC_INTERNAL_ISSUER` host are auto-bypassed. A malformed value, or one carrying
  `user:pass@`, refuses boot rather than silently falling back. See `docs/ENV.md` and
  `docs/OPERATIONS.md` "wardynd behind a corporate proxy".
- A captured AWS SSO session whose refresh token AWS has already retired now grades `expiring` with a
  sign-in action the moment Wardyn learns it, instead of reading `live` until the client registration
  lapsed — days later, while every dispatch in between was already refusing the person's runs. (Finding
  5.) The deadline named is the moment dispatch itself stops serving the access token (`ExpiresAt −
  awsSSORefreshSkew`), not the registration's own lapse. Refresh failures now record how many attempts
  were made (`attempts: 1` or `2`) beside the errors — two attempts failing is a network story, not a
  credential one; the "one dropped packet" the field report described was actually two attempts, the
  retry that already existed. `GET /metrics` gains `wardyn_sso_refresh_total{outcome}`
  (`success`/`spent`/`transport_error`/`unavailable`).
- **A run that is slow to start now says what it is waiting on.** The kubelet's
  own reason (pulling an image, waiting for a node, a reference that will not
  pull) reaches the run header, the Runs board and the sign-in pane, and a
  terminal reason ends the wait immediately instead of after five minutes. The
  sign-in pane no longer grades a start purely on a clock. (Finding 6.)
  (`0063_agent_runs_status_detail` adds the `agent_runs.status_detail` column the
  substrate's reason is written to; it is blanked at read for any run that is not
  STARTING, except a run that FAILED on a terminal reason.)
- **A captured AWS SSO session that lapses mid-run now HOLDS the agent's next
  model call while its owner signs in again, instead of failing the run.** The
  SSO access token is no longer written into the sandbox on that lane (Phase B):
  the sandbox's token cache carries an inert placeholder and the proxy injects
  the real token as `x-amz-sso_bearer_token` on the run's own
  `portal.sso.<region>` host; the short-lived role credentials the SDK mints from
  it still are resident. A lapsed session raises a visible `credential_reauth`
  approval, the proxy parks the call for at most
  `WARDYN_CREDENTIAL_REAUTH_TIMEOUT` (default 600s, clamped `[10s, 1800s]`), and
  the sign-in capture resolves it; on expiry the call fails exactly as it did
  before this existed. The whole lane is behind `WARDYN_AWS_SSO_PROXY_INJECT`
  (`on` | `off`) — `off` restores the 0.7.5 behaviour for new dispatches.
  (Migration `0064_approval_credential_reauth` adds the approval kind; a
  downgrade to 0.7.5 with such rows present is unsupported.)
  A deployment with no agent roster at all now completes this lane instead of refusing every resolve
  with "the roster changed" — there was nothing to have drifted from. The proxy's injected session is
  now pinned to `GET /federation/credentials` with the run's own dispatch-time account and role
  whenever the captured session names both, so it can no longer be ridden onto a different account or
  role, or onto `POST /logout` (which invalidates the person's sign-in session, so every run of
  theirs fails at its NEXT credential exchange — already-minted role credentials keep working until
  the permission set's own duration expires); a request the pin does not cover is forwarded with no
  credential rather than refused. And the
  MITM'd tunnel to a plain-HTTP `WARDYN_AWS_SSO_ENDPOINT_OVERRIDE` now serves a client that speaks
  plain HTTP inside the tunnel, not only one that speaks TLS — the agent's own SDK does the former,
  which was the walk's real blocker.

### Fixed

- **New Run's model warning is about the person, not the deployment.** It was keyed on a
  deployment-wide integration count — true the moment an admin saves a `per_user` roster row — so a
  member who had never signed in filled in the whole form and was refused at the click, and the one
  sentence that would have warned them said *"No model provider is connected"*, which on that
  deployment is false. The rail now reads `model_access` when the selected agent is the one it grades
  and states, in its own words, which of the four per-person states the launcher is in — carrying the
  server's own action text alongside only where it says something the sentence and button cannot (a
  pin-contradicted account/role pair). The deployment-level warning is unchanged. (Finding 1.)
- **Signing in opens its own tab.** The tab is opened on the click that starts the sign-in — the only
  user gesture in the flow — and navigated to the verification page when it appears, so browsers no
  longer block it. If your browser blocks it anyway, the link on the panel still works and now says so.
  Wardyn closes it while it is still the placeholder (Cancel, an error, a completed sign-in); once it
  has navigated to the provider's own page, that page is the tab's end state — the browser will not let
  Wardyn close a cross-origin tab it deliberately gave up its handle back into. (Finding 7a.)
- **The sign-in panel now says when you are signed in** and Wardyn is waiting on the sandbox to hand
  over the session — including when the sandbox is asking which AWS account or role to use, which it
  does in the terminal on that panel. If the sandbox's own report and the server briefly disagree, the
  panel keeps saying it is checking (with a way to cancel) instead of refusing on the spot — it only
  refuses once a background check has genuinely given up. And the panel closes itself as soon as the
  server confirms the session was stored, whether or not the sandbox's own success message reaches the
  browser at all. (Finding 7b.)
- **A sign-in dialog dismissed from outside no longer orphans its login run.** Escape or an overlay
  click on the model-access door now routes through the login pane's own cancellation, and a
  dismissal that lands while the launch POST is still in flight kills the run that POST created —
  before, either left a "wardyn: sign-in running" sandbox on the person's board for up to 30 minutes.
- **One spelling for the AWS sign-in.** The Settings dialog said "Sign in with AWS SSO" beside a
  control named "Sign in to AWS"; both are now `MODEL_ACCESS_BANNER.DIALOG_TITLE`.
- **Signing out drops the console's cached setup snapshot.** It held one person's readiness, harness
  roster and model-access state until the next `/setup/status` answered, so the next sign-in on that
  tab rendered the previous person's state for a beat — and a read still in flight across the
  sign-out was stored afterwards.
- **The model-access deadline is shown on the reader's clock.** `Sign in again before
  2026-09-19T14:03:22Z` (RFC3339 UTC) is now rendered through the viewer's own locale on the
  member's Getting Started and the admin's Agents tab, and as "in 3h" — with the absolute stamp as
  the element's title — in the strip.
- **The held-run sign-in door is offered only to the person whose sign-in can resolve it.** A mid-run
  AWS sign-in request is cleared by the subject the row names — a capture lands in the capturer's own
  scope — so the cockpit row and the `/approvals` card now show the button only to that person (or,
  on the shared lane, to an operator, whose credential it is). A shared-lane member and an admin
  reading somebody else's held run each read a sentence naming whose sign-in is awaited, and no
  control. The board card and the cockpit header say "Waiting for the owner's AWS sign-in" to the
  same readers, instead of calling it theirs.
- **A login sandbox that gets stuck on a terminal reason closes its placeholder tab.** The tab the
  click opened says the page changes to the provider's sign-in page by itself; on an
  `ImagePullBackOff` it never would, and the error sat on the tab behind it.

### Changed

- **The model-credential refusals now name a door the reader can actually open.** Under a `per_user`
  row the dispatch refusal, the create-time 422, the stored-AWS-identity refusal and the
  spent-renewal sentence all end on "sign in to AWS from Getting started in the console, or from the
  sign-in banner the console shows on every page" instead of "sign in again under Settings → Model
  provider" — that page's AWS button is admin-only, so the sentence had been sending the one person
  who could repair their own captured session to the one page that will not let them. Under a
  `shared` row the Settings destination stays (it is the admin's own door), and the arm where nothing
  ever credentialed the run drops "again".
- The captured-AWS-SSO egress allowlist now carries the test endpoint override's **port** beside its
  bare host. Only the injecting lane reads it; a bare entry already matched any port, so nothing is
  newly reachable. Without it the cleartext kind-walk lane was allowlisted, MITM-less and silently
  UNCREDENTIALED — the SDK saw the fake's own 401 and nothing in the proxy said why.

### Retired (from 0.7.5's Known gaps)

- *"The New Run rail still paints its credential chip from the ROSTER ROW alone"* — the rail now also
  reads the shared `model_access` door for the selected claude-code agent; the roster-row credential
  chip's own meaning (where the credential lands) is untouched.
- *"A member on a shared Bedrock row whose ADMIN credential is expiring is still offered the
  member-side sign-in CTA"* — this described a WORKING path, not a defect: `authorizeHarnessLogin`
  (`internal/api/harnesscred.go:708-710`) returns early for ANY operator before the `!perUser`
  refusal, and the Agents tab mounts the pane with `startURLManaged={false}` for a shared row. 0.7.6
  makes that explicit: the shared-dead state is audience-aware — the admin gets the sign-in that
  repairs it, a member gets the instruction and no button.

### From the 0.7.x readiness review

Independent documentation/security-lane patches from the owner's separate `review/0.7x-ledger`
readiness campaign, cherry-picked individually onto this release (the campaign's own ledger is kept
on its private review branch; none is a schema, configured-cap or deployment-requirement change; two
tighten an existing input check and one turns a relayed oversized upload into a 413 — see below).

- Keep contributor UI commands at repository root.
- Clarify automated conformance versus manual live acceptance gates.
- Select a patch version before preparing and validating its release.
- Correct when OIDC email-verification restrictions are enforced.
- Explain how a mismatched age key can prevent restored daemon startup.
- Describe SSH shell masking and recording retention accurately.
- Document SSH key removal and run shutdown separately from token revocation.
- Preserve undrained audit fallback state during backup and recovery.
- Document the console's existing reduced-motion support.
- Correct the recording pane's permission hint to include security admins.
- Reject oversized brokered uploads instead of forwarding truncated data.
- Prevent multiline and aliased Compose credentials from leaking in
  support bundles; omit comments/unparseable config and require review before sharing.
- Preserve dialog and sheet backdrop exit animations by forwarding their DOM refs.
- Make the run-detail test's lifecycle-hook import explicit.
- Confine filesystem recording replay and metadata reads
  to the configured recording directory, including symlink resolution.
- `run recording -o` and `support-bundle` now write through a private temp file; the finished file is
  owner-only (0600).
- `--policy-file` / `policy create|update|render -f` reject a file with more than one YAML document (a
  second `---`, including a trailing one) instead of silently applying only the first.
- Mask uploads only while storage reads — pins masking/tail-before-error/no-leak-on-abort behavior;
  no behavior change for the shipped upload path (only the brokered proxy path, which already reads the whole
  body before forwarding, ever calls this handler in production).
- Fixed: `FSStore.SaveCast` no longer leaves its own `.tmp-cast-*` temp file behind when the final rename fails
  (disk-hygiene fix, no behavior change on any success path).

### Known gaps

- **Finding 4 (a mid-run credential lapse holding the run instead of killing it) ships behind
  `WARDYN_AWS_SSO_PROXY_INJECT`, on by default**; the kill switch `WARDYN_AWS_SSO_PROXY_INJECT=off`
  is the rollback, restoring the 0.7.5 behaviour for new dispatches. A docker-gated measurement
  against the reference agent's own SDK found it still waiting on a parked credential exchange at
  eleven minutes — the test's own ceiling, not the SDK's — so the 600 s default hold is the binding
  constraint, not the SDK; the docker-gated resume test
  (`TestDocker_TheSameRunResumesWhenTheHoldReleases`) has run green on this release's lineage (~22 s).
  The live kind SSO walk found that Phase B's TLS-MITM entry for the portal host could not serve a
  plain-HTTP SSO endpoint (`WARDYN_AWS_SSO_ENDPOINT_OVERRIDE`, the kind test estate's own posture):
  terminating the CONNECT is mandatory — the sandbox holds only an inert placeholder, so the real
  token can only be substituted into a request the proxy can see, and a blind tunnel carries the
  placeholder through untouched — but the proxy re-originated every terminated tunnel as `https://`
  regardless of the origin's own scheme. Fixed: a plain-HTTP SSO endpoint override now re-originates
  the MITM'd tunnel in plain HTTP too; every other entry — including the real
  `portal.sso.<region>.amazonaws.com` — stays unprefixed, i.e. TLS, byte-for-byte as before.
  Production is unaffected: a real portal serves TLS and was never on the affected path.
- **A PENDING `credential_reauth` row is not proof a model call is still parked.** The row outlives
  the hold on purpose (the sign-in is still wanted), so it survives a hold that timed out, an SDK
  that disconnected and a final resolve that refused a roster drift. The console's copy says only
  what the row proves; the audit trail (`credential.reauth.requested` / `.resolved`, and the
  `credential:reauth-timeout` decision row) is what distinguishes the three.
- **The sign-in supersede race is wider now, and still open.** A visible re-auth door (the
  model-access banner mounts outside the router and can raise a sign-in mid-run, from any page) makes
  concurrent sign-ins likelier. 0.7.5's `run_killed` refusal inside the owner lock already makes a
  late capture lose whenever a pass saw both sign-ins. It does not cover the case where the two rows'
  timestamp order and insert order disagree: there neither run is KILLED, both stay live, and the
  later of the two uploads wins regardless of which sign-in is newer. The per-person advisory lock
  stays 0.7.7 (O-1).
- **A sign-in started from the Settings, Agents or Getting Started pane and then navigated away
  from, or whose browser tab is closed, leaves its login sandbox running to the 30-minute idle cap;
  the capture itself still lands.** The sign-in opened from the model-access strip is unaffected —
  its pane outlives a route change and ends the sandbox once the server confirms the capture. There
  is no CLI sign-in path.
- **A sticky terminal hold still costs one control-plane resolve per retry.** Once a hold has ended,
  the sidecar cannot know which approval id a later 423 names without asking, so each post-expiry
  retry makes one injection resolve — a broker mint and a `credential.mint` audit row — before the
  coordinator hands it the terminal result at once. What the hold removes is the second WAIT, the
  second counted workflow and the repeated decision row, not the round trip. Inherent to the design
  and accepted.
- **`wardyn_credential_reauth_total{outcome="cancelled"}` can overcount by one.** The run-kill cascade
  counts this run's still-PENDING re-auth rows just before cancelling them, because after the cancel
  there is nothing of the kind left pending to count. A row decided by its owner in that window is
  counted as cancelled as well as resolved. Metric-only, needs a sign-in landing inside one cascade,
  and closing it means a per-kind return from `CancelForRun`. 0.7.7.
- **Masking has no TTL.** Each resolve registers the session token in the run's own mask set (evicted
  for terminal runs past `RunSecretGrace`) and the renewal path keeps its global registration, which
  has no expiry. Unchanged from 0.7.5, restated because this lane adds a registration site.
- **The kind SSO walk's case K exercises the real path end to end — nothing in it is simulated.**
  The terminate → strip → inject → re-originate path is pinned by `internal/egress/proxy`'s own
  tests (`TestMITMConnect_PlaintextClientInsideTheTunnelIsServed` for the client leg,
  `TestForwardInspectedLLM_ReOriginatesInTheSchemeTheEntryNames` for the origin leg), not by the
  docker-gated SDK-tolerance test, whose fake also serves plain HTTP with no proxy in the loop at
  all. The hold's own sentence reaches a client that speaks plain HTTP inside the terminated tunnel
  (the walk's SDK does); only a client on the un-terminated plain lane would see the origin's own
  401. Listed here so the walk's plaintext client leg is not mistaken for a simulated one.
- **The support bundle's Compose entry is redacted for reading, not for re-use.** It is not a valid
  `docker compose -f` input when marker-named structural keys exist (e.g. a `secrets:` section or a
  `*_token`-named volume) — those keys are redacted whole rather than per-value, so the redacted
  document parses as YAML but fails Compose interpolation/validation on that section.
- **An ABSOLUTE symlink inside the recording root now refuses to read** — replay answers 500 and the
  runs list degrades to "no recording" for that entry; hard links inside the root still read (an OS
  boundary, not covered by this fix). NFS behavior of the underlying `os.OpenInRoot` is untested (no
  NFS share available in review); no reason from the API contract to expect a difference from the
  tested local/drvfs cases.
- **Finished `run recording -o` and `support-bundle` exports are owner-only (mode 0600) on Linux
  only** — on WSL drvfs (and likely native Windows) the filesystem ignores the mode bit, so the
  tightening has no effect there (probed: files land `-rwxrwxrwx` regardless).
- **A policy file ending in a bare `---`** (a legal, empty second YAML document) was silently accepted
  on the first document alone; it is now rejected with "policy input must contain exactly one
  document" — the one behavior change on a previously-accepted policy file; no shipped example policy uses a
  document separator.
- A member under a dead SHARED model credential is told, and has nothing to do about it: Wardyn
  offers no way to notify the admin from that strip. The sentence names the admin because they are
  the only repair; "Not now" is the only control the member gets. The wire cannot distinguish "never
  captured" from "captured and now dead" for a `shared` row, so the same "no longer works — sign in
  again" sentence renders for both, including to the admin who never signed in at all.
- The §7.4 frozen copy table (`docs/design/workspace-providers-prompt.md`) and its byte-parity twin
  `PROVIDER_MEMBER.LLM_MECHANISM_DEAD` still spell the single, admin-only destination. Nothing renders
  that key today and the Go sentence it mirrors already differed from it, but the table is canon: the
  doc's own **Q11** (recommend (a) — the sentence takes a `{remedy}`) has to be ruled in M2 before the
  frozen row can take the third argument.
- `WARDYN_DAEMON_PROXY_URL` has no credentialed-proxy form (a `WARDYN_DAEMON_PROXY_SECRET` secret-ref
  knob mirroring `UpstreamProxySecretRef` is a follow-up, not built in 0.7.6).
- The spent-token mark stays in-memory and unpersisted (the existing single-instance posture); a daemon
  restart re-grades a spent-but-not-yet-refresh-window credential `live` until the next dispatch marks
  it spent again. Persisting it is a follow-up, not built in 0.7.6.
- **The Runs board's GROUP header still aggregates N runs under one badge** and
  says nothing about what any of them is waiting on. Left out deliberately — one line cannot be true
  of five runs waiting on different things.
- **A run that never leaves STARTING keeps its last reason on the row.** It is
  invisible in the console (the read path blanks it for every state but STARTING
  and a terminal-reason FAILED) and exists for a SQL postmortem; nothing prunes
  it.
- **`status_detail` is the SUBSTRATE's words, not Wardyn's.** A reason Wardyn has
  no sentence for renders as `Waiting: <the raw string>` — the same honest
  degradation `failure_hint` already has.
- **No new Kubernetes permission, and none is coming.** The reason comes from
  `pods: get`, which the chart already grants; the Events API is deliberately NOT read — see
  `deploy/helm/wardyn/templates/rbac.yaml`'s own paragraph before "fixing" it by
  granting `events`. Only the Docker substrate can say "Downloading the image"
  outright, because `ensureImage` has just proved the host does not have it.
- **Carried forward from 0.7.5, still open (O-1: not bundled into 0.7.6, moved to 0.7.7):**
  - a per-person advisory lock closing the sign-in supersede race that, rarely, leaves two live
    sandboxes (see the corrected statement above — the residual is wider than 0.7.5's own bullet
    described);
  - a third Kubernetes cache volume (or `GOCACHE`/`GOTMPDIR`/`GOPATH`/npm cache pointed under the
    metered workdir) closing the autonomous-run `disk_mib` residual;
  - the admin-tier and run-token 5xx driver-text sweep (none of the sites is member-reachable).

## [0.7.5] — 2026-09-17

### Added

- **`POST /runs/preflight` answers where a run's model credential will live.**
  The response carries
  `model_credential {mechanism, residency, credential_source, staged_placeholder}`
  — `proxy` / `sandbox` / `image` / `unknown` — graded from the lane that
  ACTUALLY resolves for that exact body, not from the roster's declared
  mechanism — except the two cases a row settles by itself (`per_user` +
  `bedrock_sso` → `sandbox`; a `none` row with no lane → `image`) — so a `shared`
  row that declared lane is satisfied by a chain that fell through to the host
  `~/.aws` mount or to resident SigV4 keys grades the lane that actually
  resolved. Omitted for a run that makes no model call, and for the `422` a
  refusal answers with. A table test pins the grader against a hand-mirrored
  copy of every model-credential row of THREAT-MODEL's resident-secret table;
  nothing parses the document, so a new row there needs a case added by hand.
- **`GET /setup/status`'s harness rows carry `credential_residency` for the one
  row shape a roster settles by itself**: an enabled `per_user` + `bedrock_sso`
  row, which is `sandbox` whether or not that person has signed in. That is the
  estate the field report came from and the one case whose precise answer is
  unavailable on demand (Preflight `422`s a member who has not signed in).
  Absent on every other row — a roster cannot know which lane a run resolves.
- The rail's Credentials row states one of six sentences, each scoped to the
  MODEL credential, and chips whose AWS sign-in a resident session is. Where
  nothing has been resolved it reads *"Resolved at launch."* plus an invitation
  to press Preflight — there is no default, so the proxy sentence cannot be
  reached by an absence, and a run that makes no model call shows no Credentials
  row at all.
- **"View as a new member (not signed in)"** — a second posture of member mode that
  shows the one state the plain toggle structurally cannot: a `per_user` deployment's
  member who has not signed in to AWS yet. The account menu offers it beside **View as
  member**; inside it `/setup/status` grades the caller's model access `not_configured`
  with "Sign in to AWS", a Claude Code run is refused at create with the sentence a
  member who has not signed in already meets, and `POST /setup/harness-login` answers
  `409` rather than starting a capture that would land on the admin's own identity. The
  admin's captured session is hidden, never deleted, and returns on exit. Reported by
  the 0.7.4 field report (finding 3): member mode clamps the effective ROLE and leaves
  `sess.Sub` alone, so every per-principal credential lookup still resolved to the
  admin's own live session.
  - Wire: `/me` gains `member_mode_no_credential`; `POST /me/member-mode` accepts
    `no_credential`; `auth.member_mode` audit rows carry `no_credential: true` when the
    posture is entered (and nothing at all otherwise — existing rows are byte-identical).
  - The banner states the limit at the point of use: its VISIBLE sentence, not only its
    tooltip, says *"signing in is refused until you exit"* — a `title` is a hover, and the
    refusal is the one limit an admin walks into from inside the preview. The tooltip
    additionally says the sign-in is *hidden, not removed*. The sign-in pane offers no
    "Try again" when a launch was refused with `409` — the way out is to exit the mode. The
    entry is offered — and the posture granted — only where the org's model-access roster
    row is `per_user`; `/me` publishes `member_preview_available` and the toggle downgrades
    anything else to the plain mode.
- **Member mode's ceilings gain a fourth** (`docs/OPERATIONS.md`, and the banner tooltip
  that is actually on screen when the mistake is made): in the PLAIN toggle, model access
  and ownership still resolve to you. On a `per_user` deployment the new preview shows the
  not-signed-in state; the tooltip names no control, because the preview is not offered on
  shared/legacy deployments or while a mode is on.
- `/me` computes `member_preview_available` for the two admin tiers only, so a member's poll adds
  no store read of the agent roster.

### Fixed

- **Kubernetes: `disk_mib` now bounds an AUTONOMOUS (task-mode) run's writes to `/tmp` and its
  workdir `/home/agent/work`, narrowing 0.7.4's first known gap.** A run's `disk_mib` was the agent
  container's `resources.limits[ephemeral-storage]` and nothing else, so on an autonomous run it
  bound a container nothing writes in: the agent's commands run in an ephemeral container `Exec`
  attaches to the pod, and the kubelet meters no part of an ephemeral container's writable layer.
  (An INTERACTIVE run never calls `Exec` before an attach — its agent runs in the pod's own main
  container, whose whole writable layer the kubelet has metered against `disk_mib` since 0.7.2; this
  fix and the gap below do not touch it.) `CreateSandbox` now
  also mounts two whole-volume `emptyDir`s on the agent container, each with `sizeLimit` =
  `disk_mib` — `wardyn-tmp` at `/tmp` and `wardyn-work` at `/home/agent/work` — and the ephemeral
  container inherits them, because `Exec` copies the main container's mounts verbatim. An
  `emptyDir` is metered as the pod's local ephemeral storage whichever container writes to it.
  **Inside the budget now:** the clone at its default destination and everything written under the
  workdir (the checked-out tree, `node_modules`, in-tree build output), plus `/tmp` and the per-run
  CA files. **Still outside it:** everything the agent writes anywhere else — the rest of `$HOME`
  including the toolchain caches, and any authored target outside the workdir — stays on the
  ephemeral container's unmetered layer; this is a narrowing, not a close. Whole volumes, never a
  `subPath`:
  the API forbids subpath mounts on an ephemeral container, and `Exec`'s verbatim copy would have
  turned one into a dispatch failure for every autonomous k8s run with a disk budget. Each volume
  AND their sum are capped at `disk_mib`: the kubelet counts `emptyDir` usage toward the pod's
  `ephemeral-storage` total as well, proven on kind — a pod with the run's shape, writing 40Mi into
  each of two 64Mi volumes against a 64Mi container limit, is evicted with `Pod ephemeral local
  storage usage exceeds the total limit of containers 64Mi.` A run with no `disk_mib` gets no
  scratch volumes, exactly the pod shape this substrate produced before. The conformance case
  `TestConformanceK8s/EphemeralDiskLimit/OverTheLimitTheRunIsEvicted`, red in CI since 0.7.2, now
  runs once per fill target (`/tmp` and the workdir) and passes against a real apiserver; a new k8s
  case `ExecIsAcceptedWithADiskBudget` pins that the apiserver admits the ephemeral container a
  disk-budgeted run builds — the one thing no fake clientset can check.

  **Upgrade note.** If you set `disk_mib` / `storage.ephemeral.default_disk_mib` on Kubernetes, size
  it for the clone plus installs before upgrading: until now it did not bind an AUTONOMOUS run's agent;
  from 0.7.5 it evicts. An interactive run's agent has been bound since 0.7.2.
- **"Your model key" told a member their model access was already done, under a
  per-person AWS SSO lane, whether or not they had signed in.** The card read the
  deployment-wide `llm_ready` flag, which goes true the moment an admin saves a
  `per_user` roster row — before any member has signed in — so a member who had
  never signed in saw "Provided by your admin", and a member who HAD signed in saw
  the same admin-credit chip over their own session. Both readings contradicted the
  "Model access" chip directly above, which already reads the caller's own
  `status.model_access` correctly. The card now reads ONE total truth table
  (`model-key-state.ts`) shared with the page's own checklist bit: under a `per_user`
  governing row the result is never "Your key" and never "Provided by your admin" —
  it is "Your AWS sign-in" (live/renewable), "Your AWS sign-in · Expiring" (with a
  Sign in to AWS button), or "Not signed in" (with the same button, and the
  checklist item stays NOT done). An expired sign-in reads "Signed out" with the
  same button; `not_applicable` or an unreadable state claims nothing (below).
  A member's own leftover API key under a `per_user`
  row (or a SHARED row whose declared mechanism is Bedrock) is ignored
  for this purpose, EVERYWHERE it is read — that lane's dispatch mechanism can never
  accept one, so "Your key" + a done checklist would have sat over runs that are all
  refused. Reported by the 0.7.4 field report (finding 2).
  - The card's "Sign in to AWS" button and the page's existing chip-row button now
    both open the SAME sign-in pane (`setAwsLoginOpen`) — one pane, two entry points.
  - The page's OWN "Model access · Your key" chip, action line, and
    Sign-in button were still reading the bare `hasOwnKey` — a stale `mine` write
    from before an admin switched the roster to per_user (or Bedrock) left the chip
    row claiming "Your key" over a card that, one line below, said "Not signed in",
    and left the card's own new button dead (clicking it did nothing — the pane
    mount was ALSO gated on the same bare `hasOwnKey`). All three now read the same
    `ownKeyApplies`-gated predicate the card uses.
  - The card's own chips for `expired_signin` and `shared_expired` now drop the
    `Model access · ` qualifier the reused frozen strings carry — the card's own
    heading ("Your model key") is already the subject, so the qualifier stated a
    second one. *"There is nothing to set up for this sign-in."* now renders for the
    `not_applicable` state only; any other unreadable/unknown state under `per_user`
    renders no body at all rather than a guess.
- **A shared credential whose lane is Bedrock could still offer "Use my own key
  instead."** `mechanismSatisfied` refuses an Anthropic/OpenAI key on ANY Bedrock
  roster row, not only a per_user one — so a `shared` row with a Bedrock mechanism
  was, before this fix, the one combination left over: the card still let a member
  bring their own key, and a leftover key still said "Your key" / Done. It now
  behaves like a per_user row for this purpose (own key ignored, no bring-your-own
  form), but is graded on the deployment-wide `llmReady`/`shared_expired` rather than
  a per-principal state, since there is no per-principal state on a shared row.
- **The same page's lede said "shared credentials" under a per-person lane**, where
  the credential is specifically not shared and not inherited — the entire point of
  the lane, and the chip beside the sentence already said so. The lede now has a
  `per_user` variant that names the model-access lane as the one thing the member
  supplies themselves. Reported by the 0.7.4 field report (finding 2b).
- **The AWS sign-in sandbox runs its own sign-in now, so the Runs list no longer
  hands out a bare shell.** `agent-run --idle` creates a `wardyn` tmux session on
  the new `signin-pane.sh` BEFORE its workspace prep, and the pane runs
  `aws sso login --sso-session wardyn --no-browser --use-device-code && wardyn-aws-sso`
  exactly once. Every attach path joins that one session — the console's sign-in
  pane, `wardyn attach`, an SSH attach, and the Runs list — because attaching is
  `tmux new-session -A -s wardyn` (attach-or-create). Before this, only the
  console pane typed the pair; a person who opened the same run from `/runs` got a
  prompt with nothing typed, ran the obvious half, was told "Successfully logged
  into Start URL", and captured nothing, because `wardyn-aws-sso` — the half that
  uploads — never ran. The session is created BEFORE prep on purpose: prep takes
  a measured ~18 s and an attach lands the instant the container runs, so a
  session created after it would lose the name to that attach and leave the bare
  shell in place. If an attach wins anyway, agent-run respawns that pane on the
  sign-in.
- **`claude` and `codex` in the sign-in sandbox say what the box is instead of
  `command not found`.** Both are one shim that prints *"This is the AWS sign-in
  sandbox, not a coding agent. Start a Claude Code run from New run."* and exits
  non-zero. `agent-run --selftest` now asserts tmux is installed and that both
  shims refuse.
- **The sandbox's attach hint no longer tells someone to run a sign-in that
  already ran.** The hint fires for every interactive shell in the image —
  including the plain shell the sign-in pane hands over when it is done — so it is
  now printed only when the sign-in did NOT start on its own. Unguarded, a
  Runs-list reader saw "run this command" immediately after a successful capture,
  ran it, and met an `already_captured` refusal plus the console's fail marker on
  a sign-in that had worked.
- **The run page's sign-in banner no longer sends the reader elsewhere to sign
  in, and no longer promises a shutdown that does not happen on that path.** It
  points at the terminal — *"If the terminal shows a device code, finish it in
  your browser; if it shows a prompt, run the command the shell prints."* —
  which is true whether the image is new enough to self-run or an older pinned
  one that still hands out a bare shell, and that the sandbox stops itself after
  30 idle minutes, which is what the reaper actually does with
  `harnessLoginIdleCap`. The old clause ("closes itself when it is done")
  described the console pane's own `killRun`, which the Runs-list path never
  gets. The banner is now byte-identical for both image generations (it no
  longer asserts the sign-in is "already running", which was false for an
  older-pinned image and for a login run that had already finished) and renders
  only while the run is RUNNING.
- **A first Claude Code run on an image carrying 0.7.5's three `ENV` lines no
  longer parks an approval on the agent's own bootstrap.** On its first REPL
  start the CLI auto-installed the official plugin
  marketplace from `downloads.claude.ai`, git-falling-back to `github.com` —
  exactly the two hosts a private estate reported parking before anyone had asked
  the agent to do anything. The claude-code image now sets
  `CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL=1`, plus
  `DISABLE_AUTOUPDATER=1` and `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` (which
  remove the `registry.npmjs.org` update check and the
  `raw.githubusercontent.com` changelog fetch). A governed, version-pinned image
  should not install a plugin marketplace at start in any case: that is
  third-party code entering the sandbox outside the artifact the operator scanned
  and pinned. `examples/policies/default.json` is deliberately unchanged — the
  traffic is deleted, not allowed. Measured against a real `wardyn-proxy` with a
  model-host-only allowlist, past the trust prompt and into the REPL, plus an
  autonomous run: the stock v0.7.4 image parked approvals; the rebuilt image
  dials nothing at boot.

- **An interactive claude-code run on a 0.7.5-rebuilt image comes up on the agent, not on a
  product tour.**
  The image writes `{"hasCompletedOnboarding": true}` into the sandbox's
  `~/.claude.json` (and `${CLAUDE_CONFIG_DIR:-~/.claude}/.claude.json`) before the
  CLI starts, which removes Claude Code's theme picker and its "Security notes"
  page. That seed previously ran only on the managed-subscription lane, so under
  `bedrock_sso` — the lane a private estate actually uses — an interactive run met
  both screens before it could reach the model. The workspace-trust prompt is
  **not** pre-accepted: it is a security question, and the human attached to the
  run is the one who answers it.

- **A seeded interactive run on a 0.7.5-rebuilt image no longer loses its task text to an early
  attach.**
  The claude-code image created its `wardyn` tmux session *after* preparing the
  workspace, and both runners attach with `tmux new-session -A -s wardyn bash` the
  instant the container runs. With a repo to clone, prep is a measured 18 seconds:
  the attach won the session name, the boot session failed as a duplicate, and the
  run's initial prompt was silently dropped — with the only record on the
  container's stderr. The attaching shell then started a *bare* `claude` of its
  own, so an "auto-tools" run quietly became a supervised one. The session and its
  `agent-started` marker are now created before the first preparation step, a lost
  race falls back to `tmux respawn-pane -k`, and the boot pane waits for
  preparation to finish before starting the agent. An unseeded interactive run —
  the console's default — is unchanged.
- **The New Run rail no longer makes two unconditional security claims.** The
  "What this run can do" panel — the summary read immediately before Launch —
  hard-coded *"Minted at launch, injected by the proxy. Never written into the
  sandbox."* and *"Every keystroke and every outbound connection."*, and neither
  line consulted anything. The first is a **false assurance** on the
  `bedrock_sso` lane, where the captured AWS SSO session IS written into the
  sandbox (`threatmodel/THREAT-MODEL.md`'s resident-secret table says so); the
  second promises recording that a stock Helm install
  (`persistence.enabled=false`, the chart's own default) never captures. Both
  facts are now read from the server, and where the server has not answered the
  rail states neither.
- **A retry after an interrupted AWS SSO sign-in no longer reports a capture the server does not
  have.** Starting a sign-in now closes that person's previous sign-in sandbox for the same agent
  server-side, best-effort (before the new run is created, so a concurrency cap does not refuse the
  retry in the ordinary case) — the Claude subscription login sandbox follows the same rule — a
  closed sandbox's late credential upload is refused (`harness.credential.refused`,
  `reason = run_killed`), and the console's capture check re-reads the server's status a few times
  before accusing the sandbox — a status read can answer correctly and still be a moment behind the
  capture it is being asked about. The sandbox-side `run_killed` refusal names both causes (replaced,
  or cancelled) and where to finish.
- **The sign-in pane no longer gives up on a sandbox that is starting normally.** The wait was
  budgeted in poll ticks ("15 ≈ 30s", in practice anything from 30 seconds to fifteen minutes) and a
  measured 131-second first image pull — with healthy reads throughout — was narrated as an ordinary
  start until it wasn't. The wait is now measured on the clock and says which wait it is in: still
  starting, still starting but slow (a first start may need to pull the image), reads are
  failing but still retrying, or — after five minutes of failing reads — that Wardyn cannot read the
  run. Cancel still asks the server to end the sandbox in every one of them; where Wardyn cannot be
  reached the request is lost, and the sandbox ends at its 30-minute idle cap or the person's next
  sign-in.
- Four harness defects that kept four nightly jobs red since 0.7.0 are fixed; a green hosted
  nightly has not been observed yet (see Known gaps). None was a product regression and none was
  visible to PR CI:
  - `task-e2e-live`: the live e2e's recording seeder (`seedScriptWorkspace`) never onboarded the
    workspace it created, so every recording run was refused by the run-create mount gate
    ("is not an onboarded local directory"). It passed on a developer box only because onboarded
    workspace rows accumulate there; a fresh runner has none. The seeder now onboards its directory
    like every other seeder — the product's mount gate is unchanged.
  - `e2e-live`: `test/e2e/e2e.sh` builds its fixture agent image as `:latest`, but an agent name the
    operator image map does not carry resolves to `ghcr.io/cjohnstoniv/agent-<name>:<the daemon's own
    version>` — a tag published nowhere — so every run died at dispatch and 15 assertions cascaded.
    The script now registers the fixture (and claude-code) in `WARDYN_AGENT_IMAGES`.
  - `test-drive`: `scripts/test-drive.sh --up` brought up the stack without building the per-run
    proxy sidecar image, which lives behind compose's `build-only` profile (`scripts/up.sh` builds
    it, so `make demo` first hid the gap). On a cold host every run failed to dispatch and every
    section reported "sandbox not RUNNING". `--up` now builds it.
  - `docker-tagged-live`: `TestStandalone_CapabilityHonesty` still asserted an unconditional
    `SessionRecording: true` after the capability became honest (it mirrors `Config.Record`). The
    test now builds the driver the way a recording daemon does and pins the `Record=false` arm too.
- **Eight console sentences that were false on some deployment.** A systematic
  check of every sentence 0.7.5 added or moved, against the states the server
  actually emits, found claims that are true on the estate they were written for and
  wrong elsewhere. All eight are now either GATED on the state that makes them
  true or replaced by a no-claim state — no surface guesses:
  - A member on a **shared** Bedrock deployment read *"Model access · Your AWS
    sign-in"* over a card saying *"Provided by your admin"*. The server projects
    `live` for the admin's shared credential (that is what a member's model
    access IS there), and the chip said it was theirs. The chip row now says
    whose credential it is, and the New Run rail's own chip states **ownership**
    (*"Per-person AWS sign-in"*) rather than a sign-in status it never read.
  - The **login-sandbox note** on `/runs/:id` said the sign-in was *"already
    running in this box"* on every harness-login run — including one that had
    already finished, and including a sandbox from an older pinned `aws-sso`
    image where nothing types the chained command at all. It now points at the
    terminal, names both shapes it can be in, and renders only while the run is
    RUNNING.
  - The New Run rail told the reader to *"Run Preflight to see where this run's
    model credential will live."* beside a Preflight verdict that carried no
    residency (what every 0.7.4 daemon answers), and said *"Resolved at
    launch."* beside *"No model provider is connected."* Neither renders when it
    cannot be kept, and the **Recording** section withholds its heading, not
    just its sentence, until `/healthz` has answered.
  - The sign-in pane's corroboration refusal sent the reader to *"Getting
    Started"* — from the two admin screens that mount the same pane — told the
    admin to *"tell your admin"*, and promised a supersede a 0.7.4 replica does
    not perform mid-upgrade. It now says to sign in again, and names the run's
    audit trail.
  - Both *"Starting the sign-in sandbox"* waits blamed *"the first start after an
    upgrade"* pulling an image *"onto this node"* — on a first install nothing
    was upgraded, on compose there is no node, and the same line narrates the
    Claude flow.
  - The AWS blurb asked for the organization's access portal URL under a managed
    row, where there is no field and the server ignores what is sent.
  - The member-mode tooltip pointed at *'View as a new member'*, a menu item that
    is absent on shared/legacy deployments and gone from the menu while the mode
    is on.
  - An **expired** AWS sign-in read *"Not signed in" / "Nothing is configured for
    you until you do"* on the card while the chip row said *"Signed out"* and the
    server's action line said a session existed.
- **The member's Getting Started page carried two buttons with the identical
  accessible name "Sign in to AWS"** (plus a plain-text action line saying the
  same words), and the card's one opened a pane in a different card above it
  while staying enabled as a no-op. Both buttons now carry distinct accessible
  names — the visible text is unchanged — and the card's is hidden while the
  pane is open.
- Two AWS SSO sign-ins started at the same moment for one person (a double-click, or the console and
  a `wdn_` token) no longer leave two live sign-in sandboxes in the ordinary case. The launch
  re-checks once its own run row exists and ends only the caller's older sign-ins, a deterministic
  order that holds across replicas without a lock, and the re-check itself never ends the last live
  sign-in sandbox (a launch that fails after its first pass can still leave none — see Known gaps).
- A sign-in sandbox superseded while its credential upload was already in flight can no longer
  overwrite the capture that replaced it: the upload door re-reads the run's state immediately
  before it stores and refuses `run_killed` there too.
- **The `conformance` CI job builds the two local-only images its new
  boot-egress case needs.** 0.7.5's `TestBootEgress_NoFirstUseApproval` boots the
  real claude-code image behind the real `wardyn-proxy` sidecar; neither
  `wardyn/agent-claude-code:local` nor `wardyn/wardyn-proxy:local` is published
  anywhere, and `CreateSandbox` fails closed on an image it cannot pull — so the
  required job went red on the release commit while `make ci`, which deliberately
  excludes that target, stayed green everywhere locally. The job now builds both
  before running the suite. No skip-if-the-image-is-absent knob: the one
  measurement that proves a stock boot parks no first-use approval must not be
  able to quietly not run.
- **`agent-base` carries the three ENV lines that keep a Claude Code boot
  quiet.** `install.sh` maps the `claude-code` harness key to
  `ghcr.io/…/agent-base`, and the BYOI recipe is `FROM agent-base` — so the fix
  that stops the CLI auto-installing a plugin marketplace from
  `downloads.claude.ai` (git-falling-back to `github.com`), self-updating and
  fetching a changelog lived only in `agent-claude-code`, which has not been
  published since 0.6.2. The vars are inert in an image with no Claude Code; what
  they buy is that an image (re)built `FROM ghcr.io/cjohnstoniv/agent-base:0.7.5` inherits the quiet
  boot without having to know the list exists. `deploy/images/README.md` now
  states that contract (these
  ENV lines — not an `agent-run` export, which a default interactive run's
  `claude` never inherits — plus a `seed_claude_onboarding` call).
- **A codex-cli interactive run no longer loses its seed to an early attach.**
  The published `agent-codex-cli` still had the 0.7.4 boot shape: the `wardyn`
  tmux session created AFTER the workspace prep, the agent-started marker written
  first, no respawn fallback, and a boot pane that waited for nothing. A seeded
  run attached during prep hit `duplicate session: wardyn`, dropped the seed, and
  had its shell auto-start suppressed by the marker — a bare prompt with the
  operator's task text gone and no record anywhere a human looks. The
  create-or-take-over bootstrap is now one shared implementation used by all
  three images; the boot pane's prep wait (bounded by the prep process still
  running, never a clock) and the git-helper caller-auth secret re-read are
  shared by the two coding-agent images. The aws-sso sign-in pane keeps its own
  300-second prep wait.
- **A sign-in pane that cannot do its job still hands over a shell.** Two
  `set -u` expansions — an unset `HOME`, and an unset login command — killed the
  pane AFTER its banner, which is the one point at which the console has stopped
  typing the sign-in itself: the session was destroyed (or an attached client
  dropped) with nothing said. Both paths now end in the shell the pane promises.
  When workspace preparation never finishes, the pane says so in its own words
  and SKIPS the sign-in rather than running a pair that cannot work and landing
  on a line that names a command which fails identically. The respawn path also
  clears the stale "the sign-in did not start on its own" line the bare shell
  left above the banner.
- **A total tmux failure no longer leaves the agent-started marker behind.** When
  tmux is missing the marker is deliberately never written — the shell's
  auto-start is then the only agent a human can get. When tmux is present but
  refuses both the create and the respawn the outcome is identical, and the
  marker stayed: the human attached to a bare shell that started nothing while
  nothing ran anywhere.
- **`agent-run --selftest` honours the documented opt-out.** The three
  self-fetch vars are set with `:-` precisely so a run can turn one back on, and
  the selftest is fail-closed at dispatch — so a deliberate `=0` refused to start
  the run. It is reported now, not failed. Any other value still fails. The
  selftest cannot detect an image that lacks the Dockerfile `ENV` lines
  altogether — the library defaults them before the check ever runs.
- **The "recording is disabled" sentence (the Recordings library and a run's
  Recording tab) named the wrong switch.** It has told
  the reader to set `WARDYN_RECORDING_DIR` since 0.7.1; `WARDYN_RECORDING_DIR`
  only moves the `fs` store's path and turns nothing on or off. The actual
  switch is `WARDYN_RECORDING_STORE` (the Helm chart renders `off` while
  `persistence.enabled=false`), and the sentence now says so:
  *"No run on this server will ever produce one — set `persistence.enabled`
  (Helm) or `WARDYN_RECORDING_STORE=pg` to turn it on."*

### Changed

- The recording-disabled title and description are one shared pair instead of
  three spellings (the Recordings library and the run cockpit's Recording tab read
  both; the New Run rail reads the title), and the three `/healthz` reads behind them
  are one hook whose answer is TRI-STATE: until `/healthz` has actually replied,
  no surface claims recording is on OR off. The cockpit's own copy had already
  drifted — *"No run on this server captures one"* vs *"will ever produce one"*.
- **The console's sign-in pane stopped auto-typing into a sandbox that is signing
  itself in.** It now waits 12 s after attaching and types the chained command
  ONLY if the sandbox has not announced itself (no `wardyn: sign-in running`, no
  device URL, no success or refusal marker) — the version-skew path for an
  operator-pinned `WARDYN_AGENT_IMAGES` image older than this release. Typing into
  a running sign-in would have run a second `wardyn-aws-sso` in the same run,
  which the server refuses as `already_captured`: a fail marker on a capture that
  succeeded. Estates that pin agent images should pull the 0.7.5 aws-sso image at
  the same upgrade. **The Claude Code quiet-boot fix needs the same care**:
  rebuild the claude-code image from the 0.7.5 tree (`make agent-images`, or
  `docker build -f deploy/images/claude-code/Dockerfile -t <your-registry>/agent-claude-code:0.7.5 .`
  from the repo root), or rebuild a derived image
  `FROM ghcr.io/cjohnstoniv/agent-base:0.7.5` re-copying `deploy/images/claude-code/agent-run`
  and `deploy/images/common/agent-run-lib.sh`. An image on another base must set
  the three `ENV` lines in its own Dockerfile. **Then deliver it:** `make agent-images`
  only tags `wardyn/agent-claude-code:local` on the machine that built it, which a
  cluster's nodes cannot pull — push the rebuilt image to the registry your nodes
  pull from and re-point the `claude-code` entry of `WARDYN_AGENT_IMAGES` at the new
  tag. An older tag left pinned there keeps 0.7.4's behaviour.
- `run.kill` audit rows can now come from Wardyn itself, carrying `reason =
  superseded_by_new_login`, `superseded_for` and `superseded_by_run`; they audit as `success` like any clean
  kill. A supersede whose teardown or revocation failed audits `failure` with the failing step and writes no
  `run.revoke` row — the new sign-in still proceeds, and the closed sandbox's capture upload is refused.
  `superseded_for` is the run's `created_by` — under the shared admin-token or a local-mode principal
  that names a CREDENTIAL, not a person, so two operators sharing one supersede each other.
  `harness.credential.refused`'s `run_killed` reason is now decided on BOTH sides of the per-owner
  lock (the late arm carries `owner` + `credential_source` beside it), and `store_error` there now
  also covers the login run's re-read.
- A kill's teardown/revocation cascade is detached from the caller and runs to its own 30-second
  bound (`killCascadeTimeout`) even if the caller's connection dies mid-request (it always did on
  the kill route; it now does wherever the server kills a run on its own behalf).
- The live AWS SSO walk (`scripts/kind-sso-walk.sh`) now runs TWO spec files in
  one invocation against one cluster — `sso-member` then `sso-member-recovery`.
  The second covers the paths a member who is already signed in cannot reach:
  the agent roster saved from the CONSOLE and a member bound by its three org
  settings (start URL, pinned account, pinned role), the sign-in sandbox running
  its own login and being joined from the Runs list with nothing typed, a
  cancelled sign-in retried cleanly, an abandoned one superseded, a 65-second
  `STARTING` hold — capped there because the run would otherwise die at the
  90-second pod-IP bound — that must read as slow rather than unreadable, an interactive
  claude-code run reaching Bedrock through one workspace-trust prompt, a first
  run whose approvals list is empty, and the admin's no-credential member
  preview.
- The walk stopped typing into a sign-in sandbox that is already signing itself
  in. Since the aws-sso image runs the chained command itself, nothing echoes
  its argv, so the old 60-second poll for that text always missed and then typed
  a SECOND login into the running pane — an `already_captured` refusal and the
  pane's fail marker, on every walk. It now waits for the image's own banner.
- `WARDYN_KIND_SSO_REBUILD=1` on the walk rebuilds `wardynd` and `wardyn-proxy`
  from the working tree and reloads them before the run. The console is baked
  into the daemon image, so without it a walk can judge the previous release's
  screens with the current release's assertions. Every walk now also records the
  tree's HEAD and each image's digest into `images.txt` beside its evidence.
- CI runs `agent-run --selftest` against the aws-sso image it already builds for
  the trivy scan. Everything that selftest checks — tmux present, both
  coding-agent shims present and refusing — was otherwise exercised only by
  source-level tests with fake binaries, so a build that lost `tmux` shipped
  green, and without tmux nothing starts the sign-in.
- The conformance suites' `go test -timeout` values (docker 20m, k8s 25m) and the
  k8s job's `timeout-minutes` (35) now have room for the cases 0.7.5 added, and
  the budget pin asserts the eviction case leaves five minutes for the REST of
  the suite rather than merely fitting the ceiling by itself. A post-verdict
  sandbox teardown is bounded, so a slow cleanup of a case that PASSED can no
  longer spend the package timeout and panic away every verdict.
- `test/e2e/e2e.sh` merges a caller-supplied `WARDYN_AGENT_IMAGES` instead of
  overwriting it, and refuses loudly if the caller pins either of the two keys
  its own assertions name.
- `scripts/kind-sso-walk.sh`'s opt-in rebuild prints one warning that it retags
  the `:local` agent images on the picked daemon, which a compose stack on that
  same daemon adopts for new runs.
- `make ui-typecheck` now also runs `playwright test --project=live --list`, so a
  live spec that cannot even be collected (an import reaching a stylesheet, a
  syntax error) fails PR CI instead of surfacing only in the manual kind walk —
  the live specs themselves still run only there, never in CI.

### Known gaps and deferrals

- **The quiet Claude Code boot, the onboarding seed and the early-attach fix reach only images
  rebuilt on 0.7.5.** `agent-claude-code` is not published; rebuild your derived image
  `FROM ghcr.io/cjohnstoniv/agent-base:0.7.5` and re-copy `deploy/images/claude-code/agent-run` +
  `deploy/images/common/agent-run-lib.sh`, push it where your nodes pull from, and re-point the
  `claude-code` entry of `WARDYN_AGENT_IMAGES` at it — a `helm upgrade` alone delivers none of the three. An image on any other base must set the three `ENV` lines
  in its own Dockerfile — copying `agent-run` is not enough, because a default interactive run's
  `claude` is started by the attach shell. An older tag pinned in `WARDYN_AGENT_IMAGES` keeps 0.7.4's
  behaviour: a first interactive run still parks `downloads.claude.ai` and `github.com`.
- **Two concurrent sign-ins can still, rarely, both stay alive.** A run's `created_at` is stamped
  in-process just before the row is written, so timestamp order and write order can disagree — most
  plausibly across replicas with clock skew, or after a stall between the two steps. If the run
  carrying the EARLIER timestamp is written after the other launch's re-check, neither launch sees
  a run it should supersede and both survive. Neither is KILLED, so the capture upload's
  `run_killed` refusal does not separate them either; the person's next sign-in clears it. Closing
  it needs a per-person advisory lock around the insert and the re-check — 0.7.6.
- **The supersede's kill cascade still runs synchronously inside the launch POST** (up to ~30 s per
  superseded run; on Kubernetes it waits for pod deletion). The teardown is detached and always
  finishes, but a client that gives up mid-launch can be left with the old sign-in gone and no new
  one — retry. Documented in `docs/OPERATIONS.md` ("One live sign-in sandbox per person") rather
  than fixed, because the remedy is one retry and the alternative is a launch that answers before
  the slot it needs is free.
- **The New Run rail still paints its credential chip from the ROSTER ROW alone:**
  it says whose credential the lane uses, not whether that person has signed in.
  The signed-in/expired state lives on Getting Started, which is where the
  action is.
- **A member on a shared Bedrock row whose ADMIN credential is `expiring` is still
  offered the member-side sign-in CTA** (the actionable-state set is shared with
  the per-person lane). The chip beside it now says the credential is the
  admin's.
- **On Kubernetes, `disk_mib` is NARROWED to `/tmp` and the workdir for an AUTONOMOUS run, not
  closed.** 0.7.5 put `/tmp` and `/home/agent/work` inside the budget for a run whose agent commands
  execute through `Exec` (task mode). Everything such a run's agent writes anywhere else stays on the
  ephemeral container's unmetered writable layer: the rest of `$HOME` — including the toolchain
  caches a build actually fills (`/home/agent/go`, `~/.cache/go-build`, `~/.gotmp`, `~/.npm`,
  `~/.cache/pip`) and the dotfiles (`~/.wardyn`, `~/.ssh`, `~/.claude`) — `/opt/rust`, and any
  authored `workspace_repos` or ephemeral-source target outside `/home/agent/work` (a target may
  legally sit at `/work`, `/workspace` or elsewhere under `/home/agent`). By bytes the residual is
  the larger half on a Go or Node build. Nothing is mounted at `/home/agent` itself deliberately: a
  volume there would shadow the `.bashrc` every agent image bakes (the attach hint), swallow the
  reserved drive mount point `/home/agent/drive`, and hide the read-only `~/.claude` bind the
  subscription path uses. `readOnlyRootFilesystem` would close the residual and is not set, because
  the agent legitimately writes those paths. Node-level eviction still backstops all of it.
  **An interactive run's agent runs in the pod's own main container, whose whole writable layer —
  `$HOME` and the toolchain caches included — the kubelet has counted against `disk_mib` since
  0.7.2; there is nothing outside the cap, so size an interactive run's `disk_mib` for its caches
  too, not only the clone.** **0.7.6 follow-up:** point `GOCACHE`/`GOTMPDIR`/`GOPATH` and the npm
  cache under the workdir on this substrate, or give the caches a third volume.
- **What the 0.7.5 proof does not cover.** The kind conformance proof ran against the busybox
  conformance-agent image on runc (CC1): `emptyDir` metering of writes from an ephemeral container is
  unmeasured under gVisor (CC2) and Kata (CC3). The live kind SSO walk separately exercises a real
  `agent-run` boot — both the aws-sso sign-in sandbox and a real claude-code run — on the
  `emptyDir`-backed `/tmp` and `/home/agent/work`, on runc; that walk is manual, not CI (see
  `docs/TEST-GAPS.md`).
- **Docker's `/tmp` mount flags have no Kubernetes equivalent, on any confinement class.**
  `nosuid,nodev,noexec` on the Docker path's tmpfs `/tmp` have no counterpart on Kubernetes, before
  or after this change: an `emptyDir` cannot carry mount options. Measured, not assumed — see
  `threatmodel/THREAT-MODEL.md`. A standing parity gap, not a 0.7.5 regression.
- **The preview shows the STATE, not the FLOW.** Signing in is refused inside it by
  design, so it cannot rehearse a member's FIRST SIGN-IN; that still needs a real second
  identity (`member@wardyn.local` on the kind walk, `wardyn-member` on Entra).
- **Rolling upgrade.** The posture rides the session cookie as a second `omitempty` bool
  with no codec bump, so a 0.7.4 replica ignores it — mid-rollout it shows the admin
  their own credential and does not refuse harness-login. In the other direction a 0.7.5
  console POSTing `no_credential` to a 0.7.4 replica gets a `400` from the strict body
  decode, the mode is not entered, and the menu item reports the failure; the plain
  toggle is unaffected, because the key is sent only for the new posture.
- **The posture is model access only, and this browser session only.** The same admin's
  CLI, `wdn_` API token or second browser still creates and dispatches on their real
  credential, and `/me` still returns their own user-drive allocation.
- **A roster flipped `per_user` → `shared` while an admin is inside the preview** leaves the
  variant banner up until they exit; the cookie records what they asked for, and re-reading
  the roster on every render would put a store read on every screen.
- **`shared` deployments are deliberately unchanged** by the preview: the credential
  there is the operator namespace every run inherits, so hiding it would show a state no
  member on that deployment is ever in.
- **A sign-in sandbox opened from the Runs list is not stopped when the capture
  lands.** Nothing server-side stops a login run on `harness.credential.captured`
  — the shutdown is the console sign-in pane's own `killRun` — so a sandbox
  reached from `/runs` instead lives until the reaper's 30-minute idle cap. The
  run page now states that bound rather than promising a close it will not get.
  Follow-up: stop the run server-side on capture.
- **An unpinned multi-account sign-in asks a question in the terminal.** With no
  `sso_account_id`/`sso_role_name` on the roster row and more than one account or
  role reachable, the helper asks which one IN the sign-in pane (three tries).
  Only a WRITABLE attach can answer it; a read-only Runs-list viewer watches it
  time out. Pinning the account and role on the roster row avoids the question
  entirely.
- **The sign-in pane is deliberately NOT recorded.** The claude-code image's boot
  pane wraps its seed in `wardyn-rec`; the aws-sso sign-in pane does not, because
  it handles the credential the run exists to obtain — so there is no cast of what
  happened inside a sign-in sandbox, including anything typed at the pane's
  trailing shell. The audit trail still carries the launch and the capture.
- **A run launched with "Let it use tools before I attach" waits for a human.**
  Claude Code raises its own *"Bypass Permissions mode"* confirmation when the
  agent is started with tool approvals skipped, and its default selection is
  "No, exit" — a bare Enter quits the CLI. Wardyn does not pre-answer it: the
  image seeds product onboarding only, never a security prompt. So such a run
  comes up parked on that confirmation until someone attaches and chooses "Yes",
  rather than working before they arrive. Whether ticking that checkbox in the
  console should count as the operator's consent to the CLI's own confirmation is
  an open decision.

- **`skipWebFetchPreflight` is not set.** Under `CLAUDE_CODE_USE_BEDROCK=1` the
  CLI still preflights `api.anthropic.com` when the agent uses WebFetch. That
  fires on tool use, not at boot, and the image ships no Claude Code settings file
  to put the flag in, so it is out of scope here and recorded as a follow-up.

- **The `codex-cli` image still parks five first-use approvals at boot, and this
  release does not fix it.** Measured with the same harness: a stock
  interactive codex run reaches for `api.github.com`, `github.com`,
  `raw.githubusercontent.com` and `chatgpt.com` within a second of coming up, and
  `ab.chatgpt.com` about a minute later. The claude-code fix is three environment variables; codex-cli has
  no equivalent — every `CODEX_*` name in the shipped binary (0.149.1) was
  enumerated and none disables an update check or telemetry, and the two
  `chatgpt.com` hosts are an unauthenticated CLI reaching for sign-in and feature
  flags rather than an updater. Fixing it needs either a confirmed
  `~/.codex/config.toml` key or a per-harness boot-host allowance, both of which
  are decisions rather than edits.
- The rail states a model-credential residency with **no click** only under a
  per-person Bedrock SSO roster row. On every other deployment the honest answer
  needs the run to be described first, so the rail says "Resolved at launch."
  until Preflight answers with a residency — a verdict that carries none (a
  0.7.4 daemon, a roster read that failed) leaves the same sentence up, minus
  the now-false Preflight hint.
- A model-credential secret deleted **between Preflight and Launch** can drop a
  `shared` roster row from the never-resident Bedrock bearer lane onto a resident
  SigV4 one while the rail still shows the Preflight verdict. The dispatch-time
  mechanism gate does NOT refuse that move: a `shared` Bedrock row is satisfied
  at the coarse provider level, so bearer → captured-SSO / `~/.aws` mount /
  static keys is not a mechanism change in its terms. A `per_user` row is
  unaffected — it admits one lane only.
- With the subscription `~/.claude` mount and `WARDYN_SUBSCRIPTION_INJECT` on,
  "injected at the proxy" is the deployment's stated mode: the staged sentinel is
  written by an operator-run script the daemon never reads back. The rail names
  the mount rather than promising nothing is mounted, but an operator who staged
  a real credential and left injection on is described as staged.
- The Kubernetes bounds on how long a sandbox may take to come up — `canaryWaitTimeout` (3 minutes)
  and `podIPWaitTimeout` (90 seconds) — are not configurable, and a registry slower than them fails
  the run. The measured 131-second cold pull of the `aws-sso` image spends 73% of the first one. The
  operational answer is to pre-pull the agent images onto nodes at upgrade time; docs/OPERATIONS.md
  says how. Making the bounds configurable is an owner decision, not shipped here.
- The live AWS SSO walk proves the per-person credential path end to end against
  an **unsigned on-cluster fake**, not AWS. It confirms which account and role
  real botocore asked the portal to mint; it cannot confirm that AWS would mint
  them, that the role's policy permits Bedrock, or that a real IAM Identity
  Center matches the fake at any edge. The real-tenant walk stays owner-gated.
- It does not exercise a **genuinely cold image pull**. Images reach the kind
  node by `kind load` and the sandbox pod pulls `IfNotPresent`, so nothing is
  fetched from a registry on any walk. The cold-start case manufactures its
  65-second hold with a node taint (capped there because the run would otherwise
  die at the 90-second pod-IP bound, and lifted so the run survives), which reproduces a pod that cannot start — not a slow registry, and
  not an `ImagePullBackOff` (which is terminal, not slow). A private-registry
  estate's real cold pull is still unmeasured.
- The IdP is **Dex with two static passwords**, so group-to-role mapping,
  conditional access and token lifetimes are out of scope; the device-code step
  is **pre-approved permanently** by the fake, so a code that expires before
  anyone attaches is never exercised; and both principals are driven serially by
  one browser, so **concurrent members** are not covered.
- The four nightly fixes above are proven locally (fresh-namespace live suite, the docker-tagged test,
  and a reproduction of each dispatch failure against a host-mode daemon). Only the hosted nightly run
  can prove the two full compose suites end to end (`e2e-live`, `test-drive`): both bring up the
  default-named compose project, which cannot be run on the maintainer's box while its own stack is up.
- **A workspace create/update that fails at the store can leave library rows
  and audit entries behind.** No transaction seam exists to put both writes in
  one, and no orphan-source heal reconciles them at boot; the residue is
  inert — such a row is never mounted, scanned or cloned.
- **Concurrent MCP permission requests are still decided one at a time** by
  `wardyn-toolgate`; there is no evidence the agent issues them concurrently.
- **A scan that stops at the depth cap still reports high confidence.** A
  distinct "depth capped" note without the confidence demotion needs a new
  wire field.
- **Clamping a member policy up to an operator's `always_deny` ceiling can
  raise the reported Review-rail risk score.** The rationale is an explicit
  availability statement, not a security regression; a reword or a separate
  non-security axis is a later change.
- **A UNIQUE index making the audit spool's replay idempotent is not
  shipped**; the sanctioned path is an operator-run `CREATE UNIQUE INDEX
  CONCURRENTLY` behind a flag, since it cannot run inside a migration
  transaction.
- **A revoke that names a human's email does not reach their UI-sandbox relay
  session** — the session always carries the OIDC `sub`. Revoke by `sub`, or
  use the global `all: true` cutoff; closing this needs a schema change.
- **A role demotion is not caught until the relay session's TTL**, matching
  the SSH gateway's own admin-override staleness bound.
- **An already-established relayed WebSocket outlives a revoke** — killing the
  run is what ends one, the same bound attach and both SSH lanes already
  publish.
- **`POST /runs` is still synchronous.** The console's own launch deadline
  covers the symptom; making run creation itself asynchronous would move the
  CLI's `run --wait` contract, the 201 body and several e2e suites — a 0.8
  design item.
- **Member mode clamps the role, not ownership, groups or SSH.** Runs,
  workspaces and secrets an admin created stay theirs in the mode; the SSH
  gateway's admin override (keyed on the key's own TTL-bound role) is
  unaffected by it; a rolling upgrade's outgoing replica ignores the flag
  entirely, since it rides the existing session cookie with no codec bump.
- **No `role` parameter on `POST /me/tokens`.** A deliberately downgraded
  `wdn_` token was the other way to reach a member lane; `RefreshAPITokenRoles`
  re-stamps every token to the principal's freshly derived role at next
  sign-in, so this needs a `role_pinned` column that does not exist yet.
- **Setting an AWS SSO account/role pin does not invalidate an
  already-stored capture, and there is no admin "revoke this person's
  captured session" route.** Both need owner enumeration in the secret store,
  which the per-user namespace does not have.
- **Tracking the inner per-tunnel MITM `http.Server`s so shutdown actually
  stops them is a design gap**, not closed this release (the drop is now
  counted; the tunnels still outlive shutdown). `mitmHosts` remains keyed on
  the bare host, latent since the only producer today dedupes by bare host —
  two pinned tests turn red the moment either gap becomes reachable.
- **The fresh-install "Skipped" badge fix has its own ceiling**: "once per
  page load" can only distinguish THIS load from the NEXT one — it cannot
  tell "a previous install's mark" from "mine, from 30 seconds ago, before a
  reload". A fresh install where the operator skips Integrations and then
  reloads sees the latch re-arm and wipe its own skip. The correct fix is
  discriminating by install identity, not by page load, which needs a stable
  per-install marker `SetupStatus`/`GET /healthz` do not carry today.
- **`OPERATOR_ONLY_REASON` ("Requires the admin role.") still stands at ONE
  security-tier site**: the workspace detail record pane's tier note, which sits
  inside a `<fieldset disabled={!securityOperator}>` and so admits a security
  admin as well as an admin. It is pinned by an existing test asserting the
  sentence renders twice on that pane (the pane-level note and
  `NewSessionForm`'s own), so moving it is a test change as well as a copy one.
  The other four sites this bullet named through 0.7.4-rc all read
  `SECURITY_ONLY_REASON` at the tip: `live-approvals.tsx`'s panel hint and its
  `ScopeMenu` `Always` reason, the Approvals decide chip, and the run-detail
  cockpit's decide chip.
- **`@mermaid-js/mermaid-cli` stays a console devDependency.** Moving it needs
  a new install location `scripts/check-diagrams.sh` can find `mmdc` at — a
  structural change out of scope for the lane that found it.
- **The kind AWS-SSO walk is a manual proof.** No workflow runs it; a green
  result is evidence only for the tip it was run on. Real SigV4 and real
  Bedrock inference stay owner-hardware-only.
- **The AWS SSO test hatch is production-forbidden, not production-
  discouraged**: no AWS SSO operation Wardyn uses is signed, so Wardyn cannot
  distinguish the fake endpoint from the real one — published as THREAT-MODEL
  residual #45 rather than mitigated.
- **A workspace `write:` requirement can still flip `read_only: true` on a
  mount an admin authored read-only**, deferred: `*bool` carries no
  authorship, so the fix needs mount provenance on the type, and the
  proposed "never flip an explicit true" would break the pinned
  "required defaults to read-write" behaviour.
- **A site-config read failure at dispatch degrades per an explicit owner
  decision, not a residual**: a transient failure is retried once; a
  genuinely unreadable roster still fails the run closed rather than
  guessing.
- **Per-run branch-namespace confinement of the installation token itself is
  not built** — Wardyn deliberately does not request repo-admin access to
  create or hold a GitHub ruleset, so binding the token to a ref prefix stays
  a manual operator step; opt-in verification that a ruleset exists already
  ships.
- **A daemon-side copy of a run's session cast before `StopSandbox` tears the
  sandbox down** is a larger change than this release's residue justifies and
  is not built.
- **A HELD approval chip does not yet degrade past `hold_expires_at`** — the
  field does not exist on the wire yet; the current degrade
  (`waitingHeld(n)` → `waiting(n)`) is a known, pinned ceiling.
- **A pinned "Checking…" state for a in-flight, not-yet-decided check is not
  built this release.**
- **`enforcementFor`'s degraded-read variant is the only one implemented**;
  a fuller generalisation is deferred.
- **A member's `/` landing always re-derives from the server on every load,
  deliberately** — no per-browser flag, because a shared browser handing
  member B member A's landing mark was a real failure mode the design
  considered and rejected.
- **A generic "leaving with unsaved changes" guard is not built**; today's
  screens with a draft either dirty-guard individually or don't.
- **The app-shell header's overflow fix took a label-dropping approach
  instead of `flex-wrap`** — both close the same defect; `flex-wrap` remains
  unexplored as an alternative shape.
- **A deterministic-runner-only e2e case (the kill-request assertion
  branching on run state, not a regex over either outcome) is deferred**
  pending a runner whose kill path is reliably reproducible in the harness.
- **`wardyn-rec`'s SIGTERM handling for the in-sandbox recorder wrapper is
  unchanged** — a run stopped by the reaper or a kill signal in the exec-less
  or Kubernetes dispatch paths can still lose its final upload window; a fix
  needs signal forwarding to the wrapped process plus a bounded flush, judged
  a bigger change than this release's residue.

- **Admin-tier and run-token 5xx sites still carry driver text.** Every door a
  member can reach is converted (0.7.4) — a full route-tier enumeration says so,
  not a spot check — and the `writeServerError` chokepoint they route through is
  the shape the rest will take, but the admin-tier and run-token 5xx sites still
  hand the caller raw pgx text. **None of them is member-reachable**: they sit on the
  admin tier or on the run-token `/internal/*` lane, whose readers are an
  operator who can already read the DSN and a sandbox that holds the run's own
  token. The sweep did not land in 0.7.5 and is rescheduled: 0.7.6.
- **The killed-run tail-upload grace is still measured from `updated_at`, not
  from a terminal timestamp.** With the keepalive closed (0.7.4), the remaining
  re-openers are wardynd's own writes — chiefly the boot reconciler clearing a
  dead run's `sandbox_ref`, which can re-open the five-minute window hours after
  the run ended. The doors it re-opens are upload-only, re-check the run for
  themselves and still require that run's own unrevoked token. Closing it needs
  a `terminal_at` column.
- **A member's Model-access chip reads "Signed out" for a pin-contradicted AWS
  SSO session that is actually live and renewable.** The server deliberately
  grades `expired_signin` on a pin contradiction — a design choice, not a bug:
  `PinMismatch` is `json:"-"`, so the chip is keyed on grading state alone and
  cannot distinguish "expired" from "contradicts the current pin". The `Action`
  line directly beneath the chip is correct and does name the pin, so the member
  is not misdirected; only the chip's own label overstates the session's health.
  Closing this needs either `pin_mismatch` on the wire with its own chip label
  or a state-neutral "Model access · Sign in again", which is a canon decision
  rather than a doc fix. `docs/OPERATIONS.md` now says so explicitly.
- **The e2e `mockSecurityAdminRole` fixture does not splice the
  member-projected `/setup/status`** the way `mockMemberRole`'s
  `mockMemberSetupStatus` does, so seven specs render a security admin against
  an *operator's* status body — the one shape that tier never receives, since
  the redaction keys on `!isOperator`. The behaviour those specs cover is pinned
  elsewhere (vitest over the components, plus one security-admin e2e case), so
  this is a fixture-fidelity gap, not an uncovered surface.
- **`mockMemberSetupStatus` hand-mirrors `redactSetupStatusForMember`'s field
  drops with no parity guard.** It is a deliberate mirror of the server's
  structural drops rather than a re-derivation of its value projections, and
  nothing fails when the Go function drops one more field: the fixture simply
  starts proving a render against a body no server produces.
- **The Runs board's member empty state still speaks to the person who
  launches runs.** `RUNS_MEMBER_EMPTY` reads "Runs you launch appear here" over
  a body pointing at "a workspace your admin has made available to you" — true
  of a member, off-key for the security admin who now reaches the same empty
  state and sees every run on the deployment. Wording only, and a canon
  decision.
- **The union Go coverage floor was ratcheted 65 → 78 against a measured
  78.3 %**, a 0.3-point margin — thin enough that an ordinary refactor can turn
  `cover-check` red on a tree with no test regression in it. Re-measure and
  re-set the floor at the next release rather than treating 78 as headroom.
  Not re-measured in 0.7.5.

## [0.7.4] — 2026-09-16

0.7.4 is the governance-hardening pass over the whole surface a member or an
operator's identity touches: per-run credential residency and revocation, the
Kubernetes runner substrate's own cleanup, the egress proxy's TLS-port and
policy-normalization edges, a real "view as member" tool for admins, and a
kind-provable AWS SSO test path. The console side carries a large batch of
member-visibility and UI-consistency fixes across Runs, Providers, Workspaces,
Approvals and Setup, alongside accessibility and theming work.

**Console and refusal copy is provisional**: new `400`/`412`/`422` bodies and
new console strings in this release ship as frozen DRAFT constants pending the
maintainer's own (unpublished) canon sitting; tests assert through the
constants, so adopting final wording is a constant-for-constant substitution,
no logic change.

### Security

- **A killed run's token can no longer open any `/internal/*` door.** The revoke
  cascade that follows a run going terminal is best-effort, so a run whose
  revocation write failed kept presenting a token that verified — every internal
  door except token-renew went on minting credentials and resolving injections
  (including the operator's own live model OAuth token) for up to the token's
  TTL. Every door now re-checks the run is still alive; the three tail-upload
  doors (session recording, scan results, the captured AWS SSO cache) keep a
  five-minute grace for the watcher race that ends the run.
- **A governance profile that walls off Amazon Bedrock now withholds the
  resident credential too**, not only the never-resident bearer injection: the
  static AWS SigV4 keys in the sandbox environment, their publication on the
  sandbox spec, and the operator's whole host `~/.aws` bind mount are all
  withheld now, inside a sandbox whose principal is denied that service.
- **A pasted credential is refused for a provider captured by the containerized
  login.** Pasting a free-text AWS token used to silently overwrite the
  reserved, structured SSO session (after which Bedrock fell through to the
  host `~/.aws` or static keys) and joined the daemon's process-global
  secret-mask corpus for its whole life. Empty and over-long tokens are refused
  on the same door.
- **Dispatch resolves a run's secrets against the run's own subject**, not a
  local-mode request header, for the `llm_inspection` detection corpus and
  `env_secret` grants — unchanged for OIDC and admin-token callers.
- **A captured AWS SSO session that a later roster pin no longer allows is
  refused at run time, not discovered as an IAM 403.** `POST /runs` now answers
  422 and dispatch fails the run closed — no sandbox created, no credential
  authored — naming both the account/role the stored session is for and the
  account/role the agent now allows. The person's own recovery needs no admin:
  `model_access` grades "sign in again," the setup checklist's AWS Bedrock row
  warns naming both pairs, and signing in again replaces the stored session.
- **A corporate artifact mirror with a port-qualified allowlist entry no longer
  crash-loops the proxy sidecar for every run.** `wardyn-proxy` failed closed on
  any redirect whose paired injection rule read a port-qualified entry the
  injector's port-less lookup could not match. Shipped in the same change:
  cleartext credential injection is now refused on the TLS-conventional port
  set `{443, 8443, 9443}` regardless of what the allowlist authored (previously
  clamped at 443 only, so an authored `vendor.example:8443` handed a sandbox a
  key over plaintext), and an `https://` redirect's injection scope now
  declares `require_tls`, refusing a cleartext request to the mirror instead of
  silently uncredentialing it. A plain `http://` redirect is unchanged.
- **Control-plane traffic never rides the corporate proxy.** The decision sink,
  the injector, the approval client and the run-token renewer built their
  transport by cloning the caller's client, which preserves
  `ProxyFromEnvironment` — with `HTTP(S)_PROXY` visible to the sidecar, the run
  token, approvals, decisions and minted credential values transited the
  corporate proxy and skipped the trusted-URL pin. The sidecar now owns that
  transport unconditionally with `Proxy` cleared.
- **A policy entry spelled with trailing dots, or in non-ASCII, is no longer
  silently dead.** Request-side host normalization strips every trailing dot;
  policy-entry normalization stripped only one. Fixing both together also fixes
  an allow: an `allowed_domains` entry with a trailing dot now genuinely grants
  the host it names, where it previously granted nothing — read this before
  upgrading a policy authored that way. A non-ASCII entry is now refused at
  write time, naming the punycode spelling to use instead.
- **The git credential helper now emits a brokered credential over HTTPS
  only.** A non-HTTPS clone of an allowlisted forge previously had the brokered
  PAT emitted and sent as `Authorization: Basic` in cleartext; it now falls
  through to the unbrokered path with nothing minted.
- **A GitHub installation token the broker mints and then discards is now
  handed back to GitHub**, best-effort, closing the window where the loser of
  an exactly-once mint race (or a failed audit-insert/commit) left a live
  `contents:write` token unreachable by any later revoke.
- **A run's credential-bearing Kubernetes objects are reclaimed even when its
  pods are already gone** — a kubelet eviction over the run's `disk_mib`
  ephemeral-storage limit, or node deletion, took the agent pod with it and the
  orphan sweep reported success without touching the proxy pod, the per-run
  Secret (carrying the proxy config and every `secret_env` value) or either
  NetworkPolicy. The sweep now also lists NetworkPolicies, keyed on the run id
  the object's own `wardyn.run-id` label carries, and reclaims the Secret beside
  them
  (the Role narrowing below is how, and why it is not a Secret list). On the
  Docker substrate, a
  caller-supplied label may no longer override the driver's own `wardyn.run-id`
  / `wardyn.component` / `wardyn.managed` teardown selectors.
- **A member's policy can no longer author unbounded approval holds.**
  `first_use_hold_seconds` and `max_holds` were the last two `RunPolicySpec`
  fields the composer clamp and `validatePolicySpec` both missed — under a
  `wait_for_review` ceiling, an inline policy authoring a million-second hold
  cap turned the documented 16-hold default into a million goroutines polling
  the control plane for thirty days. Both are now bounded (`≤ 256`, `≤ 600`) at
  every ingest point.
- **The AI scan advisor's answer is validated before it reaches a profile.**
  The advisor is fed untrusted repo content, and its suggested hosts bypassed
  the deterministic scanner's own host validator while its languages, package
  managers and tools landed verbatim in the generated `AGENTS.md` the next
  agent reads — a prompt-injection re-entry path. All three now cross the same
  validators, under a length and count cap.
- **`git_push_any_branch` is graded** on the Review rail a human reads before
  approving a run — it turns off branch-namespace confinement for brokered
  pushes and previously scored nowhere. It now grades high, beside a
  read-write host mount and a write-capable GitHub token.
- **The secret-mask registry no longer holds a cached masker forever for a run
  with no secrets of its own.** A scan or grantless run's derived masker
  outlived the sweep, which listed only the per-run map its own secrets keyed.
- **The audit row's `target` field is now capped** (512 bytes plus a
  truncation marker) at every writer, closing an unbounded-write path into the
  append-only audit table on the authenticated, rate-limit-free `authz.denied`
  lane.
- **5xx bodies no longer carry driver text on any door a member can reach.**
  A new chokepoint logs the underlying error and writes only the
  operator-facing sentence; raw pgx text (DB host, port, user, database,
  SQLSTATE, table and constraint names) previously reached any member who
  could trigger a transient store failure. Every member-reachable door is
  converted — run create, preflight, read, kill, files, grants, profile and
  layout; approvals; workspaces; secrets; tokens; SSH keys; harness login;
  policies; capabilities; the setup status a member reads; the drive seed — and
  the shared ceiling/drive helpers they route through. The admin-tier tail is
  deferred (below).
- **The anonymous `/healthz` no longer publishes fleet-wide kernel-sensor
  volumes.** Its ground-truth block is the verdict, its last heartbeat and why
  it is not healthy; the cumulative counts move to the operator-gated
  `/metrics`.
- **Every response now carries `Cache-Control: no-store` by default**, closing
  a shared-cache exposure window the cookie-authenticated OIDC lane opened
  (hashed `/assets/` bundles and the SPA shell are unaffected).
- **The dev/e2e Postgres container (`scripts/up.sh cmd_pg`) now publishes on
  loopback only**, and a repo-wide guard now holds every `docker run … -p`
  under `scripts/` to the same rule.
- **`deploy/compose/.env` is now created with mode 600 from the first byte**,
  instead of a world-readable `cp` briefly holding the freshly minted secret
  store's age key before a later `chmod`.
- **A UI-sandbox relay session now names its app, and is scoped to it.** A run
  declaring more than one `ui_app` had entering the second app silently replace
  the first app's session cookie in the browser, so the still-open first tab's
  requests dialed the second app's port. The relay path now carries the app as
  a segment and the cookie is scoped to it; a bookmarked pre-0.7.4 relay URL no
  longer resolves and an open session across the upgrade must be re-entered
  from the run page.
- **The UI-sandbox relay session is bounded-stale instead of a frozen 8-hour
  bearer.** Every new connection now re-asserts owner-or-admin against the
  freshly loaded run and the same revocation cutoff `POST /sessions/revoke`
  stamps, and a warm pooled connection re-checks on its own request path at
  least every 30 seconds. A relay cookie minted before this release has no
  issued-at and is refused once — re-enter the app once from the run page.
- A UI-sandbox relay path must now spell its run id canonically; a
  non-canonical spelling (upper-case, braced, unhyphenated) no longer forwards
  the relay path prefix into the sandbox's own app.

- **Credential injection over cleartext port 80 now requires a BARE allowlist
  entry.** Accepting a port-qualified entry for the injector's host binding —
  which is what lets a `m.corp:443`-shaped mirror boot at all, above — removed
  the premise the port-80 arm was written under, that a bound host's entry is
  *silent* about the port. A host an operator named only as
  `vendor.example:8443` could therefore be handed the brokered credential on a
  sandbox-chosen `http://vendor.example/` request. Every other port still asks
  the authored-port question; a genuinely bare entry injects on port 80 exactly
  as before.
- **A port-qualified WILDCARD deny now cancels the injector's host binding.**
  `allow m.corp:8443` + `deny *.corp:8443` bound the injection rule although
  egress itself denied the port — the any-port arm shadowed exact port-denies
  but not wildcard ones.
- **The Kubernetes runner Role has no Secret-body read capability in any
  configuration.** Widening the orphan sweep to reclaim a run's Secret (above)
  first reached for `secrets: list`, and RBAC cannot scope a list by label — so
  that verb was a namespace-wide plaintext read of every Secret's body, which
  with the default empty `k8s.runsNamespace` is the control plane's own database
  DSN, OIDC client secret and ingress TLS key. The reclaim is kept and the verb
  is gone: a run's NetworkPolicies carry no credential and are now created
  **before** its Secret and deleted **after** it, so a surviving Secret always
  has a surviving NetworkPolicy to be found by, and the label-scoped
  `deletecollection` the Role has always had does the reclaim. Operators running
  their own Role (`k8s.rbac.create=false`) should add `networkpolicies: list` on
  upgrade — without it the sweep logs one warning and behaves exactly as it did
  in 0.7.3.
- **A run's activity keepalive no longer touches a run that has ended.**
  `TouchRun` stamps `updated_at`, which is also the clock the killed-run
  tail-upload grace is measured from — and the UI relay, both attach pumps and
  the SSH channel keepalives all called it *before* the door that refuses a
  non-RUNNING run, so an authenticated caller could hold `/internal/recordings`
  and `/internal/scan-results` open on a cadence for as long as they liked. The
  guard is in the shared writer, so all four lanes close at once.
- **Twenty-seven more member-reachable 5xx sites stopped carrying driver
  text** — `POST /runs` (plain tier, including its member validation path and
  the runner-capabilities 503), `/runs/{id}/{files,grants,profile}`,
  `/me/tokens`, `/me/ssh-keys` and both inline-policy resolvers. This, with the
  eight doors and the seven seams that followed it — the last four doors
  (`GET /policies/{id}`, `GET /me/capabilities` at both of its reads,
  `GET /setup/status`'s secret list, the drive seed's site-config read) and the
  three shared helpers every one of them routes through (`writeCeilingError`,
  `writeCeilingErrorPrefixed`, `writeDriveError`, which had no request to log
  against and so pasted the driver text into the body) — and six more a full
  route-tier enumeration found afterwards: the second capability read, the
  site-config read behind both repo admission and the member workspace-provider
  gate, and three in the owner-reachable workspace scan — is what makes the
  claim above true of *every* member-reachable door: a Postgres blip no longer
  answers
  a member with the deployment's database host, port, user and database name. The
  error still reaches the log with its method and path.
- **A policy entry that can never match is named at sidecar boot.** The
  non-ASCII/malformed-entry refusal is a write-time check, so a policy stored
  before it landed compiled silently — and on `denied_domains` that fails OPEN,
  since a request arrives punycode-encoded and no compiled entry spells the
  stored form. Each sidecar now logs one WARN per dead entry, naming the entry
  and its list. A warning, never a refusal: the compiled policy is unchanged.
- **Plain `http://` for `WARDYN_BEDROCK_BASE_URL` is audible.**
  `WARDYN_ALLOW_TEST_ENDPOINTS=true` unlocks two relaxations and only the AWS
  SSO override warned at boot. The Bedrock one re-points the bearer-mode
  credential-injection target, so the API key rides `Authorization: Bearer` in
  cleartext on every model call; it now logs a `TEST HATCH ACTIVE` WARN naming
  the plaintext target, and the flag's own help names the relaxation. The
  refusal without the acknowledgement is unchanged.

### Added

- **An admin can now exercise the member path without a second identity.**
  "View as member," from the account menu, clamps a signed-in SSO admin's
  session role to `member` at the one place the role is read — the console,
  `GET /me` and every operator-gated route answer exactly as a member's would,
  with a persistent banner and a way back out. The stamped role is never
  rewritten (so exiting restores it verbatim), `issued_at` freezes across the
  toggle, every audit row still names the admin's own subject, and minting an
  API token or registering an SSH key is refused in the mode (both carry a
  role stamp that would otherwise outlive it). See `docs/OPERATIONS.md`,
  "Exercising member mode as an admin," which also gives the genuine
  second-identity recipe for proving what a member is *refused*, which the
  mode alone cannot show.
- **AWS SSO is testable without an AWS tenant.** `make kind-sso` adds Dex with
  two principals and an on-cluster fake of AWS IAM Identity Center plus a
  Bedrock-runtime stub to the kind quickstart cluster; `scripts/kind-sso-walk.sh`
  walks a member signing in on their own seat and getting their own role
  credentials, with the fake's own log confirming which account and role real
  botocore asked it to mint. See `docs/OPERATIONS.md`, "Testing AWS SSO
  without an AWS tenant."
- **One gated test hatch moves every AWS SSO endpoint together**
  (`WARDYN_AWS_SSO_ENDPOINT_OVERRIDE`, refused unless
  `WARDYN_ALLOW_TEST_ENDPOINTS=true`, boot-time-only, warns loudly when
  carried) — published as THREAT-MODEL residual #45. `ui/e2e/live/` is a new
  Playwright project for specs that drive a real running Wardyn instead of the
  hermetic e2e backend.
- A docker-gated regression test pins that a sealed CC1 sandbox cannot resolve
  an external DNS name — the per-run network is gatewayless, so the daemon's
  embedded resolver has nowhere to forward a query it cannot answer.
- Four new Playwright specs close coverage gaps the plan's field audit found:
  Settings' Host card and Model-provider connect/replace/disconnect lane
  (`settings-connections.spec.ts`), a seeded workspace's real, non-intercepted
  detail page (`workspace-detail.spec.ts`), the SSH-keys add/reload/delete
  round trip (`ssh-keys.spec.ts`), and "make a policy from this run"
  (`run-policy.spec.ts`) — plus in-place hardening of the policies,
  permissions, runs, recording, navigation and member-getting-started specs.

### Fixed

- A `POST /runs` that fails after the run row exists no longer leaves a ghost
  run: every late failure now marks the run FAILED and revokes its minted
  identity, instead of a 500 that left a PENDING run holding a live token
  until the undispatched sweep reaped it up to an hour later.
- Review (`POST /runs/preflight`) now reproduces the repository-provider
  admission and agent-roster refusal, so it can no longer show a clean
  checklist for a run that create then refuses.
- The run-detail Files and Resources widgets no longer stall for five seconds
  and answer 500 on a workspace with a lot of changed files; a capped read now
  answers 200 with `truncated: true`.
- A runner that returns no exec session is a 500 on both widgets rather than a
  nil-pointer panic in the daemon.
- Renaming a run policy onto a name that is already taken answers 409 naming
  the clash, not a 500 carrying the raw Postgres constraint text.
- `title`, `description`, `repo`, `devcontainer_repo`, `task` and `agent` are
  all length-capped and control-character-checked at the door; a NUL in a
  title was previously a Postgres 500, and `repo`/`task`/`agent` had no cap
  beyond the 1 MiB request body.
- A policy whose `git_pat` or `ssh_key` grant names a missing secret is now
  refused with a message naming that grant, instead of always blaming
  `api_key`.
- A `devcontainer_repo` run on a deployment with no image builder now says so
  on the 201, instead of only in a setup log line no client reads.
- Review and the launch response no longer claim a scan or `task_mode=exec`
  run — which gets no model credential — uses Amazon Bedrock automatically.
- Several people launching runs at once no longer serialise behind an
  unresponsive AWS SSO token endpoint when their own captured session still
  has enough validity to carry the run.
- A transient site-config read failure at dispatch is now retried once before
  a run's model-credential scope is decided, so one dropped database
  connection no longer costs an otherwise healthy run (an unreadable roster
  still fails the run closed — an explicit owner decision, not a residual).
- The `harness.credential.refresh` audit row omits `registration_expires_at`
  when the captured session carries none, instead of recording a date in the
  year 1.
- **A custom `WARDYN_AGENT_IMAGES` agent can now be picked from a fresh run**,
  not only cloned from an existing run of it.
- **`site_config.write`'s audit row now names what changed, not just a
  count.** Two saves that both flipped only the upstream proxy URL used to
  audit an identical row; the datum now carries the changed fields and a
  `from→to` pair per redirect (capped, with the honest unbounded total kept
  beside it).
- `ScmHosts`/`EgressRedirects[].{From,To}`/`UpstreamProxyURL` are now stored
  canonical, not whatever case/whitespace was typed; interior whitespace is
  still a 400.
- A site config declaring only `internal_hosts` no longer reads as
  unconfigured; a dedicated info row now surfaces the same warning the
  deployment log already carries for that override.
- The egress-redirection check no longer renders a bare, empty
  `(ecosystems: ; …)` clause when every redirect is network-only.
- A legacy `artifact_overrides` body naming an unknown ecosystem key now names
  that key in its 400, instead of an opaque validation error two passes later.
- `PUT /site-config` now refuses two `egress_redirects` rows sharing a `from`,
  case-insensitively — the first duplicate silently shadowed the second.
- **A workspace whose built image was invalidated can be rebuilt again.** The
  in-memory build tracker used to outrank the workspace row for "done," so a
  PUT that deleted the built image (or a rescan that moved the profile hash)
  left the workspace unbuildable until wardynd restarted; the row decides now,
  and every invalidator drops its tracker entry except mid-flight.
- **Re-saving a workspace without changing it no longer wipes its reviewed
  state.** A differently-cased repo slug, a `.git` suffix or a mixed-case
  clone-URL host previously counted as a content change, silently clearing
  approved egress, requirements and recorded evidence.
- `DELETE /workspaces/{id}` now refuses while a session holds the workspace,
  instead of stranding an `AllowAllEgress` sandbox and losing the capture
  silently.
- The observed-egress panel no longer offers hosts that cannot be approved
  (git-broker- and control-plane-owned hosts, and hosts the operator has
  explicitly denied) — one dead suggestion used to break promotion for every
  real host beside it.
- The GitHub App mint now carries an HTTP deadline scoped to the WHOLE mint,
  not per round trip, so a blackholed `api.github.com` can no longer pin the
  grant row's lock and queue every other mint for that grant.
- `NO_PROXY` in the sandbox environment is now derived from the configured
  proxy URL, not a hardcoded name — an operator-overridden proxy URL
  previously left the sandbox's own `HTTP_PROXY` host out of its own bypass
  list.
- `wardyn-toolgate` and the in-sandbox result uploader now reach the run's own
  proxy directly instead of through `$HTTP_PROXY`, so an overridden proxy URL
  no longer denies every tool call and drops every result upload.
- Concurrent callers of the subscription provider now share one delegated
  `claude` credential refresh instead of each spawning their own.
- Workspace git-remote detection now reads regular files only, never follows a
  symlinked `.git/config`, and reads a large config whole — a FIFO named
  `.gitmodules` previously blocked the scan's HTTP handler goroutine forever.
- The remote-URL parser no longer reports a scheme, or half a split userinfo,
  as a host, and now parses a bracketed IPv6 authority in both URL and
  scp-like forms.
- The git credential helper parses a bracketed IPv6 `host=` value instead of
  truncating it at the first colon.
- `wardyn attach` no longer leaves the real terminal stuck in raw mode when the
  process is killed by a signal (SIGTERM, SIGHUP, a manual SIGINT) instead of
  a local Ctrl-C.
- `wardyn attach <run-id>` now validates the run id before dialing, the same
  way `ssh` already does.
- `wardyn run recording` gained a `--timeout` flag so a peer that sends
  headers and never finishes the body no longer hangs the download forever.
- `wardyn support-bundle` no longer reports success while writing a truncated
  bundle — a flush failure is no longer silently swallowed, and the bundle is
  written to a temp file and renamed on success only.
- `wardyn ssh`'s exit code is clamped to a non-negative value when the child is
  killed by a signal.
- `wardyn proxy-relay`'s unauthenticated-exposure warning now prints to
  stderr, not stdout.
- The CLI's 401 auth hint now names both `WARDYN_ADMIN_TOKEN` and
  `WARDYN_TOKEN`.
- `setup wall` and `setup vault` now reject a stray extra argument.
- The coverage floor no longer sits eleven-plus points below what the suite
  actually proves (`COVER_MIN` 65 → 78).
- A flaky Playwright test now fails the UI e2e gate instead of silently
  passing.
- `release-check` now runs the Postgres-gated concurrency proofs under the
  race detector, matching CI.
- `WARDYN_E2E_NO_UI_BUILD=1` now refuses a stale `ui/dist`.
- The Go unit suite has its own falsifiable skip floor for the seven
  redirect-probe tests that self-skip without `curl` on PATH.
- A member can now attach to their own AWS SSO login sandbox — an "unknown
  owner" mount previously read as "not your run" and was refused client-side
  before the server (the actual enforcement point) was ever asked.
- `POST /setup/harness-login` now answers before the sandbox is up, narrating
  the wait and attaching once the run is RUNNING, instead of racing a cold
  image pull (or the Kubernetes network-policy canary) against the console's
  own request deadline.
- The AWS sign-in sandbox names itself on `/runs/:id`, and its attach shell now
  prints the chained sign-in command it needs, with one definition shared by
  the image and pinned against the console's own copy.
- A console call that launches a sandbox now carries a 5-minute launch
  deadline instead of the 60-second bound meant for a hung daemon.
- A member's own Getting Started no longer calls an admin-only endpoint on a
  cold `/setup` load, and Settings no longer does the same on every visit —
  both used to draw a 403 and an `authz.denied` audit row against the
  legitimate member before their real role was known.
- The console's mount-time auth probe (and every HTTP 401 response) now
  discards the response body it does not read, closing a per-page-load and
  per-rejected-request socket leak.
- A failed audit-trigger restore no longer leaves the database running a
  superseded hash-chain function; the boot-time replay is now one transaction.
- Four clock-skew classes between wardynd and Postgres are closed: the boot
  heal's newer-action guard, the idle reaper's staleness check, `/healthz`'s
  "latest heartbeat" read, and a member's unscoped approvals page now all
  compare against the database's own clock rather than the daemon's.
- The admin drive doors (`GET /drives` and the drive write) now bound their
  filesystem questions the same way the member doors have since 0.7.3, instead
  of hanging on a stopped-answering mount for as long as it takes.
- An admin preview of a drive is no longer recorded as a refused run.
- A drive 409 now names the guard that actually fired, instead of always
  falling through to the home-namespace sentence.
- A confirmed drive re-home now records which identity fields moved and how
  many allocations went with them.
- A bulk credential revoke now names each credential it killed, instead of
  only an aggregate count.
- A secret written into a namespace nobody is known to own now says so on the
  audit row.
- A digest-pinned base image pre-pulled on the host is now recognised as
  present instead of re-pulled.
- The `make agent-images` hint is now attached only to a failed pull of the
  images it actually builds.
- Every devcontainer build now drops its own per-build base tag once wrapped
  into the output image.
- The git-repo build path now validates its output image tag for whitespace
  and control characters, matching the generated-files path.
- **The console no longer reads a member's redacted setup body as a fact
  about the deployment.** Settings' Image-builder row and the barrier picker
  both now distinguish "withheld for your role" from "actually off."
- **A member can delete and rebuild the workspaces they own** — the server has
  always admitted the owner; the console previously parked both on the admin
  role.
- Your model key is now named for the agent your org actually runs
  (`anthropic-api-key` for Claude Code, `openai-api-key` for Codex CLI),
  instead of always `anthropic-api-key`.
- A member's empty Runs board is now a member's, not the operator first-run
  funnel rendering a host barrier readout the member's redacted status leaves
  blank.
- Allowed hosts on a workspace's egress panel now only offers Remove where the
  remove actually lands.
- A 403 on Permissions or Governance is now reported as a role, not an
  outage, and names a security admin too where the gate admits one.
- New Run's saved-policy lane no longer leaks a redacted body into the Custom
  textarea when switching modes.
- The saved-policy rail sentence no longer claims the launch merges nothing,
  when the create door still prepends the attached workspace's mounts.
- A saved policy that stopped existing now says so on the rail instead of
  silently going quiet.
- Codex CLI's Tool-approvals segment no longer shows a checked-but-disabled
  Hold option.
- The Safety meter no longer shows a stale grade as current while a new one is
  still resolving.
- A multi-workspace clone's extra attachments now survive editing the primary
  workspace selection.
- The Workspace select gained its own accessible name.
- The live-approvals Deny confirm button is now gated against a double click.
- The New Run rail no longer accuses an unreachable daemon of having no model
  provider configured.
- An interactive run's own owner no longer hears "Requires the admin role."
  before the run has even started; a starting/queued notice with no dead link
  replaces it.
- Switching between two attach sessions on the Recording tab can no longer
  show the wrong cast.
- Escape inside the Recording session picker's — and every held-approval
  Deny confirm's — dialog no longer also exits focus mode and drops a live
  attach socket.
- A FAILED run's failure-hint chip — the only place on the page that says why
  a run failed — never disappears again at a narrow viewport.
- A run finishing mid-edit on the cockpit canvas no longer silently replaces
  the in-progress arrangement.
- A dock widget that stops being available no longer leaves an open glass
  panel with nothing renderable inside it.
- The Runs table's page cap can no longer end on an orphan group header, nor
  render more rows than the stated cap.
- "Refresh now" no longer blanks the whole toolbar into a loading skeleton for
  a round trip the board already runs every three seconds in the background.
- The dead `onerror` arm in the live-attach terminal is removed; the pinned
  "[closed] after budget exhaustion" behaviour is unchanged.
- An approval card's inlined run context no longer mislabels a nameless,
  non-interactive run as "Interactive session."
- A run's exit code and ending stay known past a chatty run's 1000-row audit
  cap, via their own scoped reads.
- The shell's own attention/approvals badge poll now pauses while parked on
  the Runs board, which already runs an equivalent poll.
- The phase rail can no longer be clicked past a gate it doesn't obey.
- The Review step's readiness rollup no longer overstates two things: an
  optional, fixable check no longer sits under green "Ready," and the badge no
  longer reads "Ready to launch" under a standing confinement-floor warning.
- A duplicate egress redirect `From` can no longer be added.
- A fresh install no longer shows a false "Skipped" badge from a mark a
  previous install left on the same browser (see Known gaps for the residual).
- A mid-session sign-out now says why, and returns to where you were — only
  when the identity that signs back in can actually reach it.
- The session-expiry banner gained a third state (`none | soon | expired`)
  instead of reading "expiring soon" forever, including past the real expiry.
- The header no longer overflows sideways on a phone.
- A rejected control-plane request no longer replays a dead admin token
  forever, and the fix no longer races a fresh sign-in landing at the same
  moment.
- `readyz()` is now bounded against a transport that accepts a connection with
  no ready backend behind it.
- A non-JSON or oversized error body no longer renders raw in a toast.
- A failed clipboard copy is no longer silent.
- The TS wire mirror gains six fields the server already sent and fixes one it
  read wrong (`scanWorkspace()`'s plural `scan_run_ids`).
- A member can now be told apart from an admin for a shared destructive-delete
  control (`useCanMutate`, a new `allowed?` prop on the shared delete-confirm
  dialog).
- `--muted-foreground` and `--ring` now clear WCAG AA on the surfaces they
  actually sit on, not only on white.
- White badge/button text on a fill that failed AA is now a token, not a
  guess, and the destructive button's hand-duplicated red (which had silently
  drifted from the design token in dark mode) is unified with it.
- The console no longer flashes light on load before React's dark-first theme
  applies.
- Motion now respects `prefers-reduced-motion` console-wide, not at one call
  site out of 82.
- Every floating or sticky container in the console is now bounded to the
  viewport (dialogs, the mobile nav drawer's sheet, popovers, and both sticky
  rails).
- Four small accessibility/dead-code primitives: configurable heading levels
  on empty/error states, a focus-visible ring on tab panels, a longer default
  toast duration with a close button, and a first favicon.
- A stale `dark:bg-destructive/60` dilution left over from the destructive-
  button unification is removed; the delete-confirm dialog's reason text now
  keys on the actual confirm-button gate, not the raw operator flag.
- A member-reachable authz-denial reason at two `live-approvals.tsx` sites now
  correctly names "the admin or security admin role" for a security-tier
  gate, instead of always "the admin role."
- The Add-workspace dialog no longer promises an edit path this console
  doesn't have, no longer silently drops a Branch typed for a non-repo source,
  and its two image-picker options that stored identical state collapse into
  one honest "Auto."
- A workspace's Recorded-sessions card no longer overclaims that the loop
  writes a least-privilege policy — it writes egress requirement rows; the
  policy hand-off is the separate "Save session profile" action.
- A stale workspace-detail load can no longer paint over a newer one after
  navigating between workspaces.
- The workspace-detail model-provider warning no longer fires when the daemon
  simply didn't answer.
- Approvals now recovers on its own from a one-off failed load, and deciding
  an approval no longer flashes the whole queue back to a loading skeleton.
- Approvals' Decided tab is now a real, shareable, reloadable URL.
- The audit trail's Event filter no longer strands itself on a kind a
  drilled-into run doesn't have.
- The policy editor can no longer be dismissed by accident once you've started
  editing — Escape and click-outside are now blocked once the draft differs
  from what the editor opened with.
- Every front-door `helm install`/`helm upgrade --install wardyn` recipe
  (README.md, docs/VERIFY.md, the wardyn-k8s-setup skill, and the chart's own
  README) now actually renders — each was missing an age-key source, a
  runs-namespace choice, or a CC2/CC3 RuntimeClass pin/default-policy override
  the chart now requires.
- Numerous stale symbol/line citations across OPERATIONS.md, AUDIT-ACTIONS.md,
  POLICIES.md, ENVBUILD.md, AGENT-THREAT-MODEL.md, sdk.md, SECURITY.md,
  DESKTOP.md and several code comments are corrected to match the code they
  describe; sdk.md's "one non-stdlib dependency" claim is made true by inlining
  a two-line helper instead of importing a whole package graph for it.
- The demo catalog's episode-minutes summary no longer counts unrecorded
  episodes that already carry a projected length, and the "See it work" demo
  grid's subtitle no longer promises every demo needs no model or key.

- **Every admin-tier `authz.denied` row now carries the `member_mode` marker.**
  Two emitters built their own `Data` map instead of calling `authzDeniedDatum` —
  the operator-only `decision_scope: always` refusal and `denyMemberField` (whose
  `workspaces.llm_cred` arm is admin-tier) — so neither marked a refusal met
  inside **view as member**, and `denyMemberField`'s rows carried no `method`
  either. Both are reachable by an admin in the mode doing what the member
  Getting Started invites: deciding their own run's held egress at scope
  `always`, creating a workspace. A reviewer filtering the denial stream read an
  admin's own member walk as a member incident — the outcome the field was added
  to prevent. `docs/OPERATIONS.md` now states the guarantee by predicate;
  `docs/AUDIT-ACTIONS.md` states the predicate and cites the four sites.
- **A member's empty Runs board and the account menu's Demos entry key on
  `role !== "admin"`.** `/setup/status` is redacted on `!isOperator`, which is
  SUPER-admin only — so a **security admin**'s status arrives with `checks` `[]`,
  `secrets.present` `[]` and the driver withheld, exactly as a member's does.
  Through the old two-valued test that tier fell into the operator first-run
  funnel and read every withheld field as a fact ("Needs the `<name>` secret" for
  secrets that may exist), over two `/setup?step=` deep links that land on a
  Getting Started which ignores `?step` — and the account menu offered the same
  dead link. Both now match `setupGateActive` and `GettingStarted`, which already
  moved for this reason.
- **A real member turning member mode ON no longer stamps the flag on their own
  cookie.** The handler already called this a no-op; it was not one. `GET /me`
  then answered `member_mode: true`, the console painted a banner naming an admin
  role the human does not hold, and both credential-mint doors — which key on the
  flag, not on the stamped tier — refused them their own SSH key and API token
  with "Exit member mode…", breaking the member Getting Started's "Connect your
  tools · Add SSH key" card and `docs/MEMBERS.md`'s SSH path. Turning it OFF
  still re-signs, always.

### Changed

- **A workspace composition is bounded** at 64 sources, matching the door's
  other list caps.
- **A repo `ref` is validated where it is authored** (create/update, not only
  at build), refusing control characters or whitespace instead of silently
  dropping that source from the clone.
- `GET /workspaces/{id}/observed-egress` now reads a bounded page of the
  newest runs instead of the whole run history; the unused client-side
  `workspaces.getObservedEgress` helper (its own doc claim was false) is
  removed.
- The `credential.revoke` audit row's note for a `github_token` now correctly
  says `RevokeRun` does not call GitHub's revoke endpoint (Wardyn now does,
  from two other doors, but neither can reach a token that was never returned
  to the run).
- Agent, proxy and egress-canary pods on the Kubernetes substrate now set
  `enableServiceLinks: false`; nothing in a Wardyn image reads the
  Service-derived env vars the kubelet would otherwise inject for every
  Service in the namespace.
- The compose Dex IdP now seeds a second identity, `member@wardyn.local`,
  beside `demo@wardyn.local`, giving the compose stack a genuine second
  identity to exercise member mode against with no hand-editing.
- ROADMAP.md's Shipped table now carries rows for v0.7.0 through v0.7.3
  (previously stuck on "Built, awaiting release" for all of them); the demo
  episode catalog's front-door table (README.md) now lists all 23 catalogued
  episodes instead of 14.
- RELEASING.md's version-bump checklist gains DESKTOP.md's two image pins, a
  reminder to add the release's ROADMAP Shipped row, and a reminder to
  regenerate `docs/TEST-GAPS.md`.
- `docs/DEMO-SCRIPT.md`'s and `scripts/demo.sh`'s `wardyn audit` examples are
  updated to the current positional form (`--run` is deprecated); the funnel
  walkthrough's Act 2 table now lists the People step `PHASES[0]` actually
  has.
- Reworded three storage/limit hints that told an operator to type `0` for
  "no limit," where the field renders `0` as blank; a base-URL error message
  no longer claims every git host needs an organization path; an allocation's
  size override is now labelled prospective for an already-provisioned drive.
- Saving Corporate-network settings now surfaces `PUT /site-config`'s four
  advisory signals instead of discarding them.
- The Agents tab now withholds Save over an invalid "Per person" AWS SSO row,
  matching the Git tab's own behaviour.
- Three hand-rolled radio-button groups (Model provider, Git-tab credential
  lanes, Agents-tab credential-source toggle) now support arrow-key
  navigation and a single Tab stop each.

- **The member-mode banner names no tier**: "Viewing as member — **your usual
  role** is paused for this session". The control is offered to both admin tiers
  and both clamp to `member`, which is already why the Exit copy names the mode
  rather than a tier to return to; a security admin was reading a sentence about
  a role they do not hold, on the one surface that is unconditional and on every
  screen. Its ceilings tooltip also now names **secrets** alongside runs and
  workspaces, matching `docs/OPERATIONS.md`'s ceiling 1 and what the code
  actually scopes.

### Known gaps and deferrals

- **On Kubernetes, `disk_mib` bounds the pod's idle main container, not what
  the agent writes.** A run's commands execute in an ephemeral container
  attached to the pod (`internal/runner/k8s/exec.go`), and the kubelet does not
  count an ephemeral container's writable layer toward the pod's
  `ephemeral-storage` limit. The `limits[ephemeral-storage]` 0.7.2 introduced
  does land on the pod and does evict a pod whose main container writes past
  it — but the agent's clone, `$HOME` and `/tmp` writes live in the ephemeral
  container and are metered only by the node's own eviction thresholds. The
  conformance case `EphemeralDiskLimit/OverTheLimitTheRunIsEvicted` has been
  red in CI since 0.7.2 for exactly this reason: its fill runs through `Exec`,
  while a bare pod with the same limit is evicted within a minute on the same
  node. The fix is an `emptyDir` with a `sizeLimit` mounted in both containers,
  which the kubelet does meter — 0.7.5. Until then read
  `ephemeral_disk_enforcement: eviction` as "the pod, not the agent's own
  writes", and the helm README and OPERATIONS sections on `DiskMiB` carry the
  same correction.
- **A workspace create/update that fails at the store can leave library rows
  and audit entries behind.** No transaction seam exists to put both writes in
  one, and no orphan-source heal reconciles them at boot; the residue is
  inert — such a row is never mounted, scanned or cloned.
- **Concurrent MCP permission requests are still decided one at a time** by
  `wardyn-toolgate`; there is no evidence the agent issues them concurrently.
- **A scan that stops at the depth cap still reports high confidence.** A
  distinct "depth capped" note without the confidence demotion needs a new
  wire field.
- **Clamping a member policy up to an operator's `always_deny` ceiling can
  raise the reported Review-rail risk score.** The rationale is an explicit
  availability statement, not a security regression; a reword or a separate
  non-security axis is a later change.
- **A UNIQUE index making the audit spool's replay idempotent is not
  shipped**; the sanctioned path is an operator-run `CREATE UNIQUE INDEX
  CONCURRENTLY` behind a flag, since it cannot run inside a migration
  transaction.
- **A revoke that names a human's email does not reach their UI-sandbox relay
  session** — the session always carries the OIDC `sub`. Revoke by `sub`, or
  use the global `all: true` cutoff; closing this needs a schema change.
- **A role demotion is not caught until the relay session's TTL**, matching
  the SSH gateway's own admin-override staleness bound.
- **An already-established relayed WebSocket outlives a revoke** — killing the
  run is what ends one, the same bound attach and both SSH lanes already
  publish.
- **The sign-in sandbox does not run its own AWS SSO login command
  automatically.** The aws-sso image has no boot-seed/tmux path, and the
  account/role chooser needs an interactive terminal anyway.
- **`POST /runs` is still synchronous.** The console's own launch deadline
  covers the symptom; making run creation itself asynchronous would move the
  CLI's `run --wait` contract, the 201 body and several e2e suites — a 0.8
  design item.
- **Member mode clamps the role, not ownership, groups or SSH.** Runs,
  workspaces and secrets an admin created stay theirs in the mode; the SSH
  gateway's admin override (keyed on the key's own TTL-bound role) is
  unaffected by it; a rolling upgrade's outgoing replica ignores the flag
  entirely, since it rides the existing session cookie with no codec bump.
- **No `role` parameter on `POST /me/tokens`.** A deliberately downgraded
  `wdn_` token was the other way to reach a member lane; `RefreshAPITokenRoles`
  re-stamps every token to the principal's freshly derived role at next
  sign-in, so this needs a `role_pinned` column that does not exist yet.
- **Setting an AWS SSO account/role pin does not invalidate an
  already-stored capture, and there is no admin "revoke this person's
  captured session" route.** Both need owner enumeration in the secret store,
  which the per-user namespace does not have.
- **Tracking the inner per-tunnel MITM `http.Server`s so shutdown actually
  stops them is a design gap**, not closed this release (the drop is now
  counted; the tunnels still outlive shutdown). `mitmHosts` remains keyed on
  the bare host, latent since the only producer today dedupes by bare host —
  two pinned tests turn red the moment either gap becomes reachable.
- **The fresh-install "Skipped" badge fix has its own ceiling**: "once per
  page load" can only distinguish THIS load from the NEXT one — it cannot
  tell "a previous install's mark" from "mine, from 30 seconds ago, before a
  reload". A fresh install where the operator skips Integrations and then
  reloads sees the latch re-arm and wipe its own skip. The correct fix is
  discriminating by install identity, not by page load, which needs a stable
  per-install marker `SetupStatus`/`GET /healthz` do not carry today.
- **`OPERATOR_ONLY_REASON` ("Requires the admin role.") still stands at ONE
  security-tier site**: the workspace detail record pane's tier note, which sits
  inside a `<fieldset disabled={!securityOperator}>` and so admits a security
  admin as well as an admin. It is pinned by an existing test asserting the
  sentence renders twice on that pane (the pane-level note and
  `NewSessionForm`'s own), so moving it is a test change as well as a copy one.
  The other four sites this bullet named through 0.7.4-rc all read
  `SECURITY_ONLY_REASON` at the tip: `live-approvals.tsx`'s panel hint and its
  `ScopeMenu` `Always` reason, the Approvals decide chip, and the run-detail
  cockpit's decide chip.
- **`@mermaid-js/mermaid-cli` stays a console devDependency.** Moving it needs
  a new install location `scripts/check-diagrams.sh` can find `mmdc` at — a
  structural change out of scope for the lane that found it.
- **The kind AWS-SSO walk is a manual proof.** No workflow runs it; a green
  result is evidence only for the tip it was run on. Real SigV4 and real
  Bedrock inference stay owner-hardware-only.
- **The AWS SSO test hatch is production-forbidden, not production-
  discouraged**: no AWS SSO operation Wardyn uses is signed, so Wardyn cannot
  distinguish the fake endpoint from the real one — published as THREAT-MODEL
  residual #45 rather than mitigated.
- **A workspace `write:` requirement can still flip `read_only: true` on a
  mount an admin authored read-only**, deferred: `*bool` carries no
  authorship, so the fix needs mount provenance on the type, and the
  proposed "never flip an explicit true" would break the pinned
  "required defaults to read-write" behaviour.
- **A site-config read failure at dispatch degrades per an explicit owner
  decision, not a residual**: a transient failure is retried once; a
  genuinely unreadable roster still fails the run closed rather than
  guessing.
- **Per-run branch-namespace confinement of the installation token itself is
  not built** — Wardyn deliberately does not request repo-admin access to
  create or hold a GitHub ruleset, so binding the token to a ref prefix stays
  a manual operator step; opt-in verification that a ruleset exists already
  ships.
- **A daemon-side copy of a run's session cast before `StopSandbox` tears the
  sandbox down** is a larger change than this release's residue justifies and
  is not built.
- **A HELD approval chip does not yet degrade past `hold_expires_at`** — the
  field does not exist on the wire yet; the current degrade
  (`waitingHeld(n)` → `waiting(n)`) is a known, pinned ceiling.
- **A pinned "Checking…" state for a in-flight, not-yet-decided check is not
  built this release.**
- **`enforcementFor`'s degraded-read variant is the only one implemented**;
  a fuller generalisation is deferred.
- **A member's `/` landing always re-derives from the server on every load,
  deliberately** — no per-browser flag, because a shared browser handing
  member B member A's landing mark was a real failure mode the design
  considered and rejected.
- **A generic "leaving with unsaved changes" guard is not built**; today's
  screens with a draft either dirty-guard individually or don't.
- **The app-shell header's overflow fix took a label-dropping approach
  instead of `flex-wrap`** — both close the same defect; `flex-wrap` remains
  unexplored as an alternative shape.
- **A deterministic-runner-only e2e case (the kill-request assertion
  branching on run state, not a regex over either outcome) is deferred**
  pending a runner whose kill path is reliably reproducible in the harness.
- **`wardyn-rec`'s SIGTERM handling for the in-sandbox recorder wrapper is
  unchanged** — a run stopped by the reaper or a kill signal in the exec-less
  or Kubernetes dispatch paths can still lose its final upload window; a fix
  needs signal forwarding to the wrapped process plus a bounded flush, judged
  a bigger change than this release's residue.

- **Admin-tier and run-token 5xx sites still carry driver text.** Every door a
  member can reach is converted (above) — a full route-tier enumeration says so,
  not a spot check — and the `writeServerError` chokepoint they route through is
  the shape the rest will take, but 86 sites across 24 files still hand the
  caller raw pgx text. **None of them is member-reachable**: they sit on the
  admin tier or on the run-token `/internal/*` lane, whose readers are an
  operator who can already read the DSN and a sandbox that holds the run's own
  token. The sweep is scheduled rather than urgent: 0.7.5.
- **The killed-run tail-upload grace is still measured from `updated_at`, not
  from a terminal timestamp.** With the keepalive closed (above), the remaining
  re-openers are wardynd's own writes — chiefly the boot reconciler clearing a
  dead run's `sandbox_ref`, which can re-open the five-minute window hours after
  the run ended. The doors it re-opens are upload-only, re-check the run for
  themselves and still require that run's own unrevoked token. Closing it needs
  a `terminal_at` column.
- **A member's Model-access chip reads "Signed out" for a pin-contradicted AWS
  SSO session that is actually live and renewable.** The server deliberately
  grades `expired_signin` on a pin contradiction — a design choice, not a bug:
  `PinMismatch` is `json:"-"`, so the chip is keyed on grading state alone and
  cannot distinguish "expired" from "contradicts the current pin". The `Action`
  line directly beneath the chip is correct and does name the pin, so the member
  is not misdirected; only the chip's own label overstates the session's health.
  Closing this needs either `pin_mismatch` on the wire with its own chip label
  or a state-neutral "Model access · Sign in again", which is a canon decision
  rather than a doc fix. `docs/OPERATIONS.md` now says so explicitly.
- **The e2e `mockSecurityAdminRole` fixture does not splice the
  member-projected `/setup/status`** the way `mockMemberRole`'s
  `mockMemberSetupStatus` does, so seven specs render a security admin against
  an *operator's* status body — the one shape that tier never receives, since
  the redaction keys on `!isOperator`. The behaviour those specs cover is pinned
  elsewhere (vitest over the components, plus one security-admin e2e case), so
  this is a fixture-fidelity gap, not an uncovered surface.
- **`mockMemberSetupStatus` hand-mirrors `redactSetupStatusForMember`'s field
  drops with no parity guard.** It is a deliberate mirror of the server's
  structural drops rather than a re-derivation of its value projections, and
  nothing fails when the Go function drops one more field: the fixture simply
  starts proving a render against a body no server produces.
- **The Runs board's member empty state still speaks to the person who
  launches runs.** `RUNS_MEMBER_EMPTY` reads "Runs you launch appear here" over
  a body pointing at "a workspace your admin has made available to you" — true
  of a member, off-key for the security admin who now reaches the same empty
  state and sees every run on the deployment. Wording only, and a canon
  decision.
- **The union Go coverage floor was ratcheted 65 → 78 against a measured
  78.3 %**, a 0.3-point margin — thin enough that an ordinary refactor can turn
  `cover-check` red on a tree with no test regression in it. Re-measure and
  re-set the floor at the next release rather than treating 78 as headroom.

## [0.7.3] — 2026-09-15

0.7.3 carries the second field report from the same private-endpoint Kubernetes
estate, written inside the first hour of running 0.7.2's new `agent_providers`
roster: 7 findings, 2 confirmed 0.7.1 fixes, and 1 finding that turned out to be a
confirmation, all in the new surfaces — plus two items the campaign's own
end-to-end run turned up. The two 0.7.1
fixes are confirmed on their deployment, with the same credential that broke
each: the captured AWS SSO session whose access token had lapsed now reads "A
captured AWS SSO session is connected. Its access token lapsed at …, and Wardyn
renews it automatically" — same blob, nothing re-captured — and the `auth.failed`
flood "stopped dead at the upgrade. Zero new rows since the rollout," though the
underlying cause (the k8s substrate's new orphan sweep ending a retry loop, not
the coalescing window) means the fold itself stays unexercised. One item is a
confirmation only — the roster's refusals are clear and the closed-set behaviour
("Off: runs naming this agent are refused") is right — no action taken.

**Console and refusal copy is provisional**: every new `400`/`412`/`422` body and
every new console string in this release ships as a frozen DRAFT constant pending
the maintainer's canon sitting. The tests assert through those constants, so
adopting the canon wording is a one-file diff and no behaviour moves with it.

Upgrading from 0.7.2 changes nothing on its own, with the exceptions below: the
roster's new `sso_account_id`/`sso_role_name` pin fields are optional,
`not_applicable` is a new `model_access` state reachable only by the shared
admin-bearer-token caller under a `per_user` row, and the widened CSRF guard
refuses only a cross-origin cookie-authenticated mutation — something no
legitimate CLI, API or console client sends. Two upgrade-visible boot refusals
exist: a deployment started with `-local-operator` / `WARDYN_LOCAL_OPERATOR`
set to the reserved admin-token mechanism principal now refuses to start; and,
where OIDC is configured, `WARDYN_OIDC_REDIRECT_URL` must now parse to an
absolute URL with a host and no userinfo — a bare hostname, a scheme-relative
value, or a `user@host` value that booted clean on 0.7.2 now refuses. Both are
new (see "Security" below). Any other deployment, with no `per_user` roster
row and a redirect URL already shaped that way, answers byte-for-byte what it
answered before.

### Added

- **The agent roster pins which AWS account and role a sign-in may capture.**
  `sso_account_id` and `sso_role_name` on a `bedrock_sso` + `per_user` row, beside
  `sso_start_url` and admin-owned for the same reason — the sign-in proposes, the
  roster disposes. Optional (a single-account tenant never had this problem) but
  set as a pair: pinning the account alone still leaves the role picked for
  whoever signs in. A new SSO entitlement granted by a cloud team cannot move a
  pin. See `docs/OPERATIONS.md`, "AWS SSO per person".
- **A chooser when nothing is pinned and the session reaches several accounts.**
  The login sandbox runs on the operator's own attach terminal, so the helper
  asks — numbered accounts, then roles in the chosen one. With no terminal to ask
  on, it refuses and names the accounts it reaches, so an admin can pin one. A
  portal that cannot be reached says so, rather than reading as a wrong pin.
- **A failure marker on the login terminal.** `wardyn: aws sso credential
  rejected: <reason>` — the counterpart of the existing success marker. A refused
  upload previously logged to stderr and printed nothing, so the console's login
  pane waited out the sandbox's 30-minute idle cap on a credential already
  refused.
- **`harness.credential.refused` audit action.** Every refusal on the SSO-token
  upload route now leaves a row, with a `reason` from a fixed vocabulary
  (`blob_shape`, `field_unsafe`, `field_shape`, `region_mismatch`,
  `start_url_mismatch`, `account_role_pin_mismatch`, `model_account_mismatch`,
  `unstamped_scope`, `already_captured`, `stamp_unreadable`, `store_error`) and
  never sandbox-chosen text. `field_shape` is the capture door's own account-id/
  role-name shape check — the same `^\d{12}$` / IAM role-name rule the roster's
  save-time pin already enforced, now applied to whatever account/role a
  sign-in actually captured, so a malformed value cannot reach one door
  refused and the other accepting. `agent_provider.write` gains `pins`;
  `harness.login.started` gains `sso_account_id`/`sso_role_name`. See
  `docs/AUDIT-ACTIONS.md`.

### Fixed

- **The per-user AWS SSO lane no longer signs with `AccountList[0]`.** A cloud
  team granting an unrelated SSO entitlement used to insert an element at index 0
  and silently re-point the AWS identity every run in the deployment authenticated
  as — with no Wardyn change, no configuration change, no diff, and no audit row
  naming it. It surfaced as a `bedrock:InvokeModel` 403 retried ten times inside
  an agent terminal, and blocked `credential_source: per_user` completely with no
  workaround (the helper read no override, and the stored blob could not be
  corrected by hand — the reserved harness secret name is sealed by pattern).
  The pin above is refused at CAPTURE and at SIGN-IN, both failing closed: at
  SIGN-IN the helper verifies the pin against the SSO portal and refuses
  rather than falling back to the first entry; at CAPTURE the upload is bound
  to the pin as it read AT LAUNCH, stamped on the run's own
  `harness.login.started` row, so a roster edit mid-sign-in cannot re-point a
  capture already in flight. At ROSTER SAVE, a pin that disagrees with the
  account the configured `WARDYN_BEDROCK_MODEL` ARN lives in is ACCEPTED, with
  a one-time warning logged rather than a 400 — the admin's explicit pin is
  the deliberate answer, taken as written, and the row still saves. The same
  disagreement also raises a `warn` on the `bedrock_provider` setup check,
  naming both accounts and the two ways to resolve it (repoint the pin, or
  confirm the model really is shared across accounts) — so the deliberate
  override is visible on the setup page an admin actually looks at, not only
  in the daemon's own journal. Model-account validation applies only when
  nothing is pinned: an uploaded session for a different account than a
  full-ARN `WARDYN_BEDROCK_MODEL`, with no pin set, is refused and named; a
  pinned capture is bound to the pin alone, and a bare cross-region
  inference-profile id names no account either way, so that check is skipped
  rather than failed. And a run that would dispatch a Bedrock credential now
  refuses to start — rather than being served the operator-wide credential —
  when the roster cannot be read at all, with an audited `run.create` failure
  row naming the cause; every non-Bedrock, non-model and login-box dispatch is
  unaffected.
- **The admin's own AWS sign-in door stopped asking for a start URL it would
  throw away.** Of three call sites that open the harness-login dialog, only
  Settings → Model provider failed to pass `startURLManaged` under a `per_user`
  row — so an admin signing in from the door muscle memory was asked to type
  the org's AWS access portal URL again, minutes after saving it on the Agents
  tab, and `handleHarnessLogin` silently discarded it in favor of the roster's
  own
  stored, admin-owned `sso_start_url`. The card now derives `per_user` the same
  way the Agents tab does, including that a DISABLED `per_user` row is graded as
  not-per-user (`enabled !== false`) — before this, a disabled row hid the
  start-URL field the server still required and graded `model_access` in the
  wrong namespace.
- **A `per_user` Bedrock admin was pointed at three dead ends.** `bedrock_provider`'s
  remediation text offered a read-only `~/.aws` mount, a `bedrock-api-key` bearer
  secret, or `aws-access-key-id`/`aws-secret-access-key` secrets — all three
  skipped outright by `per_user` credential resolution, which reads only the
  caller's own AWS SSO session. The row now names the one action that can
  actually succeed: sign in to AWS yourself. `llm_provider` had the matching
  contradiction — "No model/harness provider configured (optional)" two rows
  above a `bedrock_provider` row saying Bedrock IS configured — and now says a
  matching per-person sentence instead.
- **Declaring a per-person lane and signing in to it are linked now.** Nothing on
  the Agents tab used to say "now sign in" after a `per_user` row was saved, and
  nothing on Settings' Model provider card said the lane was declared elsewhere —
  a member's Getting Started already got this right. A tinted panel at the top
  of the Agents tab's claude-code row now says the lane is per person, including
  the admin's own, and that saving only declares it; the Settings card gets the
  mirror sentence, naming where the lane lives and whose sign-in its badge reads
  (and, beside the Bedrock bearer-key field, that the key is stored but unused
  while the lane is per person, rather than hiding a secret that is still a
  secret). Saving the Agents tab also no longer waits for the next reload to
  reflect a new pin or mechanism: it refreshes `model_access`/`harnesses`
  directly instead of re-fetching the whole screen, which used to discard an
  admin's unsaved Git/Storage edit on the same page. Both the banner and the
  login pane's managed-start-URL mode follow the SAVED roster row rather than
  whatever is sitting unsaved in the draft, so typing a mechanism/credential-
  source change and opening sign-in before clicking Save cannot show a per-person
  affordance for a lane the server does not yet know is per-person, or the
  reverse.
- **A per-principal state read through the admin bearer token reported the
  TOKEN's own state, not a caller who could act on it.** Under `per_user`,
  `GET /setup/status`'s `model_access` used to read `{"state": "not_configured",
  "action": "Sign in to AWS"}` for the shared admin token — which owns no AWS SSO
  session and never will — while the operator's own browser session was fully
  signed in and the console badged the lane Connected. `model_access.state` now
  reads `not_applicable` (no action, no deadline) for that caller, but only when
  nothing is captured in that shared namespace: a session an earlier admin-token
  capture already holds is graded normally, because dispatch still serves it to
  admin-token-created runs. On Settings' Model provider card specifically,
  `not_applicable` renders NOT connected with its own per-person note, offers
  no sign-in button — the door `POST /setup/harness-login` already refuses
  for this exact caller (see "Security" below) — and the note itself carries
  no imperative: it says the caller is a mechanism with no sign-in of its own,
  never "sign in" to a caller that cannot. That fallback — read by every
  OTHER shape
  (a `shared` row, a disabled row, no roster at all) — no longer badges
  Connected merely because a Bedrock row exists (region or model set): it now
  requires an ACTIVE credential lane (a bearer key, a captured SSO session, a
  host `~/.aws` mount or static keys, in that order), the same "green chip
  over an absent credential" fix the 0.7.1 report raised and the member's
  Getting Started chip already had. See "Security" below for the matching
  capture-side refusal.
- **The `Fence` / `NetworkPolicy: enforcing` header chips are gone.** Both were
  deployment-wide facts fixed at boot, occupying the header's most valuable real
  estate on every screen for every user while conveying nothing after one read —
  worse than redundant for a member, for whom `Fence` is internal vocabulary for
  a tier they did not choose. This is a move, not a hide: the same NetworkPolicy
  verdict and the Fence/Wall/Vault tier matrix already lived on the admin setup
  page's Environment step, reviewed and screenshotted for auditors, with the
  canary's own reasoning and the unenforced-CNI warning beside it — that page is
  now the only place to read posture, and the header carries only what varies.
- **"Start a run like this one" now reaches every terminal run, not only a
  killed one.** 0.7.2 built the clone CTA but put its only door inside the
  killed-run "What happened" panel, which renders nothing for a run that
  completed successfully — so a run that succeeded, or was auto-stopped, or
  failed to build its image, had no way to launch an identical one without
  retyping task, agent, barrier and policy by hand. The door **moved**: it is
  now on the run header for any terminal run (the 0.7.2 CHANGELOG entry, which
  sent the reader to the killed-run panel, describes where it launches FROM,
  not where the button now lives), and a matching "Start a run like this one" item
  joined the Runs-list row kebab, on both the board and the table. The
  failure block keeps its own advice with no second door. Neither door trusts
  an empty audit read any more: an older run, a pruned trail, or a non-owner's
  empty response used to fall through to wizard DEFAULTS indistinguishable
  from a faithful clone — one shared helper now refuses (a toast, no
  navigation) on BOTH the run header and the Runs-list kebab rather than
  launch one silently degraded from either door.
- **The setup screen's first paint could cost six seconds, or more, on a wedged
  Windows interop.** `GET /setup/status` ran the host-proxy sweep
  (`setup.DetectHostProxy`) on the request goroutine, and a 30s memo only ever
  helped the SECOND caller — so the first call after every daemon boot paid
  the sweep's full cost. On WSL that sweep shells out to `powershell.exe` and,
  only if that answers nothing, `netsh.exe`; each probe's own 3-second timeout
  bounded the CHILD process but not the call itself — a grandchild process
  that kept holding the output pipe open could stretch one probe from 3s to
  30s — so a wedged interop cost anywhere from six seconds to unbounded, and
  the console's own first paint waited on that call. Latent since 0.4.2 —
  `internal/setup/` did not change this release — and surfaced by 0.7.3's own
  end-to-end run, where it read as "the first test of every spec file is
  slow" (17 of 26 spec files, each on its opening page-render assertion)
  because every later poll in the same run rode the warm memo. The sweep now
  runs BEHIND the request AND is bounded on both ends: each probe's pipe is
  closed after its timeout (`cmd.WaitDelay`), so a grandchild can no longer
  hold it open, and a background sweep still hanging past 10s is abandoned,
  logged once (`WARN`, not once per poll), and retried on the next call rather
  than left to block that memo forever. `/setup/status` always returns the
  last-known value immediately. A `make setup`-seeded corporate install never
  reads an empty "nothing detected" window at all — its host proxy is decoded
  in-process from `WARDYN_HOST_PROXY_B64` with no subprocess, so the first
  call answers synchronously and correctly. The setup screen's Re-check button
  now sends `GET /setup/status?recheck=1` (operator-only), which drops the
  memo's freshness and forces a real re-detect — before, Re-check only
  re-fetched whatever the 30-second-old memo already held, so a proxy just
  configured could not be made to appear no matter how many times it was
  pressed. The forced call waits up to 2 seconds for that fresh sweep to land
  before answering, so a single press now sees the new value rather than
  needing a second press a moment later; a wedged host still answers within
  the 2 seconds, with the last-known value, exactly like an ordinary poll.
  Repeated presses cannot pile up sweeps: at most one forced re-detect is
  honoured per `hostProxySweepDeadline` (10s), and a press inside that window
  reads the sweep already in flight rather than starting another.

### Security

- **The cross-origin guard on a cookie-authenticated mutation now applies in
  every mode, not just LocalMode.** 0.7.2 named this as an open gap: the
  `Origin` check that refuses a cross-site state-changing request lived in the
  local-mode arm of `internal/api/http.go`'s auth middleware, so an SSO
  deployment relied on the session cookie's `SameSite=Lax` alone — a browser
  rule rather than ours, and one that does not bind a same-site sibling on a
  shared parent domain. `sameOriginOrRefuse` (`internal/api/csrf.go`) now runs
  at the top of the OIDC session branch, before any handler: a present
  `Sec-Fetch-Site` header refuses outright unless it reads `same-origin` or
  `none` — so `cross-site` refuses as before, and so now does `same-site` (a
  sibling host on a shared parent domain, the browser label a same-site
  sibling actually sends, and the one this guard's own justification names),
  which used to fall through to the Origin rule and pass on a request that
  omitted `Origin` entirely. Otherwise, a PRESENT `Origin` must name either the
  request's `Host` or the host of `WARDYN_OIDC_REDIRECT_URL`
  (the second name is what a TLS-terminating ingress needs — which is also why
  the scheme is deliberately not compared), and a malformed, opaque (`null`) or
  host-less `Origin` fails closed. Refused with `403` and
  `cross-origin state-changing request rejected (CSRF guard)`.
  **Nothing changes for a CLI, a CI job or the API.** A request that carries
  neither header passes — that is not a browser, and it holds no ambient cookie
  to forge — and the admin/API-token lane is exempt by construction: a bearer
  token is not something a browser attaches on an attacker's behalf, and that
  lane never enters the session branch. The local-mode arm now compares that
  same parse against its own `r.Host` (host AND port, not merely loopback) in
  place of its old loopback-only rule, and shares the Fetch-Metadata refusal
  and the refusal sentence with the OIDC arm, so the two modes cannot drift;
  both are table-tested for the first time (`internal/api/csrf_test.go`), and
  every registered mutating route
  is fenced by the `chi.Walk` route matrix rather than one sample route. Every
  refusal is audited on the existing `auth.failed` action with `reason`
  `cross_origin_refused` — no new action. **One local-mode behaviour change:** a
  page at `http://localhost:<port>` posting to `http://127.0.0.1:<port>` is now
  refused, because browsers treat the two loopback aliases as two different
  sites; the console's own fetches are same-origin relative URLs, so nothing
  Wardyn serves is affected. **Two boot refusals:** `WARDYN_OIDC_REDIRECT_URL`
  must now parse to an absolute URL with a host and carry no userinfo — its
  host is the second same-origin name, and a host-less, scheme-relative or
  `user@host` value used to surface as a console-wide "CSRF guard" 403 instead
  of a boot error. And, separately, local mode's `-local-operator` /
  `WARDYN_LOCAL_OPERATOR` may no longer be set to the reserved admin-token
  mechanism principal — a deployment that did would boot clean and then have
  `POST /setup/harness-login` refuse the very seat boot just accepted, under a
  `per_user` roster row (see the harness-login refusal below).
- **Browser PTY attach works behind a TLS-terminating ingress.** The attach
  WebSocket (`internal/api/attach.go`) used to accept `websocket.Accept`'s
  default same-origin check, which authorises the request `Host` alone — so in
  exactly the ingress shape above, where the browser's `Origin` is the public
  console name and `Host` is the internal one, browser attach was already
  being refused. The origin decision now moves OUT of that library check and
  into `attachOriginRefused` (`internal/api/csrf.go`), an explicit host
  comparison against `r.Host` or the `WARDYN_OIDC_REDIRECT_URL` host — the
  same two names the CSRF guard accepts — rather than the library's own
  `OriginPatterns`, whose glob matching mangles an IPv6-literal host in both
  directions at once. `websocket.Accept` is called with
  `InsecureSkipVerify: true` because that origin check already happened one
  line above it. A refused attach is audited on the same `auth.failed` /
  `cross_origin_refused` reason as the REST guard. With SSO unconfigured the
  second name is empty and only `r.Host` is accepted — behaviour unchanged.
- **The shared admin bearer token can no longer capture an AWS SSO session under
  a `per_user` row.** `POST /setup/harness-login` refuses (`422`) when the
  caller is the admin-token mechanism principal, the row is `per_user`, **and**
  OIDC is configured: every login made with that token lands in one namespace
  (`owner: "admin-token"`) and would overwrite the last person's capture. The
  guard fires only with OIDC configured — with no OIDC there is no console
  human or `wdn_` token to redirect to instead, so the admin token stays the
  only working `per_user` capture path there, and its `model_access` correctly
  reads `not_applicable` rather than a `not_configured` action it cannot take.
  A LocalMode operator seat, a `shared` row, and a real per-user `wdn_` token
  are all unaffected. The refusal is audited on `authz.denied` with reason
  `harness_login_mechanism_principal`.

### Known gaps and deferrals

- **Wardyn cannot see a Bedrock 403 and turn it into a once-per-run memo, and
  0.7.3 does not pretend to.** Under `per_user` AWS SSO the proxy MITMs only the
  Anthropic/OpenAI hosts; `bedrock-runtime` joins the MITM set on the bearer lane
  only, so an SSO-mode Bedrock call is an opaque CONNECT tunnel (SigV4, no CA). A
  `builtin:bedrock-access-denied` memo would be inventing a verdict from bytes the
  proxy cannot read, and a dispatch-time `GetRoleCredentials` preflight would
  prove only that the role is assumable — it would have returned 200 for this
  exact bug — while adding `portal.sso.<region>` to control-plane egress on a
  private estate. So the fail-fast is UPSTREAM instead: the helper refuses a
  sign-in that cannot reach the pin, and the upload refuses a blob that
  disagrees with the pin, or — with nothing pinned — with the model's account.
  A pin that disagrees with the model's account is the one shape the roster
  now WARNS on rather than refuses, on the reasoning that an explicit pin is
  the admin's deliberate override; every other wrong-identity shape still
  never starts a run. A Bedrock extractor plus SigV4 MITM is a 0.8+ item.
- **The pin binds at LAUNCH, so a sign-in already in flight keeps the old pin.**
  An admin who changes `sso_account_id`/`sso_role_name` while somebody's login
  sandbox is still alive does not re-point that capture; it is validated against
  the pin as it read when the sandbox launched — the same rule the credential
  scope already follows (re-reading the live roster at upload time is what let a
  mid-run roster edit re-point a member's capture in 0.7.1). The person's next
  sign-in picks up the new pin.
- **A bare Bedrock model id names no account, so the model check cannot fire.**
  `WARDYN_BEDROCK_MODEL` is passed verbatim and is most often a cross-region
  inference profile id (`us.anthropic.claude-…`), which carries no account field.
  The account-vs-model comparison is SKIPPED there, not failed — give the full
  `arn:aws:bedrock:<region>:<account>:…` ARN to get it. `wardynd` logs once at
  boot when the value *looks* like an ARN and still names no account, so a
  typo is a warning, not a silent skip.
- **Not proven on this hardware: a real IAM Identity Center tenant with two
  account entitlements.** The account/role pin is proven against
  `test/awsssofake` (which enforces the real `x-amz-sso_bearer_token` contract
  and, from 0.7.3, scopes `ListAccountRoles` to the requested account) and
  against real botocore. What remains owner-hardware-only is a live
  `aws sso login` against a real tenant where the person is entitled to two
  accounts — that AWS's `ListAccounts` ordering, its error shape for an
  unentitled `account_id`, and its role pagination match the fake's. The pin
  fails CLOSED in all three cases (a refusal with the fail marker, never a
  silent wrong-account capture), so the residual is a false refusal, not a
  wrong credential.
- **The AWS SSO-blob credential is not raised to CC3.** `RequiredConfinementFloor`
  raises a run to CC3 for a grant-delivered credential, but the SSO cache blob is
  delivered at dispatch, after that floor is already computed, so a CC1/CC2 run
  still receives it. 0.7.2 narrowed the blast radius to one person under
  `per_user`; 0.7.3 does not raise the class. Deferred, on the owner's ruling
  that it would fail closed on the reporting estate's CC1-only host: 0.8 ships
  it warn-first instead of a hard floor.
- **A visible chip for `not_applicable`.** The state exists so the admin-token
  principal's own `model_access` can say what it is instead of guessing, but no
  console surface renders a chip for it yet — the Agents tab and Getting Started
  both correctly render NO chip (never a default label) rather than a wrong one.
  Deferred to the canon sitting: a frozen `AGENTS` row is what a rendered chip
  needs.
- **The per-run UI-gateway cookie is outside the widened CSRF guard.**
  `wardyn_ui_sess` (`internal/api/uigateway.go`) authenticates a relayed sandbox
  app on a SEPARATE listener and origin (boundary B10) through its own
  middleware, which never calls `sameOriginOrRefuse`. Widening to it is its own
  change with its own threat model; it is named here rather than implied.

## [0.7.2] — 2026-09-12

0.7.2 carries ONE unplanned feature — an admin **Workspace Providers** surface —
plus the follow-ups from two customer field reports on a private-endpoint
Kubernetes estate. Carrying a feature onto `release/0.7` breaks one clause of
[RELEASING.md](RELEASING.md), and it is recorded there as a dated exception
rather than as a rule change.

**Console and refusal copy is provisional**: every new `400`/`412`/`422` body and
every new console string in this release ships as a frozen DRAFT constant pending
the maintainer's canon sitting. The tests assert through those constants, so
adopting the canon wording is a one-file diff and no behaviour moves with it.

Upgrading from 0.7.1 changes nothing on its own: with no provider rows and no
agent roster written, every predicate this release adds is a no-op and the
deployment answers byte-for-byte what it answered before.

### Added

- **Workspace Providers — which git hosts a run may clone from, and how big its
  scratch may get.** `SiteConfig` gains a `workspace_providers` block: git-provider
  rows (`github` | `azure_devops`, allowed HTTPS base URLs including self-hosted
  GHES/ADO Server, and the credential lanes permitted there) plus two storage
  ceilings. It is edited through its own admin-only
  `GET`/`PUT /api/v1/workspace-providers` (ETag/412) and is also writable through
  `PUT /site-config` — the CLI/MDM door a laptop re-applies on every boot — where
  a block the body does not NAME is carried forward rather than cleared, and `{}`
  is the clear form on both doors. Zero DDL.
  - **Admission asks one question at ten doors.** A repository reaching a clone
    passes `admitRepoURL` at workspace create, update, scan and build; at
    `POST /sources`; at run create over the RESOLVED spec (so a hand-authored
    inline policy passes through it too); on the legacy single `repo` field; on
    `devcontainer_repo`; and in the two SERVER-SIDE launchers, record and source
    scan, which create runs without passing either request-path gate. A census
    test over `Store.CreateRun`'s callers is what keeps that last pair honest.
    The refusal splits by tier: an operator's `422` lists the allowed addresses
    (or names the claiming row, or says the row is switched off), a member's `403`
    names nothing but the fact.
  - **Providers mint nothing; they veto.** A credential lane a row does not permit
    drops its wiring at all five grant sites — and for `pat`, the ADO egress
    bundle that arm would have added with it — and says so on the `201`, with an
    audit row. A host admitted only through the legacy `scm_hosts` list keeps
    working for one release and says so on EVERY response that can carry a
    warning — the three onboarding doors (`POST`/`PUT /workspaces` and
    `POST /sources`, so the console's Add-workspace dialog tells the admin who can
    enable a provider row, at the moment they onboard the source) as well as the
    run that clones it — with an audit row at all ten doors.
  - `scm_hosts` is never written and never folded: `GET /site-config` projects a
    read-only `effective_scm_hosts` union, so the console never re-implements the
    claim table. New audit action `workspace_provider.write`; `site_config.write`'s
    datum gains `git_providers`, `storage_configured` and, when the body named the
    block, `sources_no_longer_admitted`, so an MDM-applied narrowing is reviewable
    with nobody watching a console.
  - A seventh capability kind, **`workspace_provider`**, bounds which provider row
    a member's work may come from, at the six of those doors a member can reach.
    NARROWING, like every kind before it: the unenforced default stays allowed and
    a deny row still bites before anyone enables the kind.
  - **The Git host card retires.** Its three git credential lanes render inside
    the provider row they apply to, on the new `/providers` screen (admin-only, no
    nav item), reached from the funnel's new `providers` step and the Settings
    card that replaces it.
- **An agent roster, and one model-access lane per agent.** `SiteConfig` gains an
  `agent_providers` block — per agent: enabled, ONE mechanism drawn from the lanes
  that already exist at dispatch, and whether that credential is `shared` or
  captured `per_user` — with its own `GET`/`PUT /api/v1/agent-providers`, shaped
  byte-for-byte on `workspace_providers`. It exists because availability was an
  image map with no auth semantics: "Claude Code is offered here, authenticated via
  AWS Bedrock SSO, one credential per person" was not a statement the product could
  hold. Run create refuses a named agent with no enabled row (`422`), and the
  record launcher asks the same question before it launches, because that path
  hardcodes its agent and bypasses run create entirely. `credential_source:
  per_user` is `bedrock_sso`-only — the one mechanism with a per-principal capture
  path — and such a row must carry the admin-owned `sso_start_url` every principal
  signs in against, so a member's sign-in can never bind a foreign IdP. The
  member-safe carrier is three fields per row on `GET /setup/status`; the start URL
  is in neither that nor the new `agent_provider.write` audit row.
- **Ephemeral disk is enforced on Kubernetes.** A run's `disk_mib` now becomes the
  agent container's `resources.limits[ephemeral-storage]` (with a small fixed
  256Mi request, so scheduling is unchanged apart from a node genuinely short on
  allocatable ephemeral storage newly rejecting the pod), so the kubelet bounds
  the writable layer
  the clone, `$HOME` and every ephemeral workspace target live on — previously
  accepted, logged and ignored. Over the limit the pod is **evicted** and the run
  fails with `Evicted: Pod ephemeral local storage usage exceeds…`, naming the
  limit. This is eviction, not a filesystem quota: the kubelet measures
  periodically and kills the pod; it does not refuse the write. `StorageEnforcement`
  gains `eviction` to say exactly that, and both substrates' disk caps now report
  one word (`filesystem`, `eviction`, `none`) on the admin setup status and on the
  Workspace Providers screen. Docker's `applyDiskQuota` is unchanged, including its
  fail-closed overlay2-on-non-xfs arm.
- **Storage ceilings, applied at one site each.** `storage.ephemeral.default_disk_mib`
  fills in for a run that asked for no size, and the smaller of
  `storage.ephemeral.max_disk_mib` and the profile's `max_ephemeral_disk_mib`
  bounds one that asked for too much — both at dispatch,
  the one seam every lane passes after every widening phase. A ceiling bounds a
  request and never invents one: a run with no `disk_mib` and no org default stays
  unbounded. `runner.Resources` carries the provenance of its own number
  (`disk_mib_filled`, also on `run.policy.effective`) because the two are treated
  differently on a Docker host that cannot keep the cap — an org default runs
  uncapped with a warning, a policy-authored size keeps today's refusal.
  `GovernanceLimits` gains `max_ephemeral_disk_mib` and `max_drive_size_mib`
  (0 = unlimited, no DDL); the drive ceiling is enforced at the resolver fold and
  refused `422` at the admin write boundary.
- **Recording metadata on the runs list, opt-in.** `GET /runs` and
  `GET /runs/{id}` project `has_recording`, `recording_bytes` and
  `recording_duration_sec` behind `?include=recording_meta`, so the Recordings
  screen builds its whole library from one list call and fetches a cast only when
  a viewer presses play — replacing a per-run download of the entire document
  (39.8 MB measured for 200 runs). Opt-in because a stat-and-tail per run costs
  real time on the same endpoint that backs the Runs board's 3-second poll. The
  screen's own pagination control is NOT in this release; it is filed for a mock
  round.
- **A person's AWS sign-in is their own credential.** An agent roster row set to
  `credential_source: per_user` makes the AWS SSO session a run authenticates with
  the PRINCIPAL's, not the deployment's. One scope is threaded through: under
  `per_user`, credential resolution reads that principal's own namespace and SKIPS
  the bearer, host-`~/.aws`-mount and static-key arms outright, because all three
  are operator reads — a member with no session of their own is not-configured,
  never quietly served the admin's. `POST /setup/harness-login` widens from
  admin-only to any signed-in human who holds an enabled `per_user` row's agent
  capability, since an admin-only door would leave a member no route to model
  access at all; the launch uses the ROW's access portal and ignores the request's,
  so a capture can never be bound to an IdP and account of the caller's choosing.
  `GET /setup/status` carries a five-state `model_access` per principal —
  registration-first, because the access token lives an hour and a probe keyed on
  it would make "expiring" permanent — and the member redaction KEEPS it, because
  it is why a member's chip can stop reading a deployment fact that showed green
  over their own lapsed session. `harness.credential.captured` and `.refresh` carry
  the owner and the credential source, so "whose credential" is answerable from the
  trail. **What this release does NOT do** is floor the confinement class: the SSO
  cache blob is not a grant, so `RequiredConfinementFloor` — which raises a
  grant-delivered credential to CC3 at create — never sees it, and the blob is
  resident in the sandbox at whatever class the run requested. `per_user` narrows
  the blast radius to one person; the class is a 0.7.3/0.8 decision, and the
  threat model carries it as a stated residual rather than as an implication.
- **The console half of the roster: four surfaces and one widget.** Agents get
  their own tab on `/providers` — enabled, the one model-access mechanism, and
  whose credential it is, over the server's own roster rather than a row the
  client invented. New Run's agent picker is drawn from that live roster instead
  of a compiled-in list, so an agent this deployment does not offer is not
  offerable; the launch response's warnings render INLINE on the New Run screen
  and hold the navigation behind an explicit "Open run", because a warning that
  scrolls past on the way to the run detail was never read. A member whose agent
  row is `per_user` gets a **"Sign in to AWS"** CTA beside their model-access
  chip, which is the whole point of the per-principal lane having a door a member
  can reach. And the run detail gains an **Effective policy** widget over
  `run.create`'s `clamp_warnings` — every way launch narrowed what the caller
  asked for, read back on the run itself; its layout id is in the closed set
  `internal/api/ui_layout.go` pins, so the widget cannot be renamed without the
  server agreeing.
- **An org switch for drives, and two drive ceilings.** `storage.user_drive` is the
  org's half of the drives feature and nothing read it until now. The switch is
  asked FIRST, ahead of the per-profile door, because the two answer different
  questions: `disabled` says this install offers no drives at all — every drive and
  allocation write answers `422`, a run carrying `drive.enabled` is refused, and NO
  `authz.denied` row is written, because no profile denied anybody. Nothing is
  deleted, `DELETE` keeps working so an offboarding is not blocked by a switch, and
  turning it back on restores exactly what was there. The ceiling is two numbers
  binding in different places: the deployment's `max_size_mib` is refused `422` at
  the admin write boundary, while a profile's `max_drive_size_mib` cannot be
  refused at a write at all (the profile binding a subject is claims-resolved and
  unreadable from the row) and is CLAMPED where both facts are in scope, folded
  with the deployment's in one `min()`. A lowered ceiling never shrinks anything:
  on Kubernetes a PVC request cannot be reduced in place, so it is drift, and drift
  is a warning that already existed.
- **The profile editor's three number rows.** `max_concurrent_runs`,
  `max_ephemeral_disk_mib` and `max_drive_size_mib` had no control in the governance
  profile editor even though the fields, the dispatch clamp and the chip all
  shipped. One row shape serves all three (`0` = unlimited, stated in every hint),
  and the ephemeral row carries the Docker-uncapped warning read from the same
  `/setup/status` call the Storage tab uses rather than a second copy of the string.
- **Three console surfaces over facts that were already on the wire.** A repo
  source the provider policy does not admit now dims its `/workspaces` row and
  says why — naming neither a base URL nor a row id, the same disclosure rule the
  refusals follow — and repeats the reason on the New Run workspace picker instead
  of offering a workspace the launch would refuse. New Run's drive block renders a
  reason line in place of the checkbox for each of `/me`'s four
  `user_drive_unavailable` tokens, so a member learns before they launch that the
  drive they expect will not mount. And an identity-affecting edit to an
  already-allocated drive opens a confirm dialog over the server's own re-home
  text rather than failing with a bare `409`; confirming retries the same save.

### Fixed

- **A repository the run cannot clone is named on the 201, not dropped in
  silence.** The reserved user-drive target is refused at the write door
  (`400`), but a policy STORED before that rule is handed to the run verbatim —
  `resolvePolicy` does not re-validate it — so a `workspace_repos` row pointing
  into `/home/agent/drive` produced a `201`, an empty `WARDYN_REPOS` and an agent
  hunting for a repository nothing ever cloned. The drop still happens (a clone
  into the drive would write somebody else's repository into a member's
  persistent storage), and it is now stated: a warning on the launch response
  naming the repository AND the refused target, plus the `slog.Warn` the
  neighbouring dest-collision drop has always had. Inline policies are unchanged
  — they still `400` before the run exists.
- **A run whose agent exec id could not be persisted fails now, honestly, instead
  of being killed later as a mystery.** The post-`Exec` `SetRunAgentExecID` write
  was best-effort (`_ =`); the boot reconciler reserves the resulting empty value
  for "the dispatcher died before it ever exec'd the agent", so a lost write made
  a healthy, running agent indistinguishable from a corpse — finalized `FAILED`
  and torn down on the next restart, while this dispatch's `run.exec` audit row
  said `success`. The write is now retried once and, if it still cannot land, the
  run fails at dispatch through the same path a failed `Exec` takes: the sandbox
  stopped, the run `FAILED` with a hint naming the write, and a `run.exec` row
  carrying `outcome=failure` and the store error. The happy path is unchanged.
- **The console stops guessing who you are when `/me` does not answer.** It used
  to render the full admin nav off the fail-open default, so a human correctly
  DENIED at login still saw Policies, Governance, Permissions, Secrets and Audit —
  indistinguishable from an authorization breach, and read as one. The sidebar is
  now gated on identity being RESOLVED rather than merely settled: settled-but-
  unknown draws neither nav, says it could not confirm who you are, and offers a
  Retry that re-fires the call. The gate is on the ROUTE SHELL, not the nav alone
  — `/settings` and every admin route painted their operator controls to an
  unresolved identity while the sidebar hid the links to them, so an unknown
  identity now renders no route at all and the account menu drops Settings. The context's own defaults stay open, deliberately
  — the answer to a guess is not a different guess. Server authorization was never
  affected by any of this.
- **A SiteConfig change now says it does not reach a run already going.**
  `internal_hosts`, `upstream_proxy_no_proxy`, `trusted_ca_pem` and the egress
  redirects are compiled into the sidecar's config at dispatch and read once at
  startup, so an operator could fix the exact field a denial named and watch the
  same run fail nine more times with the identical message. `PUT /site-config` now
  answers `applies_from: "next_dispatch"`, the console repeats it on BOTH halves
  of the Network step's saves — the upstream proxy and the egress redirects, which
  are compiled at dispatch the same way — and the egress denial itself carries the
  lifetime clause. Live
  sidecar reload stays out of scope: a running sandbox's egress posture must not
  change under it with no audit row to show why.
- **The egress "N held" badge counts what is actually held.** It counted
  `egress.pending` rows on an append-only audit trail, so it claimed holds that had
  been decided an hour earlier while the Approvals tab correctly showed one. One
  `isHeld` derivation now backs every copy of the count; the audit rows still
  render as history, carrying their `approval_id`.
- **A run's end cancels the questions nobody can answer any more.** Migration
  `0062_approval_cancelled` adds the terminal state `CANCELLED` to the
  `approvals.state` CHECK — distinct from `DENIED` (a human refused) and `EXPIRED`
  (a sweeper aged it out), because nobody decided this one. All THREE terminal
  writers cascade — completion, failure/kill, and the idle reaper's `STOPPED`,
  which is the one an idle run reaches precisely because its agent is parked on a
  hold — and so does a run failed while already `RUNNING`, the arm three dispatch
  call sites take with the sandbox and the sidecar up. A source-scanned census
  freezes the three, so a fourth writer reds a test rather than stranding a queue.
  The cascade rides the same CAS a human decision uses, so a concurrent human
  decision wins and is never overwritten; it emits ONE `approval.cancelled` audit
  row per transition, carrying the count; and because a best-effort cascade can
  still lose a race, deciding a terminal run's approval now answers `409` and
  CAS-cancels the row instead of replaying an `always` approve into the workspace
  allowlist on behalf of a sandbox that is gone. `approvals.state` joins
  `closedEnumChecks`, DERIVED from the Go constants rather than hand-listed, so
  the Go set and the database are compared from now on. Every reader that treated
  an unknown state as "keep waiting" — the toolgate and the git helper both did,
  which would have hung the agent until its timeout — learns the value.
- **A killed run can be started again as a new one.** The killed-run panel told the
  operator to start a new run and gave them nothing to start it with, so they
  retyped task, agent, barrier and policy — hardest exactly in the loop above,
  where "kill and start an identical run" is the correct response. "Start a run
  like this one" prefills from the run row AND from its `run.create` audit event,
  which is the only place `task_mode`, `interactive_start`, `seed_auto_tools` and
  `tool_approvals` live — and it names the one thing it cannot carry, since an
  inline policy is never stored. It is a NEW run request: credentials and approvals
  are minted fresh and the ceiling re-runs at create, so a cloned admin run under a
  member clamps honestly.
- **The decision buttons disappear on a run that has ended.** Approve and Deny are
  removed, not disabled, once the run is terminal, and the archived `CANCELLED` row
  says plainly that nothing was approved and nothing denied.
- **A failed sign-out is visible.** `logout()` now answers whether the server
  confirmed it, and the shell says so when it did not, instead of writing to a
  console nobody has open.
- **The cockpit terminal has a keyboard exit** (WCAG 2.1.2), on US-style layouts.
  `Ctrl+]` leaves it, announced in the title bar and on the grid's
  aria-description — `Ctrl+]` rather than the filed chord, because Windows eats
  that one at OS level. **Known gap:** on DE/FR/ES layouts `]` needs AltGr, the
  browser then sees `altKey`, and the binding does not fire, so the trap stands
  there; a second chord for those layouts is on the 0.8 punt list
  ([ROADMAP.md](ROADMAP.md)). A hint that disagreed with the binding would be
  worse than no hint, so nothing in the UI promises an exit it cannot deliver.
- **The audit trail stops evicting itself.** Two halves, because the flood had two
  causes. The sidecar's token renewer retried ANY failure every 60s forever; it now
  stops at once on the control plane's own permanent refusals, backs off
  exponentially otherwise, and gives up with ONE log line — and the healthy-run case
  underneath it, a run holding a dead identity after a control-plane outage, is now
  ONE `run.identity.expired` audit row keyed to the run. Structurally, identical
  consecutive `auth.failed` rows (same boundary, reason, path and peer) fold into
  their first row plus one NEW summary row carrying `count`/`first_seen`/`last_seen`
  — a new append, never an update, because `audit_events` is append-only and
  hash-chained. Bounded by a maximum GAP between identical rows
  (`WARDYN_AUDIT_COALESCE_WINDOW`, default `5m`, `0` = off) and a 1000-row streak
  cap. Every summary row pays the same rate limiter a first row pays, so the
  instrument added to bound a flood cannot be turned into one by a caller
  alternating two paths on one connection (a refused summary is dropped and
  counted). The key holds the PEER IP rather than `host:port` — with the port in
  it a client that opens a connection per request folded nothing, which is exactly
  the ingress-fronted estate this bounds — and the open streak is flushed at
  shutdown rather than lost with its count. On the deployment that prompted this, 999 of the last 1000 rows were one
  sidecar's `auth.failed`, and every real security event had aged out of the
  console's window mid-investigation.
- **A private-IP denial is answered once, and says when it can change.** The ten
  retries in the field report are the agent CLI's, and each one re-resolved,
  re-vetted and emitted another `egress.deny`. The private-address guard is the one
  refusal that cannot change its mind mid-run, so a bounded per-run memo answers an
  identical repeat with a byte-identical 403 and leaves ONE row carrying the repeat
  count when it evicts or the run ends. The memo is asked BEFORE the approval
  flow, so a retry against an address no human could ever open stops spending a
  human decision on it. `X-Wardyn-Egress-Retry: never` rides that arm alone: a
  resolve failure may clear and an undecided approval is waiting for a human, and
  telling either "never" would turn a transient fault into a dead run.
- **A denial no site config can lift stops prescribing one.** Every
  post-resolution guard refusal told the same story — declare it under
  `internal_hosts`, then start a new run — but only the private/reserved-range
  class is liftable. Loopback, link-local (including `169.254.169.254`),
  multicast, NAT64- and IPv4-compatible-embedded and the other reserved ranges get
  their OWN `403` body: the class that refused, the statement that nothing lifts
  it, and NO site-config remedy and NO lifetime clause, because there is nothing
  to change and a new run would change nothing. Both variants keep rule source
  `builtin:private-ip` and `Retry: never`, and both bodies are now golden
  literals, as are `builtin:resolve-failed`'s and `policy:require-tls`'s.
- **The `cidrs` docs trap is inverted.** Scoping an `internal_hosts` entry with
  `cidrs` is intuitive and wrong from the operator's own machine: a laptop resolves
  private-endpoint names over the corporate resolver into CGNAT space while inside
  the VPC an interface endpoint has ENI addresses in the VPC's RFC1918 range, so a
  `cidrs` list drawn from what the operator can see excludes what the sandbox
  resolves — and the denial looks identical to having no entry at all. Leaving
  `cidrs` empty (still host-scoped) is now the documented default, in
  `docs/OPERATIONS.md`, in the field's own doc comment, and in the 403's remedy
  sentence.
- **A declared model-access lane is never quietly swapped for another.** Transport
  selection knew nothing about what an admin declared, so a deployment whose agent
  row says "Anthropic API key" dispatched Bedrock the moment a region, a model and
  a bearer secret existed — and the customer read a Bedrock bill for runs they had
  configured as api-key. The lane actually SELECTED is now compared against the row
  ahead of the MITM CA and every grant author, so a refused run mints nothing, and
  the same predicate answers `422` at create and at Review before a run row exists.
  With no roster, nothing is refused. The proxy's brokered-LLM refusal stops naming
  a host the reader can do nothing with and renders the reason the control plane
  compiled at dispatch, always saying it is below policy — so an agent stops
  retrying a door that does not exist.
- **A renewable AWS SSO session is renewed, not thrown away.** An expired-but-
  renewable captured credential was discarded and the run fell through to the next
  credential mode — which is what silently crossed into a different auth mechanism.
  Renewal now happens control-plane-side at dispatch, because `CreateToken` rotates
  the refresh token and only the control plane can persist what comes back; the
  sandbox cache therefore stops carrying `refreshToken`/`clientId`/`clientSecret`
  whenever a refresh token exists, since two parties refreshing one pair is how
  hourly re-auth became the resting state. Failure is classified on the response
  body's `error` field, never the status code, so one AWS throttle cannot sign a
  fleet out. Create and preflight never spend a one-use token; their verdict for
  expired-but-renewable is READY, because dispatch renews it. New audit action
  `harness.credential.refresh`.
- **Create and dispatch fold the managed subscription lane with the same terms.**
  The two ends disagreed about which lane a run would dispatch on, so a managed
  token beside a configured Bedrock bearer was refused at the door, and every
  multi-user deployment under a subscription row went `201` and then FAILED with no
  sandbox. Both callers use one spelling now, and a refusal that finds a DIFFERENT
  lane took the run names that lane instead of calling a working credential
  unconfigured.
- **Every way launch narrowed a request is recorded on the run.** `run.create`'s
  success datum gains `clamp_warnings` — the clamp already produced exactly those
  warnings and they reached nobody, so "the ceiling tightened this" was
  indistinguishable from "the product ignored my input".
- **The re-home guard's read-to-write gap is closed, in the store and under a row
  lock.** Deciding whether a drive may be re-homed was a read followed by an
  unconditional write, and its own doc named the residual: a grant created between
  the two was re-homed silently. The decision now travels into the writing
  statement — and because the predicate alone would only have narrowed the window,
  the guarded path takes a row lock in the same transaction first. Two Postgres
  tests cover it, one of them a real two-session interleaving that asserts the
  write WAITS on an open grant and then refuses.
- **A write that revoked tokens says so.** `POST /access/mappings` has returned
  `tokens_revoked` since a demotion started revoking outstanding tokens, and the
  console rendered neither that nor `stale_token_snapshots`; the People step's
  add-mapping form now shows the receipt when a write actually revoked some.
- **A per-run approval cap.** The sandbox picks the hosts and tools it asks about
  and the dedup guard only collapses repeats of the same scope, so a run walking a
  thousand unknown hosts put a thousand rows in front of a human.
  `maxApprovalsPerRun = 4096`, counted in the database, answered `429`, failing
  CLOSED on a count error and emitting no audit action — a run that hits the cap
  must not flood the trail with the refusal instead of the rows.
- **An `egress_domain` approval is HOST-WIDE, and the surfaces say so.** One
  spelling of "which host this approval is about" now backs both the cache key and
  the raised `requested_scope`, and it strips a port if one is ever present, so a
  caller passing `example.com:8443` gets the same grant rather than silently opening
  a second question for a host the operator already decided.
- **The Docker "uncapped" warning stops firing on Kubernetes.** It was gated on
  `enforcement !== "filesystem"`, so `eviction` read as unenforced and every
  Kubernetes operator was told all three of its clauses on all three disk fields —
  and on Kubernetes all three are false: the pod binds the size, a filled or
  clamped number is not uncapped, and a policy-written one does not fail at
  create. One shared predicate answers it for both surfaces now: `none` alone.
- **Review previews the number the run will actually get.** The preview clamped
  by the profile's `max_ephemeral_disk_mib` alone while dispatch also clamped by
  the org's `max_disk_mib` — 100000 MiB previewed against 4096 MiB run, under two
  comments each denying it could happen. Both ends call one expression now, from
  the inline arm and the stored/default arm alike, and the parity one of those
  comments argued away is the pin.
- **The org drive switch answers launch, preview and `/me` alike.**
  `storage.user_drive.disabled` was read in three places and asked at neither
  surface a member's console draws from: `GET /me` shipped a fully populated
  allocation beside an empty unavailable-reason list and `POST /drives/preview`
  answered `200`, for a mount the create path refuses `422`. The deployment half
  is now raised from the one read all three already pass through, so the preview
  answers the same `422` with the same bytes and `/me` reports no allocation with
  its existing `unavailable` token — still `422`-before-`403`, the deployment
  switch ahead of the profile door. An unknown enforcement word also renders NO
  gloss rather than the `none` one, and a governance write refuses a NEGATIVE
  limit by name, the boundary the sibling org block already had.
- **A member's model-access chip stops carrying the operator's deadline.** Under a
  `shared` row the graded credential is the OPERATOR's, so a member was handed
  `expiring` plus a "Sign in again before" deadline — one the body
  redaction had just stripped, and an instruction the login route then refused
  them. A non-`per_user` answer collapses to `live` while the shared credential
  works and to a `shared_expired` admin sentence with no timestamp when it does
  not; a deadline or a sign-in action is now reserved for the principal who owns
  the credential. Around it: the credential scope is stamped at LAUNCH on
  `harness.login.started` and read back at upload, so a roster flip inside a login
  run's idle window can no longer re-point a member's capture at the operator-wide
  credential; the once-only upload guard takes the refresher's own per-namespace
  lock, so two concurrent PUTs from one login run land once (`204` and `409`, not
  two `204`s); an operator's `DELETE /setup/harness-credential/aws` deletes
  through the scope the capture was WRITTEN with, so Disconnect is no longer a
  no-op on a `per_user` estate; and the stored region is validated against the AWS
  region grammar before `oidc.<region>.amazonaws.com` is composed, so a region
  carrying `/` or `@` fails visibly instead of POSTing a client secret and a
  refresh token to a host of its choosing.
- **The providers console tells the truth about what it will save.** A credential
  lane no longer defaults its host to `github.com` when a row carries no
  parseable address — an Azure DevOps row with no address was writing the admin's
  PAT to the secret the GitHub clone helper reads — and with no host every lane
  renders disabled, named, and unsaveable. A row with zero base URLs is marked
  invalid and withholds Save, because the server refuses it outright. The base-URLs
  textarea keeps its newlines (it split on every keystroke, so two addresses
  concatenated and `Enter` could never survive a render); removing the LAST
  provider row is saveable, while a form with nothing to save still withholds the
  button; the client's base-URL mirror admits exactly what the server admits; a
  fresh Azure DevOps row starts at `https://dev.azure.com/` and says its
  organisation segment is missing rather than guaranteeing a `400`; every row
  control — textarea, lane checkboxes, the SSO start-URL input — is reachable by
  its name; the Agents tab's roster Retry re-fires the call the roster actually
  comes from; an agent outside the roster, a `per_user` stored against a mechanism
  that cannot carry it, and a `model_access` state outside the five are each
  rendered honestly rather than as a confident wrong answer; the funnel's Providers
  step shows non-operators the tier hint instead of an empty panel; and the
  Settings card counts agents only once an agent policy exists.
- **The Kubernetes substrate now reclaims an evicted run's orphans — it never
  did.** The control plane's orphan sweep reaches its substrate by type
  assertion, and only the Docker driver implemented it, so on Kubernetes the
  sweep was a silent no-op: nothing ever revisited a run's objects once no
  `sandbox_ref` pointed at them. This release is the one that makes that
  routine rather than crash-only — `disk_mib` is now the agent container's
  `ephemeral-storage` limit, so the kubelet EVICTS the pod on a path no Wardyn
  code is on, leaving the run's proxy pod running with its resolved upstream
  credentials and the per-run Secret (run token, MITM CA key, injected git
  tokens) sitting in the namespace. The k8s driver implements the same sweep and
  returns the same shape: it lists the agent and proxy pods by label — both, not
  the agent alone, because the kubelet's terminated-pod GC may reap an evicted
  pod while the proxy lives on — asks the control plane which run ids are
  orphaned, and tears down every object of those by label. A user drive's claim
  is never touched (it carries no run label, and the sweep deletes only pods,
  NetworkPolicies and Secrets), and the chart needed no new RBAC verb.
- **The Compose envelope forwards the member-mount and four-eyes keys.** The
  desktop tier's member-mode envelope, the file an MDM copies to every laptop,
  sets `WARDYN_MEMBER_MODE`, the four `WARDYN_MEMBER_*` root lists and
  `WARDYN_EGRESS_SECOND_HUMAN` — and the Compose stack forwarded none of them, so
  a variable could only be interpolated, never seen by the daemon. Every m′
  device therefore booted as the ordinary admin-token tier with member host
  mounts REFUSED (unset roots fail closed), and the four-eyes egress gate read
  as ON in the envelope while being OFF in the daemon: a governance control worse
  than absent, because the envelope is what an auditor reads. All six keys are
  forwarded with their documented defaults, and `scripts/test-desktop-profile.sh`
  now fails when any variable an envelope sets has no forward — the missing
  mechanism, not just the two missing keys.
- **Tooling and mechanical pulls.** `check-file-size.sh` walks tracked files;
  `-coverpkg` closes the same-package blind spot; the sidecar's operator knobs now
  travel on BOTH container substrates from one list (a pod inherits nothing from
  wardynd, so three switches were not "off" on Kubernetes but unreachable); every
  gated route the completeness check listed has a row in `docs/OPERATIONS.md`'s
  tier table; `wardyn site-config apply` prints which post-0.6.6 fields it left as
  the server already had them; and `values.yaml` documents that
  `persistence.enabled=false` also leaves the audit spool on the ephemeral
  `/tmp` emptyDir. `make test-race` also races the `-tags k8s` tree, which no
  pass compiled before — the whole L1 substrate, its drive provisioning, its
  teardown poll and its new orphan sweep, had zero race coverage.

### Security

- **An operator can say a brokered credential is TLS-only.** The `api_key`
  injection rule gains `require_tls`. The proxy's transport rules already withheld
  a credential from cleartext to `:443` and from a host it only ever speaks TLS to,
  but a plaintext connector on `:80` is indistinguishable from an https-only vendor
  it has no table for — so that one was injected, in the clear. The plain forward
  lane now refuses such a request outright, ahead of content inspection (a refused
  request's body is never read), with its own `403` naming the host and the rule
  and a `policy:require-tls` deny row that REPLACES the allow, because nothing was
  forwarded. The scope decodes STRICTLY: a misspelled `require_tls` used to read as
  `false`, a security control failing open on a typo. Unset, every byte is as
  before.
- **PAT pushes can be confined to the run's branch namespace.** The never-resident
  `git_pat` lane has terminated its own cleartext smart-HTTP since 0.7, so the
  receive-pack parser could always have bound it; leaving it unconfined was a
  decision, and four texts said so. It is now wired through the one `confinePush`
  step both brokers call — same words, same `brokered:git:branch-ns*` vocabulary,
  one rule with two brokers — behind `WARDYN_GIT_PAT_BROKER_ENFORCE_BRANCH_NS`,
  **default off**, the opposite default from the App lane: a PAT carries whatever
  scope the operator issued, over forges whose push-ref conventions are not
  GitHub's. Off, the lane is byte-for-byte 0.7.1; the four texts and their two
  guards are re-derived to the new truth rather than deleted.
- **A clone path cannot be walked out of the provider row that admitted it.**
  Admission compared an org-scoped base URL against a path the server reads
  DECODED while the sandbox's git squashes dot segments client-side, so
  `https://github.com/acme/../evil/repo` and the percent-encoded `acme%2Fevil`
  spelling were both admitted by an `https://github.com/acme` row — and the `pat`
  lane then minted the acme organisation's credential for `evil/repo`. The
  traversable SHAPES are refused at the one chokepoint every door resolves
  through — any percent-escape, any backslash, any empty, `.` or `..` segment, in
  the https, `ssh://` and scp forms alike — and the same rule is a `400` at both
  write doors, because a locator no provider mode can ever clone is not storable.
- **The SSH scoping ceiling is said out loud.** An SSH clone URL carries no path
  to compare a base URL against (`git@github.com:acme/x.git` is not `/acme/x`), so
  a row scoped to `https://github.com/acme` admits an SSH clone of ANY org on that
  host. The ceiling STAYS — there is no path to bind — but it stops being silent:
  the providers row says it under the lanes field when it carries a path and
  permits `ssh`, `docs/OPERATIONS.md` states it with its remedy (drop `ssh` from
  the row's lanes and the addresses bind again), and run create puts it on the
  `201` with a new `run.provider.ssh_host_level` audit row. It is the only
  admission outcome WIDER than the policy reads, which is why it is the one
  recorded.
- `threatmodel/THREAT-MODEL.md` gains four residuals for what this release does
  and does not bound: provider admission is URL-prefix matching over a clone URL
  and not a repository ACL (and is host-level only for an SSH clone URL); a
  provider row's `lanes` bound which credential lane a run may use and never what
  that credential itself can reach; the `auth.failed` coalescing key contains
  `SourceIP`, which does not separate principals behind a Kubernetes ingress; and
  the private-IP memo is per run and bounded, so a name that becomes public
  mid-run stays refused until the run ends. The drive object-name residual is
  corrected: 0.7.1's migration `0061` closed the slug-uniqueness half, not the
  separator collision, whose real fix re-homes existing storage and is 0.8.

### Known gaps and deferrals

What this release did NOT prove, stated here rather than left for the next
person to rediscover. None of it is a regression; all of it is verification
debt, owner-hardware debt, or a decision deliberately not taken.

- **The 0.7 R2 review backlog is carried, not closed.** A whole second review
  round was banked against a pre-0.7.0 tip and never ingested: 19 agent result
  files, 75 candidate rows, none of which the 0.7.2 plan claimed. A quick pass
  re-checked every row at v0.7.1 and again at this tip and triaged them —
  **53 carried forward unchanged**, **10 already resolved somewhere in 0.7.x**
  (all at or before v0.7.1 — 0.7.2 fixed none of them), **5 had WORSENED and
  are fixed in this release** (the Kubernetes orphan sweep, `-race -tags k8s`,
  and the Compose envelope forwards, all named above), and **7 needed a deeper
  probe than a re-read**. The triage sheet is operator-local and untracked
  (`local/v072/R2-quickpass.md`), so this list is the shipped record of it. The
  53 carried rows are the honest number for "known, unreviewed at 0.7".
- **Two browser rows could not be delivered by the harness.** The Playwright
  agent-roster spec cannot drive "a member signs in to AWS SSO and launches" or
  "an expired shared credential is refused with the named sentence": capability
  subjects are derived from the OIDC human context alone, and the e2e harness
  authenticates with a bare admin token that carries none. Both behaviours are
  Go-pinned (`runs_dispatch_llm_mechanism_test.go`, `awssso_refresh_test.go`)
  and are walked live against a real AWS Identity Center account when that
  hardware is on hand. A harness limit, recorded as one.
- **Four live walks need hardware this release was not cut on.** A member on
  `kind` behind Entra ID (allowed-provider `201`, capability deny `403`,
  unresolved `/me`); real kubelet eviction on `kind` with the conformance
  `EphemeralDiskLimit` case; the Workstream C roster against a real AWS
  Identity Center tenant; and the desktop a′/m′ envelope walk. Everything they
  would exercise is covered by Go and conformance tests that compile and pass
  where they can run; what is missing is the live confirmation, not the code.
- **The blind-CSRF Origin guard still applies in LocalMode only.** The Origin
  check that refuses a cross-site mutating request is inside the local-mode arm
  of `internal/api/http.go`'s auth middleware (the `isLoopbackOrigin`
  predicate); an SSO or token deployment relies on the session cookie's
  `SameSite=Lax` alone, and 0.7.2 adds mutating run-plane routes behind that
  same middleware. Widening the check to every cookie-authenticated mutating
  request in every mode is a small, auth-adjacent change; it is deliberately
  NOT in this release, and it is named here rather than closed quietly.
- **A delivered AWS SSO credential blob is not floored at CC3.** Stated in full
  under the per-user bullet above and repeated here because it is an open
  posture decision, not a closed one: `composer.RequiredConfinementFloor` — one
  caller, in `internal/api/runs_create.go`'s create-time validation — raises a
  GRANT-delivered credential to CC3 and never sees the SSO cache blob, under
  either `shared` or `per_user`. 0.7.2 narrows the blast radius to one person
  under `per_user`; it does not floor the class. That is a 0.7.3/0.8 call.
- **Two Compose residuals remain beside the envelope fix above.** The
  `WARDYN_WORKSPACES_ROOT` bind is mounted `:ro`
  ([corrected by #135](https://github.com/cjohnstoniv/wardyn/issues/135): that
  `:ro` bind is wardynd's own container-local view and has no bearing on a
  member mount's writability, which `internal/runner/member_mount.go` decides
  entirely by root/deny-list match — this bullet's writable-mount claim was
  wrong); and the m′ envelope's `/srv/src` root has no bind at all. Both are
  written down in `docker-compose.yaml` at the forwards that this release
  added.
- **Every user-facing string this release adds ships as a frozen DRAFT.** Said
  once at the top of this section and repeated here because it is the largest
  single caveat: the `400`/`412`/`422` bodies and the new console copy are
  constants awaiting the maintainer's canon sitting, tracked on an
  operator-local sheet (`local/v072/M2-canon-sheet.md`). Four of them already
  diverge from the design prompt's staging, including the terminal's `Ctrl+]`
  exit chord. Tests assert through the constants, so adopting the canon is a
  one-file diff that moves no behaviour.

## [0.7.1] — 2026-09-11

### Fixed

- **The console header shows who you are, not your IdP's object id.** For an SSO
  user the account chip and its menu rendered `me.principal` — the raw OIDC `sub`
  (an Entra object id; `gsv-member-0001` on the kind demo) — even though
  `GET /api/v1/me` already returned the email beside it. The header now reads the
  IdP's `name` claim, then the email, and falls back to the principal only for an
  admin token or local mode, where there is neither; the opaque subject stays in
  the account menu as a secondary mono line, since that is the string
  `docs/OPERATIONS.md` tells an admin to paste. `GET /api/v1/me` gains `name`
  (`""` outside SSO) and the session cookie carries the claim; a cookie minted by
  0.7.0 has no `name` and is still a valid session — nobody is signed out. The
  signed-in principal every ownership check compares against is still the `sub`.
- **A stock `helm install` boots again.** With `persistence.enabled=false` (the
  chart's default) the chart spelled "recording off" as `WARDYN_RECORDING_STORE=fs`
  plus an empty `WARDYN_RECORDING_DIR`; since 0.7.0 wardynd keeps a compiled default
  when an env value is empty (F011/F067), so the empty dir became
  `./data/recordings`, `mkdir` hit the read-only root filesystem, and the pod
  crash-looped — `ci.yml`'s `helm-install-test` had been red since the 0.7.0
  release commit. Recording off is now a store of its own, `WARDYN_RECORDING_STORE=off`;
  the chart sets it when persistence is off and `fs` + the volume path when on.

## [0.7.0] — 2026-09-09

Two of the three headline blockers an enterprise adopter reported against 0.6.6 are
fixed here — both were already built when the report arrived. An operator can trust a
TLS-inspecting proxy's root CA (`WARDYN_TRUSTED_CA_FILE`): the daemon, the proxy
sidecar and every sandbox pick it up. And an operator can declare internal hostnames —
an in-cluster service, a corporate registry — allowed to resolve to private/CGNAT
addresses (`SiteConfig.internal_hosts`), so an internal service is reachable by name
instead of only by a literal IP. Everyone signs in once after upgrading: cookies
issued by an older daemon are re-derived rather than accepted. The rest of 0.7 is
assignable governance profiles, a security-admin tier, user drives, and never-resident
git PATs.

### Highlights

- **Governance profiles** — a named policy ceiling an admin can ASSIGN to a person, to an SSO group, or to everyone, so a contractor group and a platform team can hold genuinely different limits on one install. A profile REPLACES the site-wide default rather than composing with it. - **A security-admin role**, and a console that can be delegated to it — the second admin tier governs the verdict (profiles, permissions, egress decisions, token inventory, audit verification) and deliberately does NOT reach into a run. - A governance profile can cap **how many runs one person has going at once**, and self-service secrets gain the per-owner cap the sibling surfaces already had. - An admin can fence **which agents and which model providers** a member may name on their own run, as two more permission kinds on the existing Permissions page. - **User drives** — an admin registers persistent storage and allocates it to people, groups or everyone; a member mounts theirs per run at `/home/agent/drive`, **read-only unless allowed**, and the server resolves which drive belongs to the signed-in caller. Registering a drive on a host path is fenced by `WARDYN_USER_DRIVE_HOST_ROOTS`, unset and therefore closed by default. - **Git PATs for non-GitHub forges are never resident** — `agent-run` rewrites a granted host to a plain-HTTP broker path, the proxy mints server-side and injects Basic auth itself, and the grant ids are withheld from the sandbox env. `WARDYN_GIT_PAT_BROKER=off` restores the old lane; there is deliberately **no automatic fallback**.   - The lane is on by default and, until this release candidate, **was not actually running**: "the switch resolved into a setting nothing read, and the lane was carried by an internal per-launch flag no launch path ever set". - An operator can trust a corporate TLS-inspecting proxy's root CA (`WARDYN_TRUSTED_CA_FILE`), delivered on compose, the desktop profile and the Helm chart. - An operator can declare internal hostnames allowed to resolve to private/CGNAT addresses (`SiteConfig.internal_hosts`), and can tell a corporate proxy **which destinations to skip** (`upstream_proxy_no_proxy`). - Bedrock can be reached through a **VPC (PrivateLink) endpoint** (`WARDYN_BEDROCK_BASE_URL`), with full model ARNs documented as accepted identifiers. - An operator can point the API-key model-access lane at an **internal gateway** (`WARDYN_ANTHROPIC_BASE_URL` / `WARDYN_OPENAI_BASE_URL`). - **A member can bring their own model API key** and set and remove their own secrets — it works in their own runs with no admin setup and is never reachable from anyone else's run. - The **People step becomes an acting surface**: an admin adds, edits and deletes `WARDYN_OIDC_ROLE_MAP` role mappings live from the console, guarded by a posture-flip acknowledgement and a lockout refusal. - Console `tool_rules`: a per-tool allow / hold / deny editor in the policy panel, a "What this run can do" line on the New run rail, and audit rows that read **Decided by rule** with the verbatim `rule_source`. - **A blocked egress request now says WHICH rule blocked it** — `X-Wardyn-Egress-Reason` carries the decision log's own rule source (`policy:default-deny`, `approval:denied`, `builtin:private-ip`, …). - Wardyn reports whether the Kubernetes NetworkPolicy that isolates sandboxes is **actually enforced** (`/healthz`'s `network_policy` field, plus a boot-time audit event on an unenforced-but-allowed cluster). - **Helm `image.digest`** — the blessed Kubernetes path no longer has to float on a mutable tag. - External clients can drive a sandbox over the SSH gateway: `wardyn ssh-key ensure|list`, `wardyn run wait-ready <id> --json`, `wardyn ssh <id> --json`, plus the per-run `git_push_any_branch` opt-out. - **A browser desktop (noVNC) is a shipped image variant** (`deploy/images/novnc/`, `make agent-image-novnc`) — local build only, and it changed no server code. - A fresh install **remembers being set up server-side** (`POST /setup/onboarding-complete`), so a different browser — or a different admin — lands past the funnel too. - `threatmodel/AGENT-THREAT-MODEL.md` — a portable threat model for agent systems generally, carrying twice as many non-mitigated verdicts as mitigated ones. 
### Hardening pass

0.7 was reviewed before release rather than after. Seven independent verification
runs (R1–R7) were opened over the release candidate, one per subject area, staffed
with blind reviewer agents working from a written brief. Six of them produced ledgers;
the seventh (R2, the run plane) was abandoned before its first round finished, and that
gap is stated below rather than averaged away. Every finding the six recorded is in a
ledger, and every fix claim names the command that proves it. This section is the
arithmetic out of those ledgers, the contract changes an operator will notice, and what
is still open. **The bullets in the sections below this one are the individual fixes
those runs produced.**

Read the table with two qualifications, both load-bearing:

- **"Fixed" mostly means fix-claimed, not reviewer-verified.** Six of the seven runs
  ended `INCONCLUSIVE (budget)`: the fix wave landed, the gates were re-run green on
  the release candidate (`make ci` 24/24 plus the PostgreSQL lane, `gate exit=0 tree
  776c1065` and `gate exit=0 tree 0e8a751d`), but a second blind reviewer round to
  confirm the fixes was not funded. Only R1 carries reviewer-verified fixes (101 of
  them), and even there the 62 final-wave fixes are "fixed-unverified, gate-green".
- **"Open" is almost entirely Low/Info residue** deliberately left for 0.7.1, listed
  in `local/review-0.7/FOLLOW-UPS-0.7.1.md`. Only one open finding is above Low (a
  single R7 Medium). The ten `deferred` findings are the ones to read: two of them are
  **High** (R3 F055, R4 F107) and both are named under Residuals below.

| Run | Subject | Critical | High | Medium | Low | Info | Total | Fixed | Disputed | Deferred | Open | Verdict | Source |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| **R1** | authz + governance | 11 | 41 | 117 | 124 | 58 | **351** | 163 (101 verified-fixed + 62 fix-claimed) | 3 rejected | 3 | 182 | INCONCLUSIVE (budget) — 65 unverified | `local/review-0.7/runs/R1-REPORT.md` §0 — 351 = 284 on the re-init ledger + 67 carried from the deep ledger |
| **R2** | run plane | 0 | 0 | 0 | 0 | 0 | **0** | 0 | 0 | 0 | 0 | not reviewed — the round was abandoned before ingest and the ledger is empty (its report's verdict string reads APPROVED over zero findings) | `~/.claude/verify-runs/wardyn-0.7-r2-20260904-161449/report.md` |
| **R3** | egress + credentials | 2 | 32 | 62 | 45 | 23 | **164** | 95 fix-claimed | 0 | 1 | 68 | INCONCLUSIVE (budget) — 56 unverified | `~/.claude/verify-runs/wardyn-0.7-r3-20260904-161453/report.md` |
| **R4** | UI console | 0 | 17 | 46 | 67 | 16 | **146** | 57 fix-claimed | 0 | 6 | 83 | INCONCLUSIVE (budget) — 51 unverified | `~/.claude/verify-runs/wardyn-0.7-r4-20260904-160926/report.md` |
| **R5** | ops / install / CLI | 0 | 12 | 125 | 84 | 17 | **238** | 137 fix-claimed | 0 | 0 | 101 | INCONCLUSIVE (budget) — 96 unverified | `~/.claude/verify-runs/wardyn-0.7-r5-20260903-202842/report.md` |
| **R6** | docs + threat model | 0 | 8 | 62 | 57 | 7 | **134** | 70 fix-claimed | 0 | 0 | 64 | INCONCLUSIVE (budget) — 45 unverified | `~/.claude/verify-runs/wardyn-0.7-r6-20260903-202659/report.md` |
| **R7** | user drives | 1 | 7 | 41 | 44 | 14 | **107** | 44 fix-claimed | 4 disputed | 0 | 59 | INCONCLUSIVE (budget) — 27 unverified | `~/.claude/verify-runs/wardyn-0.7-r7-20260903-160043/report.md` |
| | **Total** (arithmetic over the rows above; not read from any single file) | **14** | **117** | **453** | **421** | **135** | **1140** | **566** | **7** | **10** | **557** | | |

R2 is the one subject area 0.7 did not verify: its abandoned round banked 18 lens files with 75 untriaged candidate rows (4 High, 36 Medium, 26 Low, 9 Info), which are 0.7.1's first triage (`local/review-0.7/FOLLOW-UPS-0.7.1.md`). Nothing in this release claims to have reviewed the run plane.

Full verdict strings, verbatim from each report's `## Final Verdict`:

- R1 — `VERIFY wardyn-0.7-r1-std-20260905-214053 INCONCLUSIVE (budget) — 65 unverified (mode: repo) (tools changed r1: ledger.py, references/convergence.md) 386dc19`
  · source: `local/review-0.7/runs/R1-REPORT.md:5`, also `~/.claude/verify-runs/wardyn-0.7-r1-std-20260905-214053/report.md:3428`
- R2 — `VERIFY wardyn-0.7-r2-20260904-161449 APPROVED (mode: repo) 3a46853`
  · source: `~/.claude/verify-runs/wardyn-0.7-r2-20260904-161449/report.md:46`
- R3 — `VERIFY wardyn-0.7-r3-20260904-161453 INCONCLUSIVE (budget) — 56 unverified (mode: repo) (tools changed r1: ledger.py) 3a46853`
  · source: `~/.claude/verify-runs/wardyn-0.7-r3-20260904-161453/report.md:2239`
- R4 — `VERIFY wardyn-0.7-r4-20260904-160926 INCONCLUSIVE (budget) — 51 unverified (mode: repo) (tools changed r1: ledger.py, references/convergence.md) 3a46853`
  · source: `~/.claude/verify-runs/wardyn-0.7-r4-20260904-160926/report.md:1994`
- R5 — `VERIFY wardyn-0.7-r5-20260903-202842 INCONCLUSIVE (budget) — 96 unverified (mode: repo) (tools changed r1: ledger.py, review_round.js) 020b09d`
  · source: `~/.claude/verify-runs/wardyn-0.7-r5-20260903-202842/report.md:3232`
- R6 — `VERIFY wardyn-0.7-r6-20260903-202659 INCONCLUSIVE (budget) — 45 unverified (mode: repo) (tools changed r1: ledger.py, review_round.js) 020b09d`
  · source: `~/.claude/verify-runs/wardyn-0.7-r6-20260903-202659/report.md:1964`
- R7 — `VERIFY wardyn-0.7-r7-20260903-160043 INCONCLUSIVE (budget) — 27 unverified 386dc19`
  · source: `~/.claude/verify-runs/wardyn-0.7-r7-20260903-160043/report.md:1521`




#### Contract and compatibility changes

Every row below is one `[Unreleased]` bullet. The **compat note** column is the
bullet's own words where it carries one, and `—` where the bullet states none; nothing
in that column is inferred. `CHANGELOG.md` line numbers are the pre-rename numbering.

**Upgrade-affecting first — read these five before upgrading:**

| Line | What | Compat note (the bullet's own words) |
|---|---|---|
| 645 | **Everyone signs in once after upgrading.** The session cookie's format version is stamped; cookies issued by an older daemon are re-derived rather than accepted. API tokens carry the same marker from the moment they are minted. | "This is deliberate and it is one field's fault: the cookie now records whether a member's group list was truncated" |
| 639 | **Helm `k8s.runsNamespace` must be set** — `k8s.enabled=true` with an empty value is now refused at render. | "**Upgrading a k8s install that relied on the default:** set `k8s.runsNamespace` to a dedicated, pre-existing namespace (create it first — the chart never does) … or set the new `k8s.allowRunsInReleaseNamespace: true` to render as before and accept the shared blast radius. Installs that already set `k8s.runsNamespace` are unaffected." |
| 662 | **`WARDYN_REQUIRE_OPERATOR_SET_EGRESS` default flips off → ON.** | "**Upgrade impact:** a workspace whose egress requirements are `scan_seeded` stops having them auto-added, and the run's warnings name what was skipped; an operator declaring the host is the intended fix. Set `WARDYN_REQUIRE_OPERATOR_SET_EGRESS=false` to restore the old behaviour." |
| 105 | **Migration `0061_user_drives_name_slug_unique`** refuses two drives whose DNS-1123 fold collides. | "**An upgrade fails if the install already holds such a pair** — that pair is the defect, and the remedy is to rename one drive: `SELECT name_slug, array_agg(name) FROM user_drives WHERE backend <> 'host_path' AND name_slug <> '' GROUP BY 1 HAVING count(*) > 1;`" |
| 635 | **Rebuild the agent images before upgrading if you use user drives** (`make agent-images-core`, then re-pin `WARDYN_AGENT_IMAGES` if you pin). | "on a pre-0.7 image an allocated drive appears as a root-owned directory the agent cannot write. BYOI images must do the same. 0.7 also refuses `/home/agent/drive` as an authored mount target: a pre-0.7 policy or workspace row that names it is dropped with a WARN at dispatch until you edit it (OPERATIONS.md Upgrades has the sweep)." |

**Database migrations.** 0.7 carries **twelve**: `0050`–`0061`. `v0.6.6` ends at
`0049`. Four are named in the bullets below; the rest ship unnarrated.

| Migration | Named at | What |
|---|---|---|
| `0050_secret_owned_by` | CHANGELOG.md:658 | "The `secrets` table's primary key widens from `name` to `(owned_by, name)` … existing rows are unaffected and keep working exactly as before." |
| `0051_role_mappings` | not named | console-managed role mappings (the People step's write target) |
| `0052_governance_profiles` | not named | governance profiles and their assignments |
| `0053_role_mappings_security_admin` | not named | the `security_admin` tier in the role map |
| `0054_user_drives` | not named | the user-drive tables |
| `0055_workspace_egress_edited_at` | not named | `workspaces.egress_edited_at` (the boot-heal marker at CHANGELOG.md:638) |
| `0056_audit_chain_serialize` | CHANGELOG.md:91 (by consequence) | audit-chain serialization moved into the trigger |
| `0057_audit_chain_security_definer` | CHANGELOG.md:91 | "the `0057` state `0058` repaired" |
| `0058_audit_chain_schema_qualified` | CHANGELOG.md:91 | the repair for the `0057` state |
| `0059_user_drive_grants_home_override_unique` | not named in `[Unreleased]` | one `home_override` per drive (`docs/OPERATIONS.md:240` names it) |
| `0060_api_tokens_role_check` | not named | API-token role constraint |
| `0061_user_drives_name_slug_unique` | CHANGELOG.md:105 | see the upgrade table above |


**New environment variables.**

| Line | Var | What | Compat note |
|---|---|---|---|
| 114 | `WARDYN_USER_DRIVE_HOST_ROOTS` | allowlist a `host_path` drive may be registered inside | "unset, and therefore closed, by default" |
| 117 | `WARDYN_BEDROCK_BASE_URL` | Bedrock data plane at a VPC/PrivateLink endpoint | "The endpoint is a boot flag rather than a runtime setting because in bearer mode it is the TLS-interception and credential-injection target." |
| 135 | `WARDYN_TRUSTED_CA_FILE` | trust a corporate TLS-inspecting proxy's root CA across daemon, sidecar and every sandbox | — |
| 137 | `WARDYN_OIDC_ALLOW_EMAIL_MAPPINGS` | opts in to an email-shaped console role mapping | "An email-shaped console mapping is refused by default" |
| 141 | `WARDYN_ANTHROPIC_BASE_URL`, `WARDYN_OPENAI_BASE_URL` | point the API-key model lane at an internal gateway | "subscription and Wardyn-managed runs still reach the public provider directly" |
| 222 | `WARDYN_GIT_PAT_BROKER` | `off` restores the pre-0.7 resident-PAT lane | "There is deliberately **no automatic fallback** — falling back would silently return the PAT to the sandbox." |
| 332 | `deploy/desktop/wardyn.env.m-prime.example` (`WARDYN_MEMBER_MODE` envelope) | second complete m′ envelope | "`WARDYN_ADMIN_TOKEN` carried only as a pointer to `secret.env`, never as a value" |

**Existing environment variables whose meaning or default moved.**

| Line | Var | Change | Compat note |
|---|---|---|---|
| 17 | `WARDYN_GIT_APPROVAL_TIMEOUT` | widened: "One budget now covers the mint, the wait and every poll" | — |
| 33 | `WARDYN_PROXY_IMAGE` | sidecar config decode is strict; unknown keys are named, not discarded | "Bump `WARDYN_PROXY_IMAGE`/`k8s.proxyImage` in lockstep." |
| 89 | `WARDYN_PORT` | the installer upgrade path "never discards `WARDYN_PORT` in silence" | — |
| 90 | `WARDYN_PG_DSN` | "`pool_max_conns=1` now boots with a warning, and the recommended floor with the ground-truth rotator enabled is 4 (was 3)" | — |
| 91 | `WARDYN_PG_MIGRATE_DSN` | the boot audit-chain canary "now runs on BOTH pools" in the split-role posture | — |
| 92 | `WARDYN_VERSION` | `install.sh` refuses a value that is not a plain release tag (`/` or `..`) | "Legitimate tags (`v0.7.0`, `v0.7.0-rc1`) are unaffected." |
| 308 | `WARDYN_DOCKER_SOCK` | honored from the envelope; when nothing resolves the launcher "refuses and prints what it tried" | — |
| 316 | `WARDYN_SSH_LISTEN` / `WARDYN_SSH_ADVERTISE` | now set in both the desktop envelope and the one-line installer | "**empty means off — no listener, not even a generated host key**" |
| 389 | `WARDYN_EGRESS_SECOND_HUMAN` | in local mode the switch is now answered with a `503` naming the incompatibility | "scoped to egress decisions so nothing else in local mode changes" |
| 393 | `WARDYN_ALLOW_MEMBER_ENV_SECRET` | `env_secret`'s admin-only posture now binds on inline body, selected row AND deployment default | "Operators are unaffected; a deployment that deliberately serves `env_secret` to members opens `WARDYN_ALLOW_MEMBER_ENV_SECRET` as documented." |
| 496 | `WARDYN_WARDYND_IMAGE` | the desktop launcher now reads both pins from the envelope instead of its exported `:latest` default | — |
| 549 | `WARDYN_AGENT_IMAGES` | `--agent claude-code` re-points to `agent-base` | "an override in `WARDYN_AGENT_IMAGES` is still consulted first and is keyed by agent name, so an operator who pins the image is unaffected" |
| 641 | every `WARDYN_*` string setting | an empty value now reads as "unset, keep the default" (previously `WARDYN_LISTEN=` bound `0.0.0.0:80` and silenced all three listen refusals) | "to blank a value deliberately, pass the flag (`-listen=`)" |
| 641 | `WARDYN_*` / `ANTHROPIC_API_KEY` inheritance | `wardyn ssh`, `wardyn setup`'s `docker info` and the `setup wall/vault --run` installer no longer inherit them | "`docker compose config` inside `support-bundle` still does, deliberately, and its output is redacted" |
| 655 | `WARDYN_ENVBUILD_IMAGE` | default workspace-build image pinned by tag **and** digest instead of `:latest` | "`WARDYN_ENVBUILD_IMAGE` (`-envbuild-image`) still moves the pin" |
| 662 | `WARDYN_REQUIRE_OPERATOR_SET_EGRESS` | **default off → on** | see the upgrade table above |

**Helm values and render refusals.**

| Line | Value | Change | Compat note |
|---|---|---|---|
| 109 | `image.digest` (NEW), `image.tag` | `sha256:<64 hex>` pulls by digest; a non-`sha256:` value is refused at render; a non-empty tag is kept as `repo:tag@sha256:…` | "default `\"\"`, byte-identical render when empty" |
| 33 | `k8s.proxyImage` | must move with wardynd | "Bump `WARDYN_PROXY_IMAGE`/`k8s.proxyImage` in lockstep." |
| 90 | `allowMultiReplica` | now passes the new `-allow-multi-instance` flag for you; `strategy: Recreate` is what lets an upgrade converge | "under RollingUpdate the new pod would refuse while the old one holds the lock" |
| 632 | `env.WARDYN_AGE_KEY` (checklist hint) | the age-key hint steers the key into the chart's Secret-backed options instead of a plaintext `env.WARDYN_AGE_KEY` | — |
| 637 | pod `fsGroup` on drive pods | only a managed `k8s_pvc` claim keeps `fsGroup: 1000` with `OnRootMismatch`; a `k8s_pvc_static` share pod carries none | "**Compat:** an admission policy or check asserting `fsGroup` on drive pods must scope itself to managed drives." |
| 639 | `k8s.runsNamespace` (now REQUIRED), `k8s.allowRunsInReleaseNamespace` (NEW, default false) | render refusal on an empty runs namespace | see the upgrade table above |
| 642 | `env`/`extraEnv` `WARDYN_AGE_KEY` beside `secrets.ageKeySecretRef` / `ageKeyFromSecret` / `ageKey` | now fails at render (previously rendered and the plaintext literal silently won) | "drop one" |
| 642 | `env`/`extraEnv` `WARDYN_ADMIN_TOKEN` beside `auth.adminToken.secretRef.name` / `.value` | now fails at render | "either alone still renders, and the 401 refusal no longer recommends the plaintext door" |
| 642 | `ssh.enabled` / `uiSandbox.enabled` NetworkPolicy | those ports now exclude pods labelled `wardyn.managed` | "HTTP is unchanged and operator-supplied peers other than a bare `podSelector: {}` pass through untouched" |
| 643 | ClusterRole / ClusterRoleBinding names | now carry the release namespace (`wardyn-wardyn-k8s-runtimeclasses`) | "`helm upgrade` replaces them in place. Anything outside the chart that referenced the old names — an RBAC audit, an admission policy — needs the new ones." |

**Routes.**

| Line | Route | Change |
|---|---|---|
| 42 | `/wardyn/git/<host>/` | a withheld `git_pat` is no longer published to the proxy's PAT-broker path — "withheld from BOTH halves of dispatch" |
| 116 | forty gated routes | classified across the two admin tiers in an exhaustive table a test walks from both |
| 118 | `POST /setup/onboarding-complete` | NEW; "idempotent and audited" |
| 137 | `GET /access`, `POST /access/mappings`, `DELETE /access/mappings/{id}`, `POST /access/preview` | NEW; live role-mapping CRUD from the console |
| 356 | workspace detail + observed-traffic reads | now readable by `security_admin` (previously answered "not found") — "Nothing else moved" |
| 371 | `GET /api/v1/approvals?run_id=` | now paged; "The run filter, the state filter and the page window now all run in one indexed query" |
| 385 | `POST /api/v1/admin/sandboxes/sweep` (added at 291) | stays admin-only "on the reason that actually holds"; "No behaviour changed" |
| 387 | site config, source library (list + detail), base-image catalog | **all four move member → admin tier**; "nothing member-facing consumed them" |
| 390 | recording-session launch | **admin-only again** (had sat on the security-admin tier); "Promoting recorded egress into the allowlist … stays with security admins" |
| 398 | `/metrics` | no longer disappears during an audit-store outage |
| 686 | `GET /auth/logout` | **REMOVED**; "Signing out is `POST /api/v1/auth/logout` … `GET /auth/login` and `GET /auth/callback` are unchanged." |

**Wire fields, headers, audit actions and metrics.**

| Line | Identifier | Change | Compat note |
|---|---|---|---|
| 15 | broker request-header strip list | wider and shared: "`Cookie`, `Private-Token`, `X-Goog-Api-Key`, `X-Amz-Security-Token`, `X-Functions-Key`, `X-Access-Token`, `Anthropic-Api-Key`" | — |
| 20 | `findings_capped`, `findings_past_cap`, `findings_total` | NEW on the decision wire; `finding_count` in the audit row now **is** `findings_total`, counted as produced rather than inferred | — |
| 28 | plain-lane injection header strip (`Authorization` / `X-Api-Key`) | one definition shared by both injecting paths | — |
| 46 | `Skipped{attachment_decode_error}` | NEW skip reason; honors `on_scanner_error=block` | — |
| 53 | `auth.failed` row + `wardyn_auth_failed_suppressed_total` | the sandbox/host-sensor boundary refusals now emit the public lane's rate-bound row with an actor naming which boundary refused | — |
| 54 | `X-Wardyn-Principal` (local mode) | narrowed: no longer steers the run identity's `sub` / secret namespace | "The header keeps its documented attribution job; the namespace now comes from the principal wardynd injected." |
| 61 | AWS SSO upload `region` / `start_url` | refused when they disagree with the operator's configured SSO region and portal URL; a second capture from the same run is refused | — |
| 64 | `wardyn_egress_denies_total` | narrowed: `builtin:dial-failed` and `egress.decisions.dropped:<n>` no longer move it | "they still record their `egress.deny` audit row, neither moves the counter" |
| 66 | mint 409 `code` values (`types.MintConflict*`) | one home shared by the API handler and `wardyn-git-helper`; the `denied` arm is now covered | — |
| 67 | `credential.revoke` rows on the kill cascade | now enumerate the run's successful `credential.mint` rows, each with a per-KIND note | "a `git_pat`/`ssh_key` says the operator must rotate the secret at the forge rather than claiming GitHub's TTL-expiry semantics" |
| 114 | run-request user-drive flag | "a run request carries only a flag: the server resolves which drive belongs to the signed-in caller and derives their own directory from their own identity, so nobody can name someone else's" | — |
| 120 | `X-Wardyn-Egress-Reason: approval-pending` | the value "that distinguishes 'wait, then retry' from a hard no" | — |
| 122 | SDK `ListSSHKeys`, `AddSSHKey`, `RunFiles` | NEW SDK methods | — |
| 123 | audit `brokered:git:branch-ns-off` | NEW; every `git_push_any_branch` push is audited with it | "clamped away from members' inline policies" |
| 126 | audit `rule_source` on rule-decided tool calls | rendered as "Decided by rule" with the verbatim value | — |
| 139 | `/healthz` → `network_policy` | NEW field, plus a boot-time audit event on an unenforced-but-allowed cluster | — |
| 142 | `GET /secrets` → `mine` | NEW; "AWS/Bedrock credential names stay admin-only, and an admin can still manage a member's secrets via `?owner=`" | — |
| 183 | `X-Wardyn-Egress-Reason` | NEW response header carrying the decision log's own rule source | "Same static strings the audit trail already records, so nothing new is disclosed." |
| 196 | `ssh.channel_rejected` | NEW audit event naming the channel type | — |
| 358 | admin `?owner=` secret-namespace READ | now audited — "names were listed, and how many" | "A member listing their own namespace, which the console does on every page load, is deliberately not recorded." |
| 359 | dropped failed-sign-in audit rows; API-token store-error counter | both now counted | "the gauge says plainly what it does and does not cover" |
| 363 | brokered `git_pat` egress decision | names the forge it reached, not the control-plane host | — |
| 364 | `PUT /site-config` `onboarding_completed_at` → response `onboarding_completed_at_ignored` | PUT no longer 400s on a body containing it; the stored mark is carried forward and the submitted copy reported as ignored | — |
| 379 | malformed egress-approval scope | the second bad shape now audits identically, "naming which of the two it was" | — |
| 383 | group-snapshot refusal error body | the remedy guidance travels with the error; the launch endpoint drops its false "policy invalid" prefix | — |
| 397 | `wardyn_audit_spool_lines` | meaning clarified | "Mid-drain the spool file can hold lines already replayed; `wardyn_audit_spool_lines` remains the backlog." |
| 399 | `wardyn_audit_spool_quarantined_total` | now survives a restart | — |
| 410 | `GET /runs/{id}/files` → `vcs` | no longer `"none"` for every `--repo` run | — |
| 413 | k8s run state | present-in-Spec-but-not-in-Status is now `STARTING` (was a terminal `FAILED`) | "an exec id absent from `Spec` was never exec'd against that pod and stays terminal" |
| 634 | console allocation form `home_override` | sent only when the field was touched | "so an untouched repoint keeps the pinned directory name" |
| 636 | `home_override` on a grant repoint | keeps the pinned override, and answers **409** rather than clearing the column | "`home_override` is optional on the wire: absent ≠ `\"\"`; **compat:** a client that wants to clear must send `\"\"`" |
| 636 | `wardyn_drive_refusals_total{reason}` | NEW metric; refusals recorded once at one chokepoint with a closed reason set | — |
| 636 | `host_roots_configured` in setup status | tightened: true only when a configured root is one a drive could actually bind | — |
| 637 | member-visible drive refusal text | names the drive and the directory, never a host path or another person's home | "**Compat:** a runbook that read the path from the member's failure hint must read the log." |
| 638 | `egress_edited_at` | a workspace edit that clears the approved-egress list now stamps it | — |
| 641 | `GET /setup/status` | no longer returns `scm`, `host_proxy` or `deployment` to non-operators; `harness` rows reduce to `provider`/`captured`/`expired` | "operators see the full response" |
| 644 | `GET /access` → `issuer` | **REMOVED** | "`provider` is unchanged." |

**CLI.**

| Line | Command | Change | Compat note |
|---|---|---|---|
| 90 | `wardynd -allow-multi-instance` (NEW flag) | wardynd refuses to start while another instance holds its database | "a command-line flag with no environment variable, deliberately" |
| 95 | `scripts/ci-run.sh` | no longer passes the admin bearer on the host `docker` command line | "no configuration change is needed" |
| 122 | `wardyn ssh-key ensure\|list`, `wardyn run wait-ready <id> --json`, `wardyn ssh <id> --json` | NEW | — |
| 150 | `make agent-image-novnc` / agent name `"novnc"` | NEW image variant, local build only | "It changed **no server code**" |
| 252 | `scripts/build-desktop-package.sh --rpm` | NEW; `.deb`, `.rpm` and tarball built from a clean git tree | — |
| 297 | `deploy/desktop/install.sh` on Linux | previously hard-refused every non-Darwin host | "The two platforms' intervals are asserted equal so they cannot drift." |
| 304 | `install.sh --uninstall` / `--purge` | NEW; first uninstaller on either platform | "`--uninstall` … **keeps** `age.key` and the Postgres volume … `--purge` destroys both, after saying exactly what becomes unrecoverable." |
| 372 | `wardyn sessions revoke --sub` | now matches either identity, "the email case-insensitively" | — |
| 516 | desktop converge `--pull always` → `--pull missing` | an unreachable registry no longer kills the launcher | "with envelope pins there is nothing for `always` to catch" |
| 520 | `wardyn-desktop.sh down` / `down --purge` | NEW subcommand | "It never removes `age.key`." |
| 600 | `install.sh` / README Helm version resolution | all three now resolve from `releases?per_page=1` (`releases/latest` 404s on a pre-release) | — |
| 631 | a mistyped subcommand under any `wardyn` group | now exits non-zero with an error on stderr instead of exiting 0 with help on stdout (eleven groups) | "A bare `wardyn <group>` still prints help and succeeds." |
| 632 | `wardyn setup fence` | never existed; the no-barrier banner now offers `wardyn setup status` (no `sudo`) | — |
| 641 | `wardyn --help` `--token` default | no longer prints `WARDYN_ADMIN_TOKEN`/`WARDYN_TOKEN` | "the token is resolved when used, precedence unchanged" |
| 641 | `wardyn ssh <run-id>` | refuses a non-UUID run id before contacting the daemon; `--print`, `--json`, `--config` refuse identically | — |
| 641 | `wardyn support-bundle` redaction | now covers `APIKEY`, `PASSWD`, `PASSPHRASE`, `AUTH`/`AUTHORIZATION`, `COOKIE`, `BEARER`, JSON-nested secrets and `--flag=value` argv entries | — |
| 641 | `wardyn setup proxy-relay` | keeps its `0.0.0.0` default but says what that exposes and warns on a non-loopback bind | "the command exists because a VM-backed Docker host cannot reach loopback" |
| 674 | `--agent` for `task_mode=exec` with an image | no longer required | "It is **not a blanket default** … Harness mode still requires an agent." |

**Policy and site-config keys.**

| Line | Key | Change | Compat note |
|---|---|---|---|
| 16 | `allowed_domains` port-qualified entry | the escape hatch for cleartext injection to a non-443 port: `allowed_domains: ["connector.internal:8080"]` | "The 443 clamp stays UNCONDITIONAL" |
| 18 | git grant `ttl_seconds` | a grant with `ttl_seconds <= 300` is no longer born stale | — |
| 23 | `mitm_hosts` | the configured-port clamp moved into the eligibility predicate itself | — |
| 25 | `inspect_forward_egress` | an unparseable tunnel channel now takes it like any other generic body | — |
| 32 | `allowed_methods` | a method the policy can never allow is refused `policy:method` before the approval flow | "It also no longer spends a `once` grant." |
| 40 | `denied_domains` | both sides canonicalized; `::ffff:93.184.216.34` no longer dodges a deny on `93.184.216.34` | — |
| 45 | `scan_budget` (NEW), 500-finding cap, `block_min_severity` keep-back | a 4 MiB total-scanned-bytes budget stops the scan honestly; a finding at or above `block_min_severity` is kept past the cap | — |
| 57 | `llm_inspection.workspace_secret_names` | a reserved secret name is refused at write time and skipped (audited by name) at dispatch | — |
| 58 | site-config egress-redirect allowlist entry | now port-qualified: the port `to` spells, else the scheme's default | — |
| 59 | api_key eligible-grant pairing | now includes the rule's header **and** format ("exactly one `%s`, no other verb, no line break") | — |
| 60 | `require_inspectable_llm` | refuses BOTH Bedrock sub-modes at schedule time | — |
| 110 | permission kinds: agents, model providers | NEW | "Both narrow: until one is enforced members keep the powers they had, and a deny bites even before enforcement." |
| 111 | governance profile `max_concurrent_runs` + per-owner secret cap | NEW; answered as a quota refusal, not a denial | "an overwrite at the secret cap still rotates a key" |
| 114 | `/home/agent/drive` | reserved mount target; the member mount is **read-only unless allowed** | "the size you see is the allocation, not a guarantee" |
| 115 | governance profiles | a profile REPLACES the site-wide default; denies are re-asserted inside dispatch | "with no assignment, every resolution is exactly what it was before" |
| 123 | `git_push_any_branch` | NEW per-run opt-out of push branch-namespace confinement | "clamped away from members' inline policies" |
| 126 | `tool_rules` | console editor refusing what the API refuses, in the same order, before the round trip | — |
| 140 | `SiteConfig.internal_hosts` | declare internal hostnames allowed to resolve to private/CGNAT addresses | — |
| 192 | `ui_apps` / `examples/policies/ui-sandbox.json` | first shipped example declaring the block | — |
| 362 | `first_use_hold_seconds` | the hold deadline is now armed before the concurrent-raise retry loop | — |
| 366 | `upstream_proxy_url` on `PUT /site-config` | now validated by the sidecar's own parser | — |
| 367 | egress redirect `to` port parsing | a query/fragment ends the authority; a port outside 1-65535 is refused at the write | — |
| 384 | reserved mount `target` `/home/agent/drive` on the record/verify path | refused on both run paths with the same wording and status | "a `target` copied from the docs could be rejected on write" |
| 391 | ceiling grant selection | now by pairing, order-independent | "A grant whose pairing no entry names is bounded by the strictest same-kind entry rather than an arbitrary one." |
| 401 | egress redirect target | no longer needs a duplicate `allowed_domains` entry | — |
| 472 | `tool_approvals=hold` on an interactive run | now a **400** naming the field (was a 201 with the field silently discarded) | "The run does not become unsupervised — interactive tool use is already supervised in the attach pane" |
| 480 | custom base-image `steps` | the user-facing surface is gone | "**The field itself stays**, documented as catalog identity: it is part of the `base_images` UNIQUE index" |
| 631 | `allowed_domains` > 256 entries | **400** `allowed_domains: at most 256 entries` | "a new 4xx a member can hit, applied before the per-entry narrowing that walks the list" |
| 631 | `auto_stop_after_sec: -1` | left alone under a ceiling with no positive maximum; the warning is gone | "the reaper skips every value ≤ 0" |
| 631 | `disk_mib` on an overlay2 host | still refuses to dispatch unless the backing filesystem is xfs, and now says so first | "the code's claim that ext4 enforced the quota was false (owner: keep fail-closed)" |
| 638 | managed drive non-`hash` home template | now refused **422** by the run-time resolver as well as by the validator | — |

**Image pins.**

| Line | Pin | Change |
|---|---|---|
| 94 | `make setup` in-network probe container | was `curlimages/curl:latest`; now digest-pinned and `--pull=never` |
| 96 | `golang.org/x/crypto` 0.55.0 → 0.56.0 | GO-2026-6354 / GO-2026-6355, reachable from the SSH gateway; "no other dependency moves" |
| 495 | `WARDYN_WARDYND_IMAGE` in `wardyn-desktop.sh` | both pins now read from the envelope |
| 503 | `WARDYN_PROXY_IMAGE` on desktop / one-line installs | both envelopes pin it by digest |
| 532 | one-line installer upgrade path | now rewrites exactly the version-derived `.env` image pins |
| 642 | `NPM_VERSION` 11.19.0 → 11.19.1 | "11.19.0 vendors node-tar inside CVE-2026-73566" |
| 642 | `CODEX_VERSION=0.149.1` (`agent-codex-cli`) | "the first release in which that image is reproducible" |
| 652 | `ghcr.io/coder/envbuilder:1.3.0@sha256:…` | tag **and** digest instead of `:latest` |

**Other user-visible behaviour changes with no new knob.** Most turn a
previously-permitted shape into a refusal, or narrow what a lane forwards; none has an
opt-out. `CHANGELOG.md` lines 13, 14, 16, 19, 21, 22, 24, 26, 27, 29, 30, 31, 36, 37,
39, 41, 47, 48, 49, 52, 62, 63, 87, 88, 91, 93, 137, 196, 201, 355, 357, 361, 365, 374,
375, 377, 378, 380, 381, 382, 388, 392, 394, 395, 400, 403, 538, 555, 577, 592, 616,
634, 636, 637, 657. The two with a stated caveat worth repeating:

- **The one-line installer now installs the `wardyn` CLI**, verified against the release's cosign-signed `SHA256SUMS` — "a mismatch is fatal, and an unavailable `SHA256SUMS` skips the CLI rather than installing it unverified". - **`--agent claude-code` resolves to `agent-base`**; the container-login lane deliberately does not follow the re-point — "**Named gap:** that ref is unpublished, so on a published install the login lane needs a locally-built image named in `WARDYN_AGENT_IMAGES` — unchanged from before, not a regression". 

#### Database migrations (0050–0061)

Every 0.6.x release ships through `0049`, so an upgrade to 0.7.0 applies twelve
new migrations on the first boot, in one run, under one advisory lock. **Take the
Postgres dump before the restart** — migrations are forward-only, there is no
`down` path, and that dump is the only rollback there is
(`docs/OPERATIONS.md` §Upgrades).

- **0050** — `secrets` gains `owned_by` (`TEXT NOT NULL DEFAULT ''`) and its
  primary key moves from `(name)` to `(owned_by, name)`, so a member can hold
  their own `anthropic-api-key` alongside the operator's. `''` means
  operator-owned: every existing row lands there and resolves exactly as it did
  before.
  - **0051** — creates `role_mappings` (`value` → `admin`/`member`, unique on
  `value`), the store-backed half of console-managed SSO role mappings. Rows are
  merged at login with the chart's `WARDYN_OIDC_ROLE_MAP` and the chart wins a
  collision. It is its own table rather than a `site_config` field because
  `PUT /site-config` is a full replace, and a stale client round-tripping an older
  document would silently drop mappings an admin had since added.
  - **0052** — creates `governance_profiles` and `governance_assignments` (a named
  policy ceiling an admin assigns to a person, an SSO group, or everyone), and
  adds `api_tokens.groups_truncated`. Deleting a profile that still binds a
  subject is refused by the FK (`ON DELETE RESTRICT`) and surfaces as a 409
  rather than silently widening its members back to the deployment ceiling.
  `groups_truncated` is **nullable, and NULL is not `false`**: a token minted
  before 0.7 has an unknown-completeness group snapshot and is treated as
  truncated wherever a group-tier assignment exists, which costs legacy-token
  holders one re-mint on deployments that adopt group profiles and nothing at all
  on deployments that do not.
  - **0053** — widens `role_mappings.role` to accept `security_admin`, dropping and
  re-adding the CHECK under the explicit name `role_mappings_role_check`. Without
  it, `POST /access/mappings` with `role=security_admin` passed API validation and
  was then refused by Postgres — a 500 on a surface the console offers. Widening
  only: no stored row can violate the new constraint, and there is no backfill.
  - **0054** — creates `user_drives` and `user_drive_grants`, the per-user storage
  an admin registers (a share the platform already mounts, or a volume Wardyn
  creates per person) and allocates to people, groups or everyone. Same
  `ON DELETE RESTRICT`, for a sharper reason: cascading would drop the allocations
  of a drive deleted by mistake while the directories they named still held
  somebody's work, now unreachable and unaudited.
  - **0055** — adds `workspaces.egress_edited_at`, the mark that says the operator
  has spoken more recently than an `always`-scoped approval verdict. The boot heal
  re-applies decided `always` egress approvals, so a host an operator promoted and
  later removed through `PUT /workspaces/{id}/approved-egress` came back at the
  next restart with no audit event; the heal now skips any decision decided before
  this stamp. **Nullable with no default, deliberately** — a `now()` default would
  suppress the heal for every decision made before the upgrade, which is the loss
  the heal exists to repair. Do not backfill it.
  - **0056** — the audit chain serializes **in the database**. The `BEFORE INSERT`
  trigger now takes the chain advisory lock first and allocates `NEW.seq` itself,
  so chain order is seq order for every writer, not only the two in-tree ones that
  remembered to lock. Before it, any other insert (psql, a seed script, a test
  helper) chained to the same head as a concurrent locked writer and the verify
  sweep reported **a tamper that never happened**. Nothing already written
  changes and no row is re-chained. **Operator-visible cost:** a session that
  inserts into `audit_events` and holds its transaction open now blocks every
  other audit append; the request-path write fails after 5s
  (`db.AuditChainLockTimeout`) and goes to the local spool, except a credential
  mint, whose audit row shares the mint's transaction and which is refused rather
  than issued unaudited.
  - **0057** — runs that trigger as its **owner**. `SECURITY DEFINER` with a pinned
  `search_path`, because `0056` replaced a privilege-free identity default with an
  ordinary `nextval()` call that checks `USAGE`: the documented split-role posture
  (an app role holding `INSERT` and `SELECT` on `audit_events` and nothing else)
  would have upgraded into `permission denied for sequence audit_events_seq_seq`
  on **every** audit insert — every write to the spool, the spool unable to drain,
  and every credential mint refused. Nothing is widened for the caller: a trigger
  function cannot be invoked directly, and a split-role deploy needs no new
  `GRANT`.
  - **0058** — repairs `0057`. The function now resolves `audit_events` and
  `audit_row_hash` **by schema** — discovered from the catalog when the migration
  applies — with `pg_temp` last in the pinned path, instead of trusting a
  hard-coded `pg_catalog, public`. Two defects, one root cause: on an install
  whose objects are not in `public` (an `ALTER ROLE … SET search_path` away, or
  stock Postgres's own `"$user", public` when a same-named schema exists),
  `Migrate` reported success and every audit insert then failed inside the trigger
  with `relation "audit_events" does not exist`; and an `INSERT`-capable role could
  have shadowed `audit_events` with a temp table and had the definer-privileged
  head read answer out of it, choosing its own row's `prev_hash`. Not a data
  migration. This is the state wardynd's boot canary refuses to serve over — a
  trigger can be present, enabled and correctly named and still not work.
  - **0059** — one directory, one principal: a partial unique index on
  `user_drive_grants (drive_id, home_override) WHERE home_override <> ''`. Two
  user-tier grants on one drive could carry the same `home_override` and hand two
  people one storage object, each mounting `/home/agent/drive` over the other's
  bytes. This is the race-free floor **under** the store's application guard, not
  a replacement for it: that guard answers the reachable case with a 409, which a
  raw `23505` is not, but cannot exclude a concurrent second writer. The index
  reaches the namespace `(drive_id, home_override)` addresses — which is the
  managed backends' `wardyn-drive-<drive-slug>-<home>`. A share's object name is
  `<host_root>/<home>` with no drive component, so the cross-drive share case is
  the store guard's alone; `host_root` lives on another table and an index cannot
  follow it there.
  - **0060** — constrains `api_tokens.role` to `admin`, `security_admin`, `member`,
  and **newly rejects a class of write that previously succeeded**. `0045`
  shipped the column unconstrained on the argument that the privileged set was the
  singleton `{admin}`, so any other value was inert; 0.7 stopped re-deriving the
  column and started copying the caller's verbatim session role, `security_admin`
  is not inert (the whole `securityOps` route group gates on it), and the set now
  grows with every tier. The CHECK is a **drift guard**, not a defence against an
  application-level attacker — writing an arbitrary role needs direct table write
  access, which the threat model already concedes.
  - **0061** — two drives can no longer name one storage object. `user_drives.name`
  is UNIQUE, but the name that *addresses* storage is its DNS-1123 fold, so
  "Corp NAS" and "corp nas" — or "Corp NAS (eng)" and "corp-nas-eng" — were two
  rows minting one volume or one claim for two sets of members with different size
  ceilings, writability and reclaim policy, caught only at mount time as somebody's
  run failing. Adds `user_drives.name_slug` (written by Go on every insert and
  update, so the indexed value is the one the object name is built from),
  backfills it once with an ASCII-pinned SQL fold, and adds a partial unique index
  over it. `host_path` is excluded: a share's object name is `<host_root>/<home>`
  and carries no slug.
  
**Apply notes.** All twelve apply on the first boot of the new wardynd, in
filename order, each in its own transaction, recorded in `schema_migrations` as it
commits. Ordering is not something an operator chooses — but three things follow
from it.

*Do not stop between `0056` and `0058`.* `0056` alone breaks the documented
split-role deployment (`permission denied for sequence audit_events_seq_seq` on
every audit insert), and `0057` alone is broken on any install whose objects are
not in `public` (`relation "audit_events" does not exist`, from inside the
trigger, after `Migrate` reports success). The three are one repair delivered in
three files, and a single boot applies them together; the only way to be stranded
in between is a migration that fails mid-sequence, which is the next note.

*A failure leaves the database half-upgraded, not rolled back.* Per-migration
atomicity bounds one file: a failure at *N* leaves `0…N-1` committed **and
recorded** and only *N* undone, and wardynd's refusal to boot is a refusal to
serve that state, not a repair of it. Putting the older binary back does not undo
what committed — and it boots anyway, then fails at the first write the older
schema no longer supports. Restoring the pre-upgrade dump is the only supported
recovery, which is why the dump has to be taken first.

*Three of the twelve fail loudly on purpose, and on a clean 0.6.6 → 0.7.0 upgrade
none of them can fire.* `0059` and `0061` constrain `user_drive_grants` and
`user_drives`, which `0054` creates in the same run, so no upgrading database
holds a row to violate them; `0060`'s column can only hold `admin` or `member`,
because `security_admin` did not exist to be stamped. They fail only on a database
already carrying 0.7 pre-release data or hand edits — and failing is the point: a
constraint that quietly skipped itself would leave the guard absent with nothing
to say so. **Before upgrading such a database**, run the three checks the
migrations themselves name, and expect zero rows from each:

```sql
-- 0059: two user-tier grants on one drive naming one directory
SELECT drive_id, home_override, count(*) FROM user_drive_grants
 WHERE home_override <> '' GROUP BY 1, 2 HAVING count(*) > 1;
-- 0060: a role outside the closed set
SELECT id, principal, role FROM api_tokens
 WHERE role NOT IN ('admin', 'security_admin', 'member');
-- 0061: two drive names that fold to one storage-object name
SELECT name_slug, array_agg(name) FROM user_drives
 WHERE backend <> 'host_path' AND name_slug <> '' GROUP BY 1 HAVING count(*) > 1;
```

**Also check before upgrading, whatever your data:** that the role in
`WARDYN_PG_MIGRATE_DSN` is the one that already *owns* the objects, if you run the
split-role posture — `0050`, `0052`, `0055` and `0060` `ALTER TABLE` on tables an
earlier release created and `0056`–`0058` replace a function `0047` created, all of
which require ownership; and that your backup covers user-drive **bytes**, which
the Postgres dump does not hold (it brings back the drive rows, and the runner
otherwise creates a fresh empty volume on first use, silently).

`docs/OPERATIONS.md` already covers all of this and is the place to point an
operator, rather than repeating it here: §Upgrades (`:3054-3075`, forward-only, and
the once-only SSO sign-out that rides the same restart), §Upgrading a one-line
install (`:3158-3189`), §Splitting the migrator and app roles (`:3191-3247`, which
enumerates the ownership-requiring migrations and spells out the half-upgraded
failure shape), §`helm upgrade` (`:3318-3323`), the audit-log boot checks and the
`ENABLE ALWAYS` hardening that survives a failed migration run (`:167-227`), the
chain's serialization and `SECURITY DEFINER` posture (`:395-410`, `:494-500`,
`:3299-3306`), and the user-drive backup and restore steps (`:72-81`, `:113-131`).

**Reversibility: none of the twelve, and none by oversight.** This repo has no
down-migration mechanism at all — no `-- down` sections, no goose, no
`+migrate Down`, and nothing in `internal/db` that could run one.
`schema_migrations` stores a filename and an applied timestamp and nothing else;
`migrateOn` reads the embedded `migrations/*.sql` in lexical order, skips what is
recorded, and applies the rest forward. A rollback is `pg_dump` restored onto the
older binary, and `install.sh` refuses a downgrade outright rather than let one
proceed.


#### Demo videos

Six episodes of the walkthrough series were re-recorded on this release and ship as its assets (01, 02, 03a, 03b, 03d, 05). The rest are being re-recorded on 0.7 and are uploaded to this same release as each take passes; until then their rows in the README and the console read *coming soon*, and no 0.6.0 footage is linked, because the console it shows has changed.

#### Known residuals and owner-gated items

**Owner-gated — the release does not claim them.**

- **The real-tenant Entra walk has not been run.** "The connector is unit-tested against a fake; the `mail` / `userPrincipalName` `$search` legs and the guest `#EXT#` UPN shape have never met a real tenant." `deploy/azure-entra-sso/` scripts the walk and `deploy/helm/wardyn/README.md:353` says "Run it end to end before trusting any of this against a tenant that matters" — the runbook is shipped; running it is the operator's. - **The real-AWS PrivateLink walk has not been run.** "No AWS account in the harness; the composed dispatch test covers the wiring, not the network." `docs/OPERATIONS.md` says the same of itself: "The composed dispatch test proves the env vars propagate, not that TLS validates." - **Adopter acceptance is pending.** The three handoffs "all came from one deployment behind a TLS-inspecting corporate proxy on a PrivateLink estate, written against 0.6.6", and "the three adopter handoffs' own acceptance tests … need that estate". The two blockers this release leads with are fixed against the reported symptoms, not against the adopter's network. 
**Named residuals — known, stated, not closed.**

- **R3's one open High, F055, was fixed after the counts above were taken** (`fix/v0.7-f055`, merged 41079162): a name that did not resolve was audited as the private-IP guard; it is now audited as itself. The R3 row still counts it as open.
- **Twenty-four of R1's first-run claims carry a wrong `fix_elsewhere` annotation.** The fixes and their evidence are real; the claim script that stamped them had a column bug. The annotations were left as they are rather than rewritten.
- **PF-48 — resident credential lanes sit above the ceiling re-assertion.** "Resident credential lanes (`WARDYN_GIT_PAT_GRANTS` with the broker off, `WARDYN_SSH_GRANTS`) are marshalled into the sandbox ABOVE the re-assertion phase, so a ceiling-denied host's PAT or key is still resident and exfiltratable through other egress. Named, not closed." The code says the same: they "are marshalled into env at `applyDispatchModeEnv`, above this phase … a resident credential for a denied host still meets that deny on every named dial." - **PF-1 residual — an unassigned member still selects stored policies unclamped.** "Closing it deployment-wide is an `all`-subject assignment, by design." The governance code keys every refusal on `ceiling.Profile != nil`, "never on the ceiling's contents — an UNASSIGNED member is byte-for-byte today here (PF-1's stated residual)". - **An egress redirect's `to` cannot be an IPv6 literal.** R3 F005, severity Info, **status `open`**: "No site-config field can name an IPv6 literal: `hostrules.HostOf` cuts at the first ':' inside the brackets … Every such value is rejected at PUT with 'invalid URL'/'invalid host'; the operator has no spelling that works." Pre-existing, unrelated to this campaign, unfixed. Use a hostname or an IPv4 literal. - **The redirect probe's SNI swap has one edge it can green wrongly.** The probe now "dials the target while presenting the original hostname, which is what a real run does" (fixed, `CHANGELOG.md:402`, with `WARDYN_PROBE_TO_CONNECT`). The residual: "The narrow edge is an **Ecosystem** redirect whose `To` is a literal IP: the agent's tool dials the To address directly, so its SNI is the To IP, and the probe's From-SNI swap would green a config the real tool fails." `redirectProbeTo` still fires the swap on `net.ParseIP(toHost) != nil` alone and reads no redirect kind. "Confirm on the real-mirror walk before changing probe code." - **F055 — a DNS failure is still reported as an SSRF deny, and closing it is an owner decision.** R3 F055, severity **High**, status **`deferred-proposed`**: "A DNS/resolver failure is audited and reported to the sandbox as an SSRF private-IP deny, with a remediation instruction to widen the SSRF guard." It is deferred because the fix is all canon surface: "The fix REQUIRES a new quoted audit decision string in the decision stream (`builtin:resolve-failed`), a new row in `docs/UI-SANDBOXES.md`'s `rule_source` table, and a new operator-facing `X-Wardyn-Egress-Detail` sentence / 403 body". Recorded as an owner decision: "F055 [High] is `deferred-proposed` (canon-strings law) and is an OWNER decision (FOLLOW-UPS P0)." **One of exactly two High-severity findings in the whole campaign that ship neither fixed nor rejected** (the other is R4 F107, below). - **R4 F107 — a failed sign-out looks identical to a successful one.** Severity **High**, status **`deferred-proposed`**: "A failed sign-out is reported only to the devtools console, so the human sees the sign-in gate while the HttpOnly SSO session stays alive … a page reload re-enters the console through the still-valid session cookie without any credential being presented." It is deferred with the other five R4 items that need new console copy under the canon rule: "R4 DEFERRED-PROPOSED six (owner mock round; new copy/state under canon rule (c)): R4-F009 preflight `setup_items` in the rail, R4-F052 the four `user_drive_unavailable` sentences …, R4-F070 security-tier probes surface, R4-F093 preview `home_subject`/`warning` rows, R4-F107 sign-out-failed strings, R4-F144 terminal keyboard-trap chord." - **R4 F144 — the cockpit terminal is a WCAG 2.1.2 keyboard trap.** Severity Medium, status `deferred-proposed`: "Keyboard focus cannot leave the cockpit terminal … Tab, Shift+Tab and Escape all stay in the PTY textarea." Deferred with the same six; it is the one on that list with an accessibility-conformance name attached. 
**Closed since the handoff was written — do not re-publish these.**

- **The ssh cockpit widget predicate.** `local/HANDOFF-0.7-RELEASE.md:142-146` says the widget is owner-only while `ConnectSSHCard`'s `mayAttach` is owner-or-admin. That is no longer true in the tree: `widget-registry.ts:189-192` now reads `ctx.run.state === "RUNNING" && ((!!ctx.principal && ctx.run.created_by === ctx.principal) || ctx.operator)` with the comment "run-detail-ssh.tsx:50 verbatim, plus the RUNNING check: owner OR admin", and `run-detail.tsx:580-584` asserts "all three must agree". What survives is a **test** gap — R4 F129, "The SSH widget's owner-only availability gate has no test, and the canvas suite is constructed to avoid it", status `fix-claimed`, verification `unverified-budget`. 
**Verification debt owed before 0.7.1 can claim "reviewed".**

Quoted from `local/review-0.7/FOLLOW-UPS-0.7.1.md:19` (P1) and :28 (P4), and §"Deferred review work" (30-45):

- **R2 (run plane) — NO review in 0.7.** The one subject area with no verification at all. 75 candidate rows (4 High, 36 Medium, 26 Low, 9 Info) sit untriaged in `~/.claude/verify-runs/wardyn-0.7-r2-20260904-161449/inbox/round-1`.
- **R5 — 137 fix claims unverified by reviewer**; **R6 — 70**; **R3 — no blind round 2** (a Critical/High-only adversarial pass was the 0.7 substitute); **R4 — no round 2**, 51 unverified; **R7 — round 2 dropped** by owner decision ("treat R7 like R5/R6"), 27 claims unverified; **R1 — 62 wave fixes "fixed-unverified, gate-green"**.
- **Low/Info residue, all runs, deliberately unfixed**: R1 182 · R3 68 · R4 83 · R5 101 · R6 64 · R7 59.

**Published threat-model residuals.** `threatmodel/THREAT-MODEL.md` §5 now lists 39;
`v0.6.6` listed 27. **#28–#39 are new in 0.7.** The ones a 0.7 operator should read
before turning a 0.7 feature on:

- **#28** — a configured `WARDYN_TRUSTED_CA_FILE` "makes the corporate middlebox a trusted issuer for `wardynd`, every proxy sidecar, and every sandbox — not merely tolerated on one hop." - **#29** — "The operator's model-provider credential is disclosed to whatever host they nominate as the internal gateway." - **#30** — "`/healthz` is anonymous and now also names the k8s substrate's NetworkPolicy posture". - **#31** — "Directory autocomplete grants the control plane read of the WHOLE directory". - **#32** — "The one-line installer trusts the release ORIGIN: the compose definition has no digest". - **#33–#37** — the five user-drive residuals: a share extends trust to whoever administers the host and the share (#33); two drive-and-directory pairs can name one object, refusing one person's run (#34); a drive inherits the member-mount check-then-bind race (#35); "A drive's SIZE is an allocation Wardyn never enforces, on any substrate" (#36); renaming a drive orphans every object already provisioned under it (#37, with 0.7.1 named as the fix target). - **#38** — "A per-user API token's GROUP SNAPSHOT is frozen at mint, with no expiry". - **#39** — "A group claim the IdP FILTERS is indistinguishable from a complete one, so a shrink-the-claim workaround loses grants silently." 

### Security

- **A name that did not resolve is audited as itself, not as the private-IP guard.** A resolver outage, NXDOMAIN or a zero-answer lookup was denied under `builtin:private-ip` with advice to declare the host under `internal_hosts` — advice that cannot fix a DNS fault and points at widening an SSRF control. The deny now carries its own reason, `builtin:resolve-failed`, its own operator sentence (check the sandbox's resolver, not the allowlist) and its own row in the audit panel; `builtin:private-ip` is reserved for a real private-address block. Fails closed exactly as before. (R3 F055)
- **The literal-IP guard reads a zone-suffixed IPv6 literal as the literal it is.** `net.ParseIP` returns nil for `fe80::1%eth0` (and the RFC 6874 authority form `fe80::1%25eth0`) while `netip.ParseAddr` parses it and `net.Dial` dials it, so the zone id alone decided the verdict: the canonical `fe80::1` was denied at step 0 while the zoned spelling reached policy, could raise a first-use approval for a link-local address, and under a corp upstream was handed over verbatim. Both consumers of the deny-only gap-filler now cover it. The threat model's octal example is corrected with it: `0251.0376.0.1` is `169.254.0.1` (link-local), not `127.0.0.1`. The deprecated IPv4-compatible form (`::127.0.0.1`, `::169.254.169.254`, RFC 4291 §2.5.5.1) is closed on the other axis — `net.ParseIP` PARSES it, but its embedded IPv4 is invisible to `To4()`, so it was admitted at step 0 AND by the resolve-based guard; `isBlockedIP` now re-runs the embedded address the same way it does for a NAT64 prefix, and `::8.8.8.8` stays reachable because only that address decides. (R3 F105, F114)
- **A body-bearing method the vendor does not document is uninspected, not quiet.** The LLM classifiers answered "not prompt-bearing" for anything that was not a POST while the forward path scans POST, PUT and PATCH alike, so `PUT /wardyn/llm/anthropic/v1/messages` carried a secret to the vendor with the operator's brokered credential under `mode=block`, allowed, with no scan block — audit-indistinguishable from a bodiless `GET /v1/models`. Both classifiers now share one definition of "body-bearing" with `hasScannableBody`. (R3 F088, F112)
- **The two broker lanes strip the sandbox's credential headers through the same one definition the injecting lanes use.** `handleGitBroker` and `handleGitPATBroker` each re-spelled a narrower `Header.Del("Authorization")`, so a clone reached the forge carrying the brokered Basic auth AND the sandbox's own `Private-Token` (GitLab's first-class access-token header), `X-Api-Key`, `Cookie` and the rest. The list itself is wider now: `Cookie`, `Private-Token`, `X-Goog-Api-Key`, `X-Amz-Security-Token`, `X-Functions-Key`, `X-Access-Token`, `Anthropic-Api-Key`. (R3 F104)
- **Cleartext credential injection is refused by a rule, not at the single port 443.** The clamp keyed on one port, so `http://<host>:8443/…` still put the operator's credential on the wire unencrypted. Injection over cleartext is now refused to any host this proxy only ever speaks TLS to, and to any port the operator did not author in the allowlist (`allowed_domains: ["connector.internal:8080"]` is the escape hatch); port 80 and https are unchanged. The 443 clamp stays UNCONDITIONAL — an authored `host:443` entry does not re-admit cleartext injection to the TLS port, which matters because an `api_key` grant appends the bare host beside the port-qualified one so both entries coexist. (R3 F110)
- **The brokered-credential path is bounded by `WARDYN_GIT_APPROVAL_TIMEOUT`.** The env var bounded only the approval wait's timer while every HTTP call under it rode a control-plane client with no timeout, so against a control plane that accepts a connection and never answers a clone blocked forever — with `WARDYN_GIT_APPROVAL_TIMEOUT=1s` set. One budget now covers the mint, the wait and every poll, and the control-plane client carries a whole-request ceiling. (R3 F070)
- **A short-TTL git grant serves one clone from one mint, and the mask covers the username the lane actually sends.** The broker cache used the injected-credential refresh margin (5m), so any grant authored with `ttl_seconds <= 300` was born stale and the second half of a clone re-minted — fatal for a single-use grant. Separately, the mask registered `base64(<mint username>:<token>)` while the GitHub lane authenticates as the constant `x-access-token` (a `github_token` mint states no username), so the rendering actually on the wire was unmasked. (R3 F120)
- **Inspection's memory bound covers the buffer's lifetime, not just the scan.** The concurrency slot was released the moment the scan returned while the caller still held the whole buffered body for its upstream round trip, so N stalled requests retained N x 32 MiB with no slot held — around the very cgroup arithmetic the slot exists to enforce. The buffered bytes are now charged to a process-wide byte budget until the caller releases them, and a request that cannot be charged fails closed. (R3 F074)
- **The decision log is bounded in bytes, and a truncated scan says so.** The findings cap bounds how many findings a request reports, not how large they are: a `field_path` is built from agent-authored JSON keys, so a 0.3 MiB body of long keys produced a 46 MB decision log under a cap that never fired — over the control plane's 1 MiB limit, so the audit record was refused and silently lost while the whole line still went to stdout. Field paths are capped at 256 bytes, a refused decision POST now counts as dropped (and is summarised), the truncation rides on the wire (`findings_capped`, `findings_past_cap`, `findings_total`) even when an earlier skip reason claimed `skip_reason`, `finding_count` in the audit row is `findings_total` — the number of findings the scan produced before the cap truncated the list, COUNTED as they were produced rather than inferred from `findings_past_cap` (block mode reports a past-cap finding it kept back for severity while also counting it past the cap, so the sum double-counts every keep-back), and the severity keep-back applies in every mode — in `alert` it displaces a lower-severity finding rather than letting 900 cheap ones evict the operator's high-severity one from the alert. (R3 F075)
- **The literal-IP guard covers the spellings a resolver accepts and Go does not.** `127.1`, `0x7f000001` and `2130706433` are all `127.0.0.1` to `inet_aton`, and `0251.0376.0.1` is `169.254.0.1`; with a corporate upstream configured every one of them was handed to the operator's proxy unvetted; both the guard in `evaluate` and `egressTarget`'s upstream branch now re-run the block check on the `inet_aton` reading. Deny only — a spelling the operator did not type inherits no `allowed_domains` grant. (R3 F105)
- **The own-subnet/control-plane exclusion fails closed and covers every control-plane address.** A failed startup capture used to make the two admin-authored exceptions to the private-IP guard fire MORE widely rather than less, silently; it now refuses every lift and trust and logs, and a `wardynd` behind more than one A record has all of its addresses excluded, not just the first. (R3 F002)
- **A port-mismatched MITM host can no longer be re-admitted by the LLM branch.** The configured-port clamp moved into the eligibility predicate itself, so a Bedrock/gateway host that is also on `mitm_hosts` is not TLS-terminated and credential-injected on a port the operator never configured. (R3 F009)
- **Content inspection and the forwarder agree on where the path ends.** A `%23`/`%3F` suffix made the classifier read one path and the upstream receive another (`/v1/messages`), forwarding a prompt body unscanned under `mode=block`; the upstream request is now built from the same parsed path the classifier judged. (R3 F035)
- **A MITM'd body is inspected on the strength of its channel, not its host classification.** Widening the Bedrock matcher to the PrivateLink form had moved `vpce` Bedrock hosts onto the unscanned branch; a tunnel whose channel cannot be parsed now takes `inspect_forward_egress` like any other generic body, and says so in the decision row when it is not inspected. (R3 F036)
- **The brokered LLM route keeps the private-IP guard on the public vendor host.** The relaxed per-request vet is scoped to a control-plane-authored gateway again, as the threat model and `docs/OPERATIONS.md` already stated; without a configured gateway, `api.anthropic.com`/`api.openai.com` take the ordinary SSRF-guarded resolve. (R3 F087)
- **The plain forward lane inspects model traffic like the tunnel does.** An absolute-form `POST https://api.anthropic.com/v1/messages` sent as an ordinary forward request took no inspection at all — forwarded with the brokered credential, unscanned even under `mode=block`, under a single `allow / policy:allowed` row with no scan block and no blind marker; it now takes the same per-endpoint classifier `handleConnect` does, and an LLM host this lane cannot inspect emits the honest one-per-host coverage signal. (R3 F103, F141)
- **Plain-lane injection strips the sandbox's own credential headers.** The forward lane set the brokered header and left an agent-supplied `Authorization`/`X-Api-Key` in place, so which credential the upstream honoured was the upstream's choice; the strip list `forwardInspectedLLM` has always applied is now one definition shared by both injecting paths. (R3 F104)
- **A brokered credential is no longer attached to a cleartext request aimed at the TLS port.** An `api_key` grant's exact allowlist entry is port-blind, so a sandbox could send `POST http://<injected-host>:443/…` on the forward lane and have the operator's credential put on the wire unencrypted; that shape is now uninjected (the upstream answers 401). An https request, and an ordinary plaintext connector on its own port, are unchanged. The remaining half — an operator cannot yet declare that an `api_key` host is https-only — is a follow-up. (R3 F110, partial)
- **An https absolute-form forward request is vetted, dialled and audited as port 443.** It was hardwired to 80, so the policy matched, the row recorded and the TLS handshake ran against the wrong port. (R3 F141)
- **Every unrecognised POST on a brokered LLM route is honestly uninspected, not quiet.** The classifiers' enumerated default streamed the vendors' content-upload surface (Anthropic `POST /v1/files`, OpenAI `/v1/files` and `/v1/audio/*`) through with the brokered credential and no scan block, and let a strict `on_scanner_error=block` operator's refusal be bypassed by choosing a different suffix; the default arm is now fail-closed and `THREAT-MODEL.md`'s published gap list says so. (R3 F112)
- **A method the policy can never allow raises no approval and takes no hold slot.** The method restriction now runs before the first-use approval flow, so a POST under `allowed_methods: [GET]` is refused `policy:method` instead of parking a connection in `wait_for_review` and filling the operator's queue with decisions the next step refuses. It also no longer spends a `once` grant. (R3 F032)
- **The proxy sidecar refuses a config key it cannot honour.** The sidecar image is pinned by the operator independently of wardynd, and a lenient decode used to accept a newer daemon's config with `err == nil` and silently discard every unknown key — `upstream_proxy_no_proxy`, `trusted_ca_pem`, `internal_hosts`, `llm_upstreams`, `pat_grants` — leaving a routing document half in force; the decode is strict now and names the offending key. Bump `WARDYN_PROXY_IMAGE`/`k8s.proxyImage` in lockstep. (R3 F029)
- **The fail-closed refusals an `on_scanner_error=block` operator buys now have regression pins**, as does the upstream lane's literal-IP guard: its named regression passed with the entire guard deleted, because it ran under a default-deny allowlist and asserted no rule source. (R3 F090, F004)
- **The proxy-side mask covers every rendering of a credential, not one.** It registered `Bearer <tok>` but not the bare `<tok>` a vendor echoes back, the git installation token but not the `base64("x-access-token:…")` actually on the wire, and the minted PAT not at all — so an error body or decision line carrying the other rendering left the proxy in cleartext. One definition now registers all of them. (R3 F155)
- **`responses` and `embeddings` are refused fail-closed in both spellings.** The bare form (the default, gateway-less spelling) fell through to "not prompt-bearing" and was forwarded with the brokered credential unscanned. (R3 F088)
- **The git_pat broker builds its upstream URL from validated pieces.** A `%23`/`%3F` in the sandbox-supplied path satisfied the smart-HTTP verb check and then re-split the concatenated URL, sending the brokered PAT to an arbitrary path on the granted forge — the forge's REST API included. (R3 F085)
- **The proxy's decision log masks JSON-escaped secrets.** `maskDecisionBytes` matched raw bytes while both its callers hand it marshalled JSON, so an `&`/`<`/newline-bearing registered secret survived into the stdout decision line and the control-plane decision POST. (R3 F125)
- **A run cannot grow the first-use approval cache without bound.** Past a 4096-host cap a new host resolves to pending — fail closed, no approval raised — where 20,000 invented hostnames previously produced 20,000 cache entries and 20,000 `PENDING` rows. (R3 F071)
- **A denied literal IP is denied in every spelling of it.** `denied_domains` matched the raw request string while the trusted-literal path matched the canonical address, so a policy denying `93.184.216.34` still allowed `::ffff:93.184.216.34` under `allow_all_egress` — and a non-canonically spelled deny entry was a dead rule. Both sides are canonicalized now. (R3 F130)
- **The unconditional IP guard covers the rest of the non-globally-reachable ranges.** `240.0.0.0/4` (which already had `255.255.255.255/32` listed inside it), the three TEST-NETs, the deprecated 6to4 relay anycast, IPv6 site-local `fec0::/10` and 6to4 `2002::/16` are denied regardless of policy, and the tables are now diffed against the IANA special-purpose registries by a test. (R3 F115, F131)
- **A brokered forge's `git_pat` is withheld from BOTH halves of dispatch.** The proxy's PAT-broker allowlist was built from the unfiltered grant map, so a PAT dispatch withheld from the sandbox and audited as withheld was still published to `/wardyn/git/<host>/`; only the mint handler's refusal closed it. (R3 F216)
- **The threat model no longer justifies the unconfined `git_pat` push with an impossibility.** Since the 0.7 PAT broker terminates that push on the proxy's own cleartext route, leaving it outside branch-namespace confinement is a stated scoping decision, not something no parser could bind. (R3 F121)
- **The model-gateway refusal predicate has one body.** The control plane's boot validator and the proxy's per-request re-check were byte-identical copies coupled by a comment, and the proxy copy's NAT64 arm was unpinned; both now call `ipguard.GatewayIPRefused`. (R3 F089)
- **Content inspection bounds findings-per-request and scanned-bytes-per-request, not just per-span.** A body split into many sub-`max_scan_bytes` spans (an agent-controlled JSON body, tool input, or attachment) previously fanned out into unbounded CPU cost and, separately, unbounded findings copied verbatim into the decision log mirrored to stdout and POSTed to the control-plane audit — measured at 600,000 findings / ~104 MiB of decision-log output for one 22.4 MiB request, and 14.6s of proxy CPU for another with zero findings. A 4 MiB total-scanned-bytes budget (`scan_budget`) now stops the scan honestly, the same Skipped/SkipReason shape as `span_oversize`. A separate 500-finding cap (`findings_capped`) bounds only the size of the reported decision log — scanning itself is not stopped by it, and a finding at or above the policy's `block_min_severity` is kept past the cap (up to a hard ceiling) so `mode=block` cannot be bypassed by fanning out cheap low-severity noise ahead of the real secret. (R3 F075, F073)
- **An undecodable content-inspection attachment is no longer reported as "inspected clean."** `extractAnthropicAttachments` now tries every base64 alphabet a real client might use before declaring failure, and a genuine decode failure is recorded `Skipped{attachment_decode_error}` — honoring `on_scanner_error=block` like every other scanner-error case — instead of silently passing as a clean scan. (R3 F056)
- **The OpenAI/Codex content-inspection channel now scans the system prompt.** `extractOpenAIChat` previously read only the last message, so a system prompt carried as a `role:"system"` message (OpenAI has no top-level `system` field the way Anthropic does) was scanned only in the degenerate case where it was also the last message — understating THREAT-MODEL.md 5.1a's own coverage claim on exactly the channel it was meant to bound honestly. (R3 F049)
- **A hostname is vetted under a corporate upstream too.** The private/loopback/metadata guard held only for the literal spelling when an upstream proxy was configured; a name resolving into blocked space is now denied before the corp proxy is asked to dial it, with the two residuals stated in `threatmodel/THREAT-MODEL.md` §4.2: a name this proxy cannot resolve at all is forwarded unvetted, and — the target being sent by name — a name that answers differently to this proxy and the corp proxy is bound at check time only. (R3 F008)
- **`wardyn-aws-sso` no longer shells out to the `aws` CLI with the live SSO access token as a command-line argument.** The best-effort account/role lookup (`resolveAccountRole`) put the token on that child process's argv, readable by any `/proc` reader sharing the sandbox's PID namespace; it now calls the SSO portal API directly over HTTP (the token in the `x-amz-sso_bearer_token` header the SSO portal's `authtype: none` operations read, through the sandbox's egress proxy) instead of exec'ing `aws`. A blank account/role is not actually accepted by the control plane (`internal/api/harnesscred.go`'s `awsSSOBlob.valid` requires both non-empty, and rejects a half-resolved capture with 400), so the lookup could not simply be dropped. (R3 F160)
- **`wardyn-toolgate` now names a control-plane outage instead of blaming the human.** A poll error while waiting on an approval was silently treated as PENDING with no log anywhere; it is now logged to stderr (rate-limited) and, if every poll failed through the deadline, the deny message says so instead of reading as "no human decided in time". (R3 F159)
- **`wardyn-tetragon-ingest` now logs when its export file cannot be opened.** A typo'd or unreadable `-export` path retried silently forever while heartbeats and the stats loop kept printing as if the sensor were healthy; the open failure (and its recovery) is now logged. (R3 F158)
- **The Bedrock bearer's TLS-MITM entry names its port.** It was authored as a bare host, which the proxy treats as any-port, so an agent could CONNECT to the Bedrock data-plane host on a port nobody configured and have that tunnel terminated with the Wardyn leaf and the operator's `Authorization: Bearer` injected onto whatever answered. The entry is now `host:port` (443 unless `WARDYN_BEDROCK_BASE_URL` names another), matching what an artifact redirect has authored since 0.6. (R3 F037)
- **The sandbox/host-sensor auth boundary now leaves a trace when it refuses.** `internalAuth`, `internalAuthGroundtruth` and the internal approval handler's two 400s — including the one that refuses a sidecar trying to raise a `credential` approval — answered 401/400 and recorded nothing: no audit row, no log line, no metric. All of them now emit the public lane's own rate-bound `auth.failed` row, with an actor naming which boundary refused, and share its `wardyn_auth_failed_suppressed_total` counter so a burst from inside a sandbox is visible without flooding the log. (R3 F068)
- **The local-mode `X-Wardyn-Principal` override cannot choose whose secrets a run mints.** It steered the run identity's `sub`, which is the secret-namespace selector for broker mints and proxy-side injection — and a stored row owned by the named principal wins over the operator's — so on a database carrying member-owned secrets from an SSO-configured era, a local caller could mint another member's `git_pat` or `ssh_key` by naming them in a header. The header keeps its documented attribution job; the namespace now comes from the principal wardynd injected. (R3 F099)
- **A login sandbox cannot choose process-global redaction patterns.** The AWS SSO capture registers its uploaded token with the login run's own mask set instead of the process-wide one; the credential is still masked everywhere it is used, because the dispatch that actually selects it registers the stored value globally. (R3 F007)
- **A typo'd flag no longer prints your secrets.** Environment-supplied values are applied to the flag's variable instead of its registered default, so `-help` and any flag parse error show only the compiled default — `WARDYN_ADMIN_TOKEN`, `WARDYN_AGE_KEY`, `WARDYN_OIDC_CLIENT_SECRET`, `WARDYN_GROUNDTRUTH_TOKEN` and `wardyn-rec`'s `-run-token` were written verbatim to stderr, and thus to container logs. Precedence is unchanged: an explicit flag still beats the environment. (R3 F157)
- **`llm_inspection.workspace_secret_names` takes the reserved-name guard every other credential sink takes.** Resolution puts the named secret's plaintext in the proxy sidecar's policy, so a policy naming `wardyn-signing-key`, `wardyn-session-key`, a harness OAuth blob or a resident AWS SigV4 secret is now refused at write time and skipped (audited by name) at dispatch. (R3 F126)
- **An egress redirect trusts its target on the port it named, not every port of the address.** The allowlist entry a site-config redirect adds is now port-qualified — the port `to` spells, else the default of the scheme `to` spells (`80` for an explicit `http://`, `443` otherwise) — matching the port its TLS termination and token injection already used — so a `to` on a literal IP no longer opens 22 or 5432 on that address. (R3 F106)
- **A member cannot re-home the operator's blessed api_key secret under a header of their own.** The eligible-grant pairing an inline or profile grant must match now includes the api_key rule's header and format, not only its host and secret name, and the policy path applies the same format rule the integration authoring path always did (exactly one `%s`, no other verb, no line break). (R3 F097)
- **`require_inspectable_llm` refuses BOTH Bedrock sub-modes.** The bearer sub-mode was admitted as "inspectable" because the proxy TLS-terminates it — but Wardyn has no Bedrock extractor or prompt-bearing channel, so such a run was scheduled with zero scan coverage under a policy that promises the opposite. Both Bedrock sub-modes now fail closed at schedule time; `threatmodel/THREAT-MODEL.md` 5.1a says so. (R3 F048)
- **A captured AWS SSO credential is bound to what the operator asked for, not just to which run may upload it.** The sso-token upload now refuses a blob whose `region` or `start_url` disagrees with the operator's configured SSO region and the access-portal URL that login run was launched with, and refuses a second capture from the same run — so code inside the vendor login sandbox can no longer substitute an attacker's IdP session as the operator-wide credential every later Bedrock run authenticates with. (R3 F006)
- **A `deny·always` on a Bedrock host is refused like any other model-provider host.** The guard that exists to stop an operator permanently bricking a workspace's model access consulted the anthropic/openai-only host predicate, so `bedrock-runtime.<region>` (and a `WARDYN_BEDROCK_BASE_URL` endpoint) — which carries proxy-side bearer injection exactly as the other lanes do — was accepted. The same narrow predicate gated the artifact-redirect veto, so an operator-wide site-config redirect whose `To` named a Bedrock host could still author an artifact-token injection on it, and `buildInjector`'s by-host map is last-write-wins; both reject-direction sites now share one predicate. (R3 F019)
- **The mint route fails closed when it cannot read the run's grants.** `brokeredForgeMintKind`, the residual check for a policy stored before grant-lane exclusivity existed, answered a `ListGrantsByRun` error by minting the credential anyway; it now answers `503`. A daemon with no grant store still mints, which is not a failure but the absence of anything to check. (R3 F098)
- **`wardyn_egress_denies_total` counts only what it says it counts.** A failed upstream dial (`builtin:dial-failed`, on a request policy ALLOWED) and the synthetic `egress.decisions.dropped:<n>` audit-fidelity summary both moved the series exposed as "denied by policy"; both still record their `egress.deny` audit row, neither moves the counter. One class rides along with the first exclusion: the brokered LLM route reuses `builtin:dial-failed` for a guard refusal of the configured model gateway, so that refusal stops moving the counter too — it still records its `egress.deny` audit row, and separating it needs a distinct `rule_source` at the emitting site. (R3 F065)
- **The per-run secret-mask registry de-duplicates, and the masker is built once per change instead of once per masked byte.** `Registry.Add` appended unconditionally while its sibling `AddGlobal` de-duplicated, so a leased `git_pat` run accumulated one entry per git operation — and every PTY chunk and every audit event then re-cloned and re-sorted the whole set (a 32 KiB chunk: 29.8µs at one secret, 1.51ms at 1024). No cap was added: dropping a registered secret past a ceiling would fail open. (R3 F076)
- **The mint 409 `code` values have one home.** They were declared twice — the API handler and `wardyn-git-helper` — and every server-side test compared the decoded JSON against the same constant the handler wrote, so all four wire values could be renamed with the suite green; both sides now read `types.MintConflict*`, the tests assert the literals, and the previously untested `denied` arm is covered. (R3 F134)
- **The kill switch now audits every credential the run actually minted, and says the truth about each kind.** The revoke cascade read only the approvals whose `minted_jti` was burnt, so an auto-mintable grant (which creates no approval row) and a leased `git_pat`'s 2nd..Nth mint produced a live credential and NO `credential.revoke` row, while the threat model published step 4 as "every minted credential for the run"; it now enumerates the run's successful `credential.mint` rows as well, and each row carries the per-KIND note — a `git_pat`/`ssh_key` says the operator must rotate the secret at the forge rather than claiming GitHub's TTL-expiry semantics. (R3 F096, F122, F013)
- **The broker's mint transaction pins READ COMMITTED.** Its in-transaction `credential.mint` audit write inherited `default_transaction_isolation`, so on a pool set to REPEATABLE READ two writers racing on the audit-chain lock could fork the hash chain; `TxBeginner` now exposes only `BeginReadCommitted`, and the age-key rekey transaction pins the same. (R3 HANDOFF-1)
- **Console hardening (R4 review, round 1).** 16 High and 41 Medium console defects fixed on the 0.7 RC; every fix carries a vitest pin, and browser-only behaviours carry Playwright pins run before release. Highs:
  - Run detail pulls the WHOLE fleet's approvals and filters in the browser, defeating the ?run_id= predicate wardynd grew for exactly this (F002).
  - Permissions paints all six kinds 'Not enforced', with their member-powers prose, from a snapshot the fetch never returned (F015).
  - The per-widget ErrorBoundary added to the canvas was not extended to FocusMode, which renders the same widget components - one throwing widget still blanks the whole cockpit there (F021).
  - putSiteConfig strips only `integrations`, so every Corporate-network save 400s once onboarding has completed (F029).
  - AllowedHostsCard.remove() does two writes on two different tiers and reports a landed partial write as a total failure (F030).
  - RecordPane enables every Approve-host control for a security_admin, but approveHosts writes the operatorOnly requirements route (F031).
  - Governance's Limits column ignores max_concurrent_runs, so a quota-only profile reads 'None' (F032).
  - Role preview labels a security_admin verdict 'member' (F033).
  - GettingStarted routes only role==='member' away from the deployer funnel, so a security_admin gets the admin funnel built from a REDACTED status (F034).
  - pnpm overrides pin brace-expansion BELOW the patched floor, and `make npm-audit` (--prod) is structurally blind to it (F065).
  - No console request has a deadline: wfetch (and health()'s raw fetch) never bound or abort a request, so a daemon that accepts and never answers freezes the whole console with no error, retry or recovery (F079).
  - The Preflight verdict rendered beside Launch is never invalidated when the run body changes, so Review stops predicting launch (F090).
  - The declared e2e gate is RED: episode-catalog.spec.ts hard-codes 2 member-chip rows, and the 0.7 user-drives episode 04d made it 3 (F112).
  - The approval-decision wire body (decision_scope / decision_expires_at) is pinned by zero tests, and the daemon accepts a wrong key silently (F125).
  - Audit keeps showing a green "Live · appending" chip after the audit feed has stopped answering (F132).
  - Enforcement confirm states "0 members are bounded" when the count could not be read (F133).
  - Mediums: F001, F003, F004, F005, F010, F014, F020, F023, F025, F027, F035, F037, F040, F041, F049, F051, F061, F062, F063, F066, F069, F073, F074, F077, F091, F092, F106, F110, F111, F116, F117, F118, F119, F126, F127, F128, F129, F137, F141, F142, F143.
- **`make setup` refuses what wardynd refuses.** It no longer warns and boots through a `-gen-age-key` mint that produced no key (an ephemeral key makes wardynd fail closed and crash-loop on its second boot against the persistent Postgres volume — the condition `install.sh` and the Helm chart already refuse) or a `deploy/compose/.env` carrying both `WARDYN_LOCAL_MODE=true` and `WARDYN_OIDC_ISSUER` (wardynd refuses to boot on that pair). Both refusals name the fix; no override was added, because `WARDYN_ALLOW_LOCAL_MODE_WITH_OIDC` is not forwarded by the compose file. The one-line installer discloses the SSH listener it enables on every install and says when a pre-v2.1.1 Compose left "Wardyn is running" unprobed. (R5 F150, F012, F153, F231)
- **The confined replay applies the same egress-provenance gate as run-create.** With the operator-set provenance gate on (its 0.7 default), a confined replay's allowlist is now narrowed to operator-approved hosts exactly as a launch is; previously the replay path built its allowlist from every recorded host, scan-seeded ones included. (R6 F008)
- **`install.sh` waits for health before it says "Wardyn is running", and never discards `WARDYN_PORT` in silence.** The upgrade path names the discard and the two-step remedy instead of ignoring an explicit port; the banner follows a `docker compose up --wait` (probed for support) and a failed wait prints the daemon log and exits. (R6 F036, F037)
- **`wardynd` refuses to start while another instance holds its database.** It takes a single-instance advisory lock at boot — the runtime half of the one-replica control that until now existed only as the chart's render-time `replicas > 1` refusal, which `kubectl scale`, an HPA or a non-Helm replica edit defeated silently. The failure it guards is real: the secret-masking registry is in-memory and process-local and fails open, so a recording uploaded to an instance that did not serve the run's proxy injection is persisted verbatim, live credentials in cleartext, with a `success` audit event. Pass `-allow-multi-instance` (a command-line flag with no environment variable, deliberately) to start anyway and accept that; the chart's `allowMultiReplica: true` now passes it for you, and its `strategy: Recreate` (unchanged) is what lets an upgrade converge — under RollingUpdate the new pod would refuse while the old one holds the lock. Honest ceiling: an advisory lock dies with its Postgres *session*, so a database restart or failover releases it under a still-running daemon — at most one steady-state instance, not mutual exclusion. The lock holds one Postgres connection for the process lifetime: `pool_max_conns=1` now boots with a warning, and the recommended floor with the ground-truth rotator enabled is 4 (was 3) — `docs/ENV.md`'s `WARDYN_PG_DSN` row says so. (R5 F197)
- **`wardynd` refuses to start when its own audit-chain canary comes back unchained.** After the trigger catalog check, boot appends one synthetic audit row inside a transaction it always rolls back and asserts it came out chained (`row_hash` set, `prev_hash` matching the head read under the same lock); a chain that demonstrably does not chain refuses the start even when every trigger is present, enabled and correctly named — the `0057` state `0058` repaired is exactly this case. A canary that could not run at all (lock busy, statement cancelled) logs at ERROR and boot continues, since that is bounded and self-clearing. In the split-role posture (`WARDYN_PG_MIGRATE_DSN`) the canary now runs on BOTH pools — the migrator's, at the end of `Migrate`, and the app role's, the connection every audit row is actually written on. (R1 F201)
- **`install.sh` refuses a `WARDYN_VERSION` that is not a plain release tag.** A value containing `/` or `..` was interpolated into the compose, CLI-asset and `SHA256SUMS` URLs and could redirect all three to another GitHub owner, defeating the installer's same-origin checksum check. Legitimate tags (`v0.7.0`, `v0.7.0-rc1`) are unaffected. (R5 F183)
- **`make setup`'s pull-first path verifies what it claims.** It now runs `cosign verify` and `cosign verify-attestation` itself when `cosign` is on `PATH`, refuses an image that fails and builds from source instead, and says plainly that nothing was checked when `cosign` is absent — it previously announced "cosign-signed, SBOM-attested" after a plain `docker pull` by tag. (R5 F005, F100, F151)
- **The in-network probe container `make setup` runs is digest-pinned and `--pull=never`.** It was `curlimages/curl:latest`, re-resolved on every `up` and run on the control-plane bridge; `scripts/check-image-pins.sh` now covers the `make setup` shell path. (R5 F006, F010, F152)
- **`scripts/ci-run.sh` no longer passes the admin bearer on the host `docker` command line**, where `ps` exposed it to every user on a shared runner. The token still reaches the CLI through the wardynd container's own environment; no configuration change is needed. (R5 F199)
- **`golang.org/x/crypto` 0.55.0 → 0.56.0 (GO-2026-6354, GO-2026-6355).** Two
  denial-of-service advisories in `golang.org/x/crypto/ssh`, both reachable from
  the SSH gateway: `govulncheck` traces each to `handleSSHConn`'s own
  `ssh.NewServerConn` call. A malicious peer deadlocks the whole connection —
  6354 by flooding the incoming requests of a channel that is registered but not
  yet established, 6355 with crafted messages after establishment — and the
  gateway clears its handshake deadline once the session is up, so a deadlocked
  connection has no timer left to reap it. Fixed upstream in 0.56.0; no other
  dependency moves.
- **Two user drives can no longer name one storage object.** `user_drives.name` is UNIQUE, but the name that actually addresses storage is the DNS-1123 fold of it (`wardyn-drive-<drive-slug>-<home>`), so "Corp NAS" and "corp nas" — or "Corp NAS (eng)" and "corp-nas-eng" — were two rows minting one volume or one claim, handing one directory to two sets of members with different size ceilings, writability and reclaim policies. It was caught only at mount time, by the runner's `wardyn.drive` label check, as somebody's run failing. Registering or renaming a drive onto another drive's fold is now refused 409 at the write boundary (`0061_user_drives_name_slug_unique` adds the column the store writes and the partial unique index over it; `host_path` is excluded, because a share's object name is `<host_root>/<home>` and carries no slug). **An upgrade fails if the install already holds such a pair** — that pair is the defect, and the remedy is to rename one drive: `SELECT name_slug, array_agg(name) FROM user_drives WHERE backend <> 'host_path' AND name_slug <> '' GROUP BY 1 HAVING count(*) > 1;`. (R1 F284)

### Added

- **Helm: `image.digest`.** `deploy/helm/wardyn/values.yaml` gains `image.digest` (default `""`, byte-identical render when empty). Set it to `sha256:<64 hex>` and the Deployment pulls `<repository>@<digest>`; a non-empty `image.tag` is kept alongside as `repo:tag@sha256:…` rather than dropped; a digest not beginning with `sha256:` is refused at render. Until now the blessed Kubernetes path could only float on a mutable tag. (R5 F195)
- An admin can fence **which agents and which model providers** a member may name on their own run, as two more permission kinds on the existing Permissions page. Both narrow: until one is enforced members keep the powers they had, and a deny bites even before enforcement. A model-provider permission bounds only what the member chose — the provider a workspace is pinned to and the site-wide default still reach every run.
- A governance profile can cap **how many runs one person has going at once**, and self-service secrets gain the per-owner cap the sibling surfaces already had. Both answer with a quota refusal rather than a denial, because the caller is authorized and simply at a limit; an overwrite at the secret cap still rotates a key.
- The **who** fields suggest from your directory. Picking a group fills the object GUID Wardyn actually matches against while showing the name, and says so. A deployment without the connector sees a plain text field — no banner, nothing disabled.
- A corporate proxy can be told **which destinations to skip** (`upstream_proxy_no_proxy`), so a private endpoint is dialled directly instead of through a proxy that cannot route it. It is a routing decision only: a skipped destination still faces the address guard and the run's policy, which is why reaching a private endpoint also needs an internal-host declaration.
- **User drives**: an admin registers persistent storage — a share the platform already mounts, or a volume Wardyn creates per person — and allocates it to people, groups or everyone, with per-person size, mode and directory-name overrides. A member mounts theirs per run at `/home/agent/drive`, **read-only unless allowed**, and a run request carries only a flag: the server resolves which drive belongs to the signed-in caller and derives their own directory from their own identity, so nobody can name someone else's. A governance profile can deny the door outright. Wardyn says plainly where the size is enforced: on Kubernetes it is the volume request and the storage class decides whether it binds, on Docker a managed drive has no byte cap at all, a share is bounded by its own quota — **the size you see is the allocation, not a guarantee**. Registering a drive on a host path is fenced by an operator-set allowlist (`WARDYN_USER_DRIVE_HOST_ROOTS`) that is unset, and therefore closed, by default.
- **Governance profiles**: a named policy ceiling an admin can ASSIGN — to a person, to an SSO group, or to everyone — so a contractor group and a platform team can hold genuinely different limits on one install. Precedence is user over group over all, with a subject claim beating an email and priority then name breaking ties, so the answer never depends on the query plan. A profile REPLACES the site-wide default rather than composing with it, which is the only shape where reading a profile tells you what it permits; with no assignment, every resolution is exactly what it was before. A profile can only ever NARROW credential eligibility, and that bound is re-applied when the ceiling resolves, not just when it is saved, so a redeployed default that drops a pairing cannot leave a stale profile serving it. Denies are re-asserted inside dispatch, after the phases that add corporate hosts and credential injections — including the brokered git and PAT lanes, which never consulted the deny list before.
- **A security-admin role**, and a console that can be delegated to it. A security admin governs the verdict — profiles, permissions, egress decisions, token inventory, audit verification — and deliberately does NOT reach into a run: it is never stamped on an SSH key or an attach ticket, and no capability grant can widen it. That separation is what makes the surface safe to hand out. Forty gated routes are classified in an exhaustive table that a test walks from both tiers.
- Bedrock can be reached through a **VPC (PrivateLink) endpoint**: `WARDYN_BEDROCK_BASE_URL` points the data plane at a private endpoint, full model ARNs — including the `application-inference-profile` form — are documented as accepted model identifiers, and a private-endpoint hostname is now recognised as model traffic so the audit trail classifies the call an auditor will ask about. The endpoint is a boot flag rather than a runtime setting because in bearer mode it is the TLS-interception and credential-injection target.
- A fresh install now **remembers being set up server-side**: finishing Getting Started records completion on the install itself (`POST /setup/onboarding-complete`, idempotent and audited), so a different browser — or a different admin — lands past the funnel too. Until then, every console access force-lands an admin in Getting Started (once per page load; the funnel's own affordances can still leave), while members are never gated. The old per-browser flag survives only as a fallback for older daemons.
- The demo-video catalog groups by **deployment path**: core "Start here" episodes lead, the install's own path (single-user vs multi-user, read off the live install) follows, path-agnostic running-work episodes next, and the other deployment's path folds behind a disclosure. Member-audience episodes in the multi-user group carry a "For your members" chip, and the member Getting Started rail leads with "Your path".
- Egress demo **"Denied, however you spell it"**: allow-all plus one `denied_domains` entry, then the trailing-dot spelling of the blocked host meeting the identical 403 — the deny-list dodge the proxy's host canonicalization closes — with the refusal's machine-readable reason headers (`X-Wardyn-Egress`, `-Reason`, `-Host`) on camera for the first time. Held-at-the-door's missed-window step now names the `approval-pending` value that distinguishes "wait, then retry" from a hard no.
- `deploy/kind/sso/`: a documented overlay that flips the kind quickstart to a **multi-user (SSO) install** — demo-grade Dex with two static users, the chart re-rendered on OIDC, the split-horizon port-forward, and a default policy floored to the cluster's own barrier (without it, the baked default's CC2 floor refuses every member run on a Fence-only cluster — the chart's documented confinement-floor trap, now with a worked escape).
- `wardyn ssh-key ensure|list`, `wardyn run wait-ready <id> --json` and `wardyn ssh <id> --json`: scripted key registration, readiness (RUNNING plus an inspectable workspace) and target discovery so an external IDE or agent tool can drive a sandbox over the SSH gateway (`docs/SSH.md` §6). SDK: `ListSSHKeys`, `AddSSHKey`, `RunFiles`.
- Policy `git_push_any_branch`: a per-run opt-out of push branch-namespace confinement for a sandbox a human drives from an external tool; every such push is audited as `brokered:git:branch-ns-off`; clamped away from members' inline policies. Example: `examples/policies/remote-workspace.yaml`.
- `docs/design/CONSOLE-RULES.md`: the console's design rulebook (color budget, four body type rungs, three elevation levels, the run status vocabulary and glyph pairing, in-flight feedback timing, the screen review rubric).
- Console: error boundaries with `resetKey` isolate each cockpit widget per run; `useDeferredBusy` at remote-submit sites (disable at once, spinner only if the call lingers); `ErrorState` takes an action.
- Console `tool_rules` surface: a per-tool allow / hold / deny editor in the policy panel (it refuses what the API would refuse, in the same order, before the round trip), a "What this run can do" line on the New run rail, and audit rows — on the Audit screen and the run page's Audit tab — that read **Decided by rule** with the verbatim `rule_source` for a tool call the policy answered without a human.
- Runs board: two-row run cards with one shared attention rule — a held approval now counts, joined from the pending-approvals feed, so the board and the sidebar's amber badge can no longer disagree about the same run; a pinned **Needs you** lane above the title groups; a loading skeleton shaped like the card it stands in for.
- Sidebar: members get **Workspaces**; **Recordings** returns for admins (it was route-only, so the evidence trail was undiscoverable). The UI-sandboxes off-state names `docs/UI-SANDBOXES.md` beside the need.
- Run page: a run that ended badly says **What happened** and **What to do**, derived from its audit trail (image pull, selftest, kill, auto-stop); an ending the trail does not explain shows the state and the audit link only — no invented cause.
- Setup: each corp-network probe verdict (`timed_out`, `not_run`, an exec that never started) gets its own heading, note and tone instead of sharing one.
- Cockpit: the command bar carries the board's who + what status pair; the approvals strip caps at two rows and ranks held above passive; focus-mode shortcuts are labelled key chips spelled per platform.
- New run: field rhythm and the rail from the New run mock, Enter submits and Esc leaves an untouched form, preflight results unframed.
- Install instructions are organised by three audiences — running Wardyn for yourself, running it for a team, and joining a Wardyn someone else operates — with a new [`docs/MEMBERS.md`](docs/MEMBERS.md) for that last audience.
- Getting Started can play the demo episodes in place: **Watch** streams the pinned release asset only after the click (no autoplay, no prefetch), an episode not yet recorded shows a plain "Not recorded yet" instead of a dead link, and a failed load offers the release page as a fallback.
- An operator can trust a corporate TLS-inspecting proxy's root CA (`WARDYN_TRUSTED_CA_FILE`): the daemon, the proxy sidecar and every sandbox — including the published base image's exec-mode runs and the setup connectivity probe — pick it up, delivered on compose, the desktop profile and the Helm chart.
- The admin's Getting Started gains a **People** step naming single-user vs multi-user access; a member signing into someone else's Wardyn gets their own Getting Started page — what's already set up for them, adding a workspace, bringing their own model key, their first run, approvals they can decide, and connecting their tools — and the console's first landing waits until it knows the signed-in role before choosing a screen.
- The People step becomes an **acting surface**, not just an explainer: an admin adds, edits and deletes `WARDYN_OIDC_ROLE_MAP` role mappings live from the console (`GET /access`, `POST /access/mappings`, `DELETE /access/mappings/{id}`, `POST /access/preview`), with a merged chart+console table, shadowed-row badges when a later chart change or allowlist entry collides with a saved row, and a sign-in preview. Two guards protect the write: a posture-flip acknowledgement when a change would move what an unmatched, non-allowlisted human gets, and a lockout refusal when a write would remove the acting admin's own access — checked against their last sign-in, with a distinct refusal when that snapshot is itself too stale to answer the question either way. An email-shaped console mapping is refused by default (`WARDYN_OIDC_ALLOW_EMAIL_MAPPINGS` opts in); `deploy/azure-entra-sso/` adds a scripted, worked validation of the whole path against a real, free-tier Entra tenant. The cockpit's inline egress approvals stop blanket-disabling a member's own-run decisions, matching the server's existing own-run rule (F-12).
- Audit rows that no tool rule decided also show which rule decided them — allowed by policy, released by an approval, refused by a built-in guard, brokered, or a declared internal host — beside the row itself, not just through the raw audit API.
- Wardyn reports whether the Kubernetes NetworkPolicy that isolates sandboxes is actually enforced (`/healthz`'s `network_policy` field, plus a boot-time audit event on an unenforced-but-allowed cluster), closing a gap where sandboxes ran unconfined with no visible signal.
- An operator can declare internal hostnames — an in-cluster service, a corporate registry — that are allowed to resolve to private/CGNAT addresses, so an internal service is reachable by name instead of only by a literal IP (which breaks TLS and routing).
- An operator can point the API-key model-access lane at an internal gateway instead of the public provider host (`WARDYN_ANTHROPIC_BASE_URL` / `WARDYN_OPENAI_BASE_URL`); subscription and Wardyn-managed runs still reach the public provider directly.
- A member can bring their own model API key: it works in their own runs with no admin setup and is never reachable from anyone else's run. Members can set and remove their own secrets (`GET /secrets` now also returns `mine`); AWS/Bedrock credential names stay admin-only, and an admin can still manage a member's secrets via `?owner=`.

- **A browser desktop (noVNC) is a shipped image variant.**
  `deploy/images/novnc/`, `make agent-image-novnc`, declared as
  `"name": "novnc"`. It changed **no server code**, which is exactly what the
  roadmap predicted an app-on-the-relay would cost.

  It is `FROM agent-base`, **not** the `agent-claude-code` the code-server image
  uses: agent-base is the contract with no vendor CLI and is what this project
  actually publishes, and an X stack layered on node + npm + a vendor CLI is
  surface for nothing. Measured rather than estimated — **~565 MB** over the
  base (code-server costs ~228 MiB; an X stack is simply expensive), listening
  **~1s** into the 20s readiness budget. Local build only, like `agent-vscode`:
  publishing an X stack would drag in the trivy matrix, a per-image SBOM and a
  GPL source offer for a whole desktop.

  Three ceilings a desktop raises above an editor's are stated in
  `docs/UI-SANDBOXES.md` — nothing inside a relayed app is recorded, its
  JavaScript runs in the operator's unconfined browser, and the relay cookie
  never re-checks the principal. None is new; a desktop makes each bigger.
- **The BYOI-wrap and UI-sandbox e2e lanes now run nightly, and a failure opens
  an issue.** `make test-e2e-byoi` and `make test-e2e-ui-sandbox` ran in **no
  workflow at all** — and the BYOI lane is the one that reproduces the
  `desktop-envelope` failure fixed below, so nothing scheduled would have caught that
  regression returning. Nothing in `.github/workflows/` previously used
  `if: failure()`, opened an issue, or notified anywhere, and no nightly job is
  a branch-protection context: a red nightly blocked nothing and told nobody.

  The notification is **scoped to the two new jobs**, deliberately. The flagship
  `e2e-live` job carries pre-existing, never-re-characterised failures; wiring
  an alert to the whole workflow would fire every night, and an alert that
  always fires is filtered by week two — the same end state as no alert, reached
  expensively. The lanes still wired nowhere (`test-e2e-ssh-k8s`,
  `test-e2e-subscription`, `test-e2e-concurrent`) are now named in the workflow
  with the reason, so "it runs nightly" is not read as "everything does".
- **A blocked egress request now says WHICH rule blocked it.** Eight distinct
  outcomes collapsed into one `X-Wardyn-Egress: denied`, and they call for
  completely different actions — ask the operator to allowlist a host, versus
  stop retrying because a human already refused, versus fix a broken policy.
  `X-Wardyn-Egress-Reason` carries the decision log's own rule source
  (`policy:default-deny`, `policy:denied`, `approval:denied`,
  `builtin:private-ip`, …). Same static strings the audit trail already records,
  so nothing new is disclosed. Documented in `docs/UI-SANDBOXES.md`, which is
  where developers most often meet the egress policy.
- **`examples/policies/ui-sandbox.json`** — the first shipped example declaring
  a `ui_apps` block. `grep -rl ui_apps examples/policies/` previously returned
  nothing, so the one policy field the UI-sandbox feature turns on had no worked
  example anywhere.
- **An SSH channel refused by the per-run cap is now audited**
  (`ssh.channel_rejected`, naming the channel type). A refusal used to be
  invisible to the deployment: the client saw `ResourceShortage` and nothing was
  recorded. That mattered less while the gateway was off by default — the
  desktop envelope now ships it **on**.
- **Git PATs for non-GitHub forges are never resident.** The two git lanes had
  opposite credential postures: a `github_token` was minted proxy-side and
  injected on the outbound leg — never in the sandbox, per-repo, ref-confined,
  ≤1h — while a `git_pat` for GitLab or Azure DevOps was handed to the
  **in-sandbox** credential helper and sat in the agent's process for the life of
  the run, at whatever scope the operator's PAT carried. The asymmetry was the
  gap.

  `agent-run` now rewrites a granted host to a plain-HTTP broker path, so the
  proxy terminates the request, mints server-side and injects Basic auth itself —
  removing the opaque `CONNECT` tunnel rather than trying to inject into one. The
  grant ids are **withheld from the sandbox env**, which is the half that
  matters: leaving them would let the in-sandbox helper mint the PAT anyway and
  the credential would be resident despite the broker.

  **It does not make the PAT least-privilege** — Wardyn cannot narrow a scope the
  operator issued, and there is no ADO/GitLab equivalent of a scoped installation
  token. The allowlist is per **host** for exactly that reason. The broker admits
  only the smart-HTTP surface the GitHub lane does; forwarding arbitrary paths
  would make it a credentialed proxy to the whole forge.

  `WARDYN_GIT_PAT_BROKER=off` restores the old lane. There is deliberately **no
  automatic fallback** — falling back would silently return the PAT to the
  sandbox.
- **`threatmodel/AGENT-THREAT-MODEL.md`** — a portable threat model for agent
  systems generally: terminology, fourteen threat categories, and who owns which
  control. The shipped `THREAT-MODEL.md` is excellent and is a threat model **of
  the Wardyn implementation** — its assets are our artifacts, its boundaries are
  named after our env vars, and one residual is about the coverage of a single Go
  test. It is structurally unusable by anyone not running Wardyn, and it has no
  glossary and no attack-category taxonomy at all.

  The new document travels; every category still carries a **Wardyn coverage
  verdict** citing code, because a taxonomy with no verdict is a whitepaper.
  Verdicts are exactly three values — `mitigated`, `partial` (must name the
  bypass class), `not addressed` (must say by-design or by-omission) — and the
  document carries **twice as many non-mitigated verdicts as mitigated ones**.
  That asymmetry is the reason to trust the rest.

  Two of them are gaps worth naming here: **denial of wallet** (there is no
  token-spend or model-call budget anywhere — an agent holding a valid model
  credential can exhaust it) and **approval fatigue** (the platform leans heavily
  on human approvals with no rate limit, no batching guard, no anomaly signal and
  no separation of duty).
- **`docs/PLUGGABILITY.md` answers the four-layer question directly** — physical
  sandbox / ingress-egress / LLM gateway / MCP tool gateway — with the honest
  score: **one of four is genuinely pluggable.** It also promotes the strongest
  pluggability claim in the codebase out of a matrix cell: `substrate.Substrate`
  has **two independently-built implementations held to one conformance
  contract**, both enforced in CI. And it states plainly that `wardyn-toolgate` is
  **not** an MCP gateway despite speaking MCP.
- **The desktop tier is packageable** as a `.deb`, an `.rpm` and a tarball. `scripts/build-desktop-package.sh` builds
  a `.deb` and a tarball, **from a clean git tree — never by copying the working
  directory**. `deploy/compose/.env` is a real file on a maintainer's box: 0600,
  gitignored, carrying a **live `WARDYN_AGE_KEY`**. Copying the compose dir
  copies it, packaging tools normalize modes, and the maintainer's age identity
  then lands on every managed laptop — making every device's secret store
  decryptable by anyone holding the package, against `docs/DESKTOP.md`'s
  "never via MDM". `git archive` cannot pick up an untracked file, so the
  guarantee is structural; a payload scan for a real age identity and for a
  literal `.env` backs it up.

  The `.rpm` builds inside a throwaway Fedora container (`--rpm`), so `rpmbuild`
  need not be installed on the maintainer's host and its version is pinned rather
  than inherited.

  Both packages declare a **real architecture**. They were first written
  `Architecture: all` / `BuildArch: noarch` while shipping a compiled Go binary —
  `rpmbuild` refuses that outright, and **dpkg does not**, which is the worse
  failure: an `all` `.deb` installs happily on arm64 and only then does the CLI
  fail to run.

  The payload keeps the `deploy/` level because `wardyn-desktop.sh` computes
  `REPO_ROOT` as `../..`, and it ships the **`wardyn` CLI**, which the packaged
  tier otherwise lacked (the one-line installer is fixed separately below).
- **Model access on the member-mode profile (m′) has a documented, working path that needs no member secret at all — it is Bedrock.** Three
  shipped mechanisms compose into what reads as a dead end (m′ mandates OIDC;
  OIDC refuses subscription injection;), and the
  daemon's own refusal message names the way out in a clause that is easy to
  skip: *"…or use Bedrock"*. Bedrock is **daemon-level, MDM-set** config rather
  than a per-member credential, so it routes around that wall entirely and needs
  no member secret write. It appeared **zero times** in `docs/DESKTOP.md` and in
  both envelopes; it is now documented in each, with the `claude-code`-only
  constraint stated.
- **Log rotation.** The LaunchDaemon appended stdout *and* stderr to one file
  every 300s forever with no `max-size` anywhere. Ships a `newsyslog` fragment
  (macOS) and a `logrotate` one (Linux, where journald otherwise handles it),
  keeping **seven** generations on purpose: the audit-drop counter surfaces only
  in that file on a laptop, so rotating aggressively would destroy the evidence
  that the SIEM fanout dropped events.
- **An operator can sweep leaked sandboxes on demand** —
  `POST /api/v1/admin/sandboxes/sweep`. Not a ticker (the sweep is an unpaged
  `ListRuns` plus a probe per terminal run, so it grows with history and would
  need leader election) and **not a second boot pass**: the existing reconciler
  already covers boot, and adding it there tears the same sandbox down twice.
  The gap it fills is a laptop that suspends for a week and never reboots.
- **The desktop tier installs on Linux.** `deploy/desktop/install.sh` hard-refused
  every non-Darwin host (*"the Linux/systemd path is not built yet"*), so a tier
  whose own topology diagram showed Linux had no Linux path. It now branches on
  `uname -s` and ships `wardyn.service` + `wardyn.timer` — `Type=oneshot` driven
  by the timer, mirroring launchd's `RunAtLoad` + `StartInterval 300`, logging to
  journald. The two platforms' intervals are asserted equal so they cannot drift.
- **There is an uninstaller.** `grep -rn uninstall deploy/` previously returned
  nothing on either platform. `install.sh --uninstall` stops the converge job and
  the stack and **keeps** `age.key` and the Postgres volume, so a re-install
  recovers the device; `--purge` destroys both, after saying exactly what becomes
  unrecoverable.
- **`WARDYN_DOCKER_SOCK` is honored from the envelope.** The converge job runs as
  root while Docker Desktop, Colima, rootless Docker and Podman all expose a
  **per-user** socket — and auto-detection shells `docker context inspect`, which
  as root reads *root's* contexts. CI never surfaces this (its daemon is
  root-reachable). The launcher now prefers an envelope value and, when nothing
  resolves, **refuses and prints what it tried** rather than converging against
  the wrong daemon. Documented in `docs/DESKTOP.md` "Which Docker socket", with
  the decision — system-scope unit — recorded.
- **The desktop tier can now be reached from your own terminal.** Both listener
  variables default to empty in the included stack, and **empty means off — no
  listener, not even a generated host key**. The desktop envelope and the
  one-line installer set neither, while both published `127.0.0.1:2222`. So the
  tier shipped a **port that refused every connection**, on the release whose
  stated outcome is that a developer reaches a governed sandbox from their own
  tools. `WARDYN_SSH_LISTEN`/`_ADVERTISE` are now set in both, with the
  recording ceiling stated beside them.
- **The one-line installer installs the `wardyn` CLI.** It previously installed
  **no host binary at all** — the only command path was `docker compose exec`,
  which is in-container and root-only — so `wardyn ssh <run-id>` had no client
  on the very machine that enables the gateway. The binary is fetched per
  os/arch and **verified against the release's cosign-signed `SHA256SUMS`**; a
  mismatch is fatal, and an unavailable `SHA256SUMS` skips the CLI rather than
  installing it unverified.
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

- **A MITM leaf is re-minted before it expires.** The per-host cache never consulted `NotAfter`, so past ~25h of sidecar uptime every new CONNECT to a MITM'd host was answered `200 Connection Established` and then failed the sandbox's TLS handshake, silently and permanently. (R3 F078)
- **Concurrent content inspection is bounded, and the proxy tells the Go GC about its cgroup cap.** The extractor expands a buffered body ~5.3x, so two concurrent in-cap bodies exceeded the proxy sidecar's 256 MiB ceiling and got it OOM-killed — taking the run's only network path with it. An over-budget request now waits and is still fully inspected, and the wait itself is bounded by the request context plus a wall-clock cap that fails closed — the agent-facing listener has no ReadTimeout, so one slow-loris POST used to park every other inspected request of the run. (R3 F074)
- A security admin can now read the workspaces it already governs. It could see every workspace in the list and could rewrite any of their allowed and denied hosts, but opening one — or reading the traffic that workspace had actually been observed making — answered "not found", so the tier was deciding a denylist without being able to look at the evidence for it or check its own work afterwards. Reading those four views now works for that tier. Nothing else moved: another member still cannot see someone else's workspace, and a security admin still cannot rename, reassign, delete or attach credentials to one.
- Demoting someone now reaches their API tokens. A token recorded its owner's role when it was minted and nothing could ever update it, so taking admin away from a person left every token they already held still acting as an admin until somebody remembered to revoke each one by hand. Signing in now refreshes the role on all of that person's tokens — the same thing their SSH keys have done since 0.6. The limit is stated plainly rather than implied: someone who is demoted and never signs in again keeps the old role on their tokens, so revoke when a change has to take effect immediately or when the owner has left.
- An admin reading another person's secret namespace is now recorded. Every *write* through the admin-only `?owner=` surface already stamped whose namespace it landed in; the *read* on that same surface recorded nothing, so an admin could enumerate someone else's secret names and leave nothing for a later investigation to find. The new row says plainly what it is — names were listed, and how many — because that route returns names only and never a secret's value. A member listing their own namespace, which the console does on every page load, is deliberately not recorded.
- Two authentication failures that were previously invisible to monitoring now have counters. Wardyn caps how many failed-sign-in audit rows it writes per second, so a password-guessing run against a deployment produced the same handful of rows as a few typos and looked *quieter* the harder it was pushed; the dropped rows are now counted, so the real rate is graphable and alertable. And a database error that makes every API-token request fail used to leave nothing behind at all — no audit row, no log line, and a store health gauge still reporting green, because that gauge only pings the database and a reachable database can still fail a query. It now logs at error level, increments its own counter, and the gauge says plainly what it does and does not cover.
- The group-completeness safety check no longer reads the entire permissions table on every request that triggers it. The check exists so a group DENY cannot quietly evaporate for a caller whose group list arrived incomplete — including every API token minted before 0.7, on every request it makes — but it answered by scanning the whole grant table once per value examined, so the cost of a permission decision grew with the size of the table and a single old token could force that scan repeatedly. It now asks for just the rows that could possibly match. The refusal itself is unchanged, and a test pins the new path against the old scan over a matrix of cases rather than only measuring that it got faster.
- **An approval-gated `git_pat` grant can clone again.** The PAT broker minted per sub-request with no cache or single-flight, so the second half of a clone 409'd `already_minted`, and a pending credential approval was a 502 instead of a wait; both lanes now share one credential lifecycle. (R3 F120)
- **`wait_for_review` holds for the operator's budget.** The hold deadline was armed after the concurrent-raise retry loop, so a hung control plane blocked a request for roughly six times the control-plane client timeout instead of `first_use_hold_seconds`. (R3 F070)
- **A brokered `git_pat` decision names the forge it reached**, not the control-plane host, so two granted forges are no longer the same row in the egress decision stream. (R3 F014)
- `wardyn site-config apply` accepts what `wardyn site-config get` emits again. Once an operator finished the Getting Started funnel the document carried `onboarding_completed_at`, and `PUT /site-config` refused any body containing it — so the documented disaster-recovery round-trip, the MDM-delivered `/etc/wardyn/site-config.json`, and every console save on the Corporate network screen (which builds its body by spreading the GET document) 400ed. The stored mark is now carried forward and the body's copy ignored unconditionally — including on a FRESH store and when the file names a different instant than the one this install holds (the MDM file re-applied after the laptop's own funnel, or a captured baseline applied after re-onboarding); the drop is reported as `onboarding_completed_at_ignored` in PUT's response and printed by `wardyn site-config apply`. (R3 F025)
- The redirect probe no longer reports a public host that ACCEPTED the sandbox's connection as "correctly blocked when dialed directly (redirect enforced)". curl returns the same timeout code for a dial that never left the sandbox and for one that connected and then stalled, so a tarpit, an accept-and-hold load balancer or a host merely slower than the probe's budget scored a wide-open network as enforced. The probe now reads curl's own connection count beside the status code and calls any completed connection a bypass. (R3 F148)
- `PUT /site-config` refuses an `upstream_proxy_url` the proxy sidecar's own loader would refuse. A port of `0` or `99999` saved with `200 OK` and then failed the sidecar's config validation at container start, which exits it — killing the egress path of every dispatched run. The write now delegates to the sidecar's own parser (as `upstream_proxy_no_proxy` already did), and a URL that reaches dispatch through the secret ref and fails the same check is dropped with an audited reason instead of delivered. (R3 F003, F028)
- An egress redirect's port is now read the same way by everyone that reads it. A query or fragment ends the authority, so `https://mirror:8443?repo=npm` is port 8443 and no longer silently 443 — which had mis-scoped the TLS-MITM/token-injection set (the operator's registry token withheld on the port the redirect names, presented on one it does not) and made the probe dial the wrong port. A `to` that spells `http://` with no port is port 80, not 443, so a working plain-http mirror is no longer reported as unreachable. And a port outside 1-65535 is refused at the write instead of being coerced to 443 by one reader and rejected outright by another. (R3 F033, F082, F091)
- The `timed_out` probe verdict names the real budget. Both `/site-config` probes formatted the wait as a raw nanosecond count, so the operator was told the run never reported completion "within 90000000000s" instead of within 90s. (R3 F149)
- The recording leak check now searches the reassembled session output, not just the stored bytes. A secret split across two PTY writes lands in two asciicast events, so the framed byte run is always broken and the headline assertion of the boundary-split masking test could never fail — proven by disabling the tail-retention fix and watching a cleartext credential pass that check. (R3 F147)
- The Postgres-gated concurrency proofs now run under the race detector. The only race job strips the database DSN, so every `WARDYN_TEST_PG`-gated test skipped there, and the job that sets the DSN ran without `-race` — leaving the broker's exactly-once credential-mint proofs, whose whole value is racing goroutines, unchecked by any gate. `make test-race-pg` is that gate, and CI's `test-pg` job runs it. (R3 F137)
- A run's approvals list is read from the database, one run at a time. `GET /api/v1/approvals?run_id=` — the shape the CLI and a run's detail page poll — installed no page window, so it loaded every approval row the deployment had ever written and filtered them in memory; decided rows are never deleted, so that read grew with the deployment's age. The run filter, the state filter and the page window now all run in one indexed query. (R3 F072)
- **Revoking a human by email now actually revokes them.** `wardyn sessions revoke --sub` advertised "the OIDC sub/email", but only the sub was ever matched — so on an identity provider where the two differ (Entra, whose `sub` is an opaque per-app identifier nobody reads off a screen), naming the email logged nobody out, revoked none of their API tokens, and still answered success to the responder and to the audit log. Both halves of a revoke now match either identity, the email case-insensitively, the way a per-user permission grant already did.
- Documented what a security admin's "revoke sessions and API tokens" actually reaches: a **super admin's** sessions and tokens too, and the revoke-all arm logs out every principal and permanently revokes every API token in the deployment, CI credentials included. That is the tier working as designed — incident response is its job and a revocation only ever takes reach away — and what bounds it is now stated as well: the session cutoff is a timestamp, so signing in again clears it, and the admin bearer break-glass ignores revocations entirely. Only the API tokens do not heal by themselves, which is why the deployment-wide arm is an incident lever rather than a routine one.
- A member is no longer silently promoted to full admin by a directory change. Entra stops sending the group (or App Role) claim once someone is in more groups than its token limit, and Wardyn read that absence as "this person is in no groups" — so on a deployment where unmatched people default to admin, the person the hidden claim was going to wall got the top tier instead, with no warning and nothing in the session to show it. Such a sign-in is now refused with a reason that says what to change, and the refusal is narrow: anyone whose claims actually matched signs in as before, and so does anyone defaulting to the ordinary member role.
- A group name your directory spells with a non-English character no longer makes a group permission or a governance profile silently do nothing. Such a name can never appear in the login-time group snapshot the two are matched against, so a deny written for it protected nothing and a group's profile quietly fell back to the deployment-wide ceiling — accepted, shown as active, enforcing nothing, with no refusal and no audit line. Writing one is now refused with the reason, and a sign-in that carries one is treated as an incomplete snapshot, which the deny and ceiling resolvers already fail closed on. A crafted claim can no longer imitate one of your ASCII group names either: the check that decides what a group name is now runs before the lowercasing that made a look-alike character collapse onto a real group, on the group snapshot and on the console's role-mapping writes alike.
- Launching a **recording session over someone else's workspace** is now visible in one audit query. Every other workspace-scoped admin write stamps the marker that says whose data was touched — including a sibling event in the same file — but the record launch did not, so an auditor filtering for cross-user admin access missed the single most privileged one: a session that mounts the owner's directory and injects their credentials. Recovering it meant joining the workspace back to its owner out of band, which stops working once that workspace is reassigned or deleted. Both the success and the failure event now carry it, and a launch refused because a stored source names a reserved path is recorded too rather than returning silently.
- The **never-resident `git_pat` lane now actually runs.** Three documents — the environment reference, the policy guide and this changelog — state that since 0.7 a non-GitHub forge's personal access token is minted proxy-side, never enters the sandbox, and has its grant ids withheld from the sandbox environment, with only `WARDYN_GIT_PAT_BROKER=off` restoring the older resident behaviour. None of that was happening: the switch resolved into a setting nothing read, and the lane was carried by an internal per-launch flag no launch path ever set, so every run took the pre-0.7 path where the token is minted inside the sandbox and lives in the agent's process for the run. A deployment that read the documentation and left the default alone believed it had the stronger posture and did not. The lane is now derived from the operator's own switch at the one place every launch passes through, so no launch path can omit it; the `off` escape hatch still works, and is tested in both directions.
- The boot-time heal that re-applies **permanent egress decisions** after a restart now asks the same question the live API asks, and records what it did. It replayed a verdict recorded months ago against the workspace as it is today, so an operator who marked a host *required* after an older permanent **deny** on that host had that deny silently re-written on every restart — breaking the workspace's own contract each time, with nothing in the audit trail to explain it, because the heal recorded nothing at all for the durable writes it made. It now skips a verdict the live path would refuse, and every write and every skip is audited under the same action as the operator-driven decision, distinguished by source.
- Approving an egress request whose recorded scope is **malformed in a second way** now says so. One shape of bad scope already produced an audit row explaining why the workspace requirement was not written; a neighbouring shape — valid JSON that is not an object — returned success and wrote nothing, with no explanation, which is the exact symptom the first fix existed to remove. Both now answer identically, naming which of the two it was.
- A listen address written as a **hostname** is now classified like the address it resolves to. Two boot refusals ask whether the bind exposes a specific non-loopback interface — the one that stops `-local-trust-forwarder` disabling the loopback-peer check on a LAN-reachable no-auth surface, and the one that stops serving cookies in cleartext to LAN peers — and both answered "cannot classify, do not refuse" for any hostname. Naming the interface instead of numbering it skipped them. A name that resolves to a LAN address now refuses exactly as its literal does; one that resolves only to loopback stays quiet, and one that does not resolve stays quiet too, since the bind itself will fail moments later with a better message and a boot guard that cries wolf is one nobody reads.
- Turning on the four-eyes egress switch in local mode now **says so at boot**. It cannot be enforced there — local mode authenticates nobody — so every egress decision is refused; the warning names that consequence and the remedy instead of leaving it to be discovered when the first approval hangs. It fires only for that combination: the switch alone works normally, and local mode alone is unaffected.
- One run request now resolves the caller's governance ceiling **once**. A single member launch read the assignment tables three separate times — the shape gate, the policy resolution, and the credential-grant filter each asked independently — with dispatch making a fourth, and nothing tying them together. Two comments in the code asserted opposite things about whether that mattered. It matters: an admin narrowing a profile mid-flight (the incident-response action) could be raced by a launch already in progress, landing a run whose egress was bounded by the old ceiling and whose credentials were filtered by the new one. Every site now shares one answer for the life of the request, and nothing is cached beyond it, so revoking a profile still takes effect on the very next request.
- A refusal a member can actually act on now **says how**. When someone's group membership cannot be established, the launch and secrets endpoints correctly refuse — but two of them printed only the internal reason code, with none of the "sign in again so your ceiling can be resolved" guidance every other endpoint gives for the same refusal, and the launch endpoint prefixed it with a claim that the caller's policy was invalid, which it was not. One symptom was `wardyn secret list` printing a bare internal word a member had no way to act on. The guidance now travels with the error itself, so any endpoint that reports it carries the remedy.
- A workspace whose stored source targets `/home/agent/drive` is now refused on **both** run paths, not just one. That path and everything under it is reserved for the per-person user drive, and an ordinary run already refused it — but the record/verify path re-checked only its scratch-directory sources, so a directory source aimed there became a real bind mount at the reserved path, and a repo source aimed there was dropped so late and so quietly that the session simply started with a repo that never cloned. Nothing downstream caught either: the composed recording policy skips the policy validator, and the container driver's own check does not carry the reservation. Both paths now refuse the same workspace with the same wording and the same status. The reservation is also stated in the policy reference, whose `target` rows described every other rule but this one — so a target copied from the docs could be rejected on write.
- Corrected what the docs and the router say a **security admin** can do to other people's runs. Both the router's own note on the sandbox sweep and the route-classification table justified keeping that sweep admin-only as protecting "the one axis the security tier never gets" — tearing down runs the caller does not own. Neither half was true: the sweep skips every run that has not already ended (it reaps containers that outlived a finished run, never a live one), and the security tier can already stop any run in the deployment, deliberately, because killing a foreign run is incident response and is the tier's most time-critical act. An operator deciding who to trust with the role was reading the opposite of the truth in the two places that look authoritative. The sweep stays admin-only on the reason that actually holds — it drives the container runtime across every run at once, which is host reach and fleet-wide blast radius — and the operations guide now states plainly that the tier can stop a run but never reach into one. No behaviour changed; the ability to stop a foreign run was deliberate and stays.
- The approvals queue's member scoping is now tested. A member's `GET /approvals` is narrowed to approvals on runs they created, and a backend that cannot answer that question refuses rather than serving the whole fleet's queue — both were correct in the code and asserted nowhere: removing the scoping entirely left the whole API test suite green. The database query the narrowing rests on (approvals carry no creator of their own, so it is a join onto the runs table) had no test in either lane. Both are pinned now, in both directions.
- **Members no longer read the operator's estate.** Four read endpoints — the site config, the source library (list and detail) and the base-image catalog — were readable by any signed-in member while every corresponding write was admin-only, a split made by verb rather than by what the document holds. Between them they returned the upstream-proxy secret reference and every integration's credential reference by name, the internal proxy, SCM, artifact and registry hostnames, the **host filesystem path** of every local-dir source, the names of the secrets and internal hosts each library entry requires, and the bootstrap URLs a base image is built from — a lateral-movement target list, handed over on a console page load. No secret values were exposed; they are write-only. All four now sit on the admin tier beside their own writes rather than being individually redacted, because nothing member-facing consumed them: the console has no client for the source or base-image endpoints at all, and its two site-config callers already tolerate not getting one. The one place a member would have noticed is the Settings page's corporate-proxy line, which would otherwise have told them sandboxes "go direct" on a proxied deployment — a false statement rather than a redaction — so that posture row is now admin-only too.
- One member request no longer decides how much database work — or, since the capability batch was indexed by kind, how much CPU — the control plane does. Narrowing a member's own inline policy resolved that member's permission grants **once per host in their `allowed_domains` list** — two round trips each, three when their group snapshot cannot be answered — and nothing on that path caps or de-duplicates the list, which comes straight from the request body. Measured against a real Postgres: about half a millisecond per entry, so a single request carrying the most entries that fit in the 1 MiB body limit made 104,850 sequential queries and held a request handler for 27 seconds (157,275 and 45 seconds on an unanswerable snapshot) — repeatable for free through the preflight endpoint, which persists nothing, against a connection pool of a handful of connections. The grants and the enforcement settings are now read **once per request** and every host matched in memory: 2 round trips, 3 when stale, no matter how long the list. Same answers, including the case where an unanswerable group snapshot must override a grant the caller does hold. Nothing is cached between requests, so revoking a permission still takes effect on the very next one.
- The **four-eyes egress switch** (`WARDYN_EGRESS_SECOND_HUMAN`) now refuses local mode outright instead of appearing to hold there. Local mode authenticates nobody, so both halves of "the decider is not the creator" came from the same client-supplied source: the dev-only principal header the mode honours by design set the decider, and a run's `created_by` is written from that same source, so the run's own author could satisfy the gate either by adding one header when deciding or by creating the run under a name and then deciding it with no header at all — and the approval recorded the invented name as who decided it. Comparing the injected local operator instead of the header would have closed only the first of those. There is no second identity in that mode to compare against, so the switch is answered with a `503` naming the incompatibility and pointing at SSO, scoped to egress decisions so nothing else in local mode changes.
- Launching a **recording session** is admin-only again. It sat with the workspace egress-decision routes on the security-admin tier, on the reading that recording is how the hosts an admin later promotes get observed — but recording does not decide anything, it starts an interactive sandbox with open egress by default, the workspace's directory bind-mounted (writable if its owner allowed that), the repo clone credential minted, the workspace's required secrets injected proxy-side and the operator's model credential attached, and it stamped the launcher as the run's owner. That ownership stamp then satisfied the guard written to stop exactly this: a security admin is refused an interactive shell in someone else's sandbox, but not in one they launched themselves — so the tier could obtain a terminal, with credentials and unrestricted egress, over any member's files, including a workspace the same tier is answered "not found" for on a plain read. Promoting recorded egress into the allowlist — the actual decision, which launches nothing — stays with security admins, and the console now shows them that control live beside a disabled Record.
- A run's credential grant is now bounded by the ceiling entry that names **its own** pairing. The runtime clamp indexed the operator's eligible grants by KIND alone and kept the last one it saw, so a ceiling listing two `ssh_key` grants — the normal shape for two forges, and equally normal for `api_key` and `git_pat` — bounded a run naming the *strict* forge's host+secret by the *permissive* forge's `requires_approval` and `ttl_seconds`. A stripped `requires_approval` auto-mints the credential at proxy startup with no human in the loop, and the same ceiling produced different answers depending on the order its entries happened to be written in. Selection is now by pairing, order-independent, and shared with the check that decides whether a governance profile's grants are within the deployment ceiling — previously two implementations of one rule, which disagreed on exactly this axis. A grant whose pairing no entry names is bounded by the strictest same-kind entry rather than an arbitrary one.
- The dispatch-time governance ceiling now binds on **every** door, not the two that opted in. It was carried by two optional fields on the internal dispatch parameters, and the re-assertion phase returns immediately when they are empty — so a launch path that simply did not set them ran with no profile enforcement at all and wrote no `run.ceiling.reassert` row to say so. Three of the five paths did not set them, and two of those are reachable by a principal a profile binds: a member scanning a workspace source they own got a sandbox holding brokered clone and SSH credentials for the very host their profile denies, at the deployment's confinement floor rather than their own; a security admin's site-config probe ran its grants and injections unbounded. The ceiling is now a required argument of dispatch that a lane cannot omit, with no "exempt" value to claim by mistake — a run handed an unresolved ceiling fails closed instead of launching — so a path added later inherits enforcement rather than inheriting the gap.
- `env_secret`'s **admin-only** posture now binds on every path a member's run gets a policy, not only where a governance profile applies. The drop lived inside the member grant filter, which the stored-policy and deployment-default paths reached only for a member an admin had assigned a profile to — so on the DEFAULT posture (no assignment, and every pre-0.7 install upgrading into 0.7) a member selecting a stored policy that carried an `env_secret` grant had the operator's raw secret value written into their sandbox environment for the whole run, unaudited. The rule is a role check plus `WARDYN_ALLOW_MEMBER_ENV_SECRET`, never a ceiling check, and it is now applied as one: every non-operator, every route (inline body, selected row, deployment default), with the drop audited as `authz.denied` like the member filter's own. Operators are unaffected; a deployment that deliberately serves `env_secret` to members opens `WARDYN_ALLOW_MEMBER_ENV_SECRET` as documented.
- The audit-chain verification sweep is walked in pages over the primary key instead of one unbounded query. Whether that query streamed or buffered the whole table was left to the planner, and it was observed choosing to buffer - on the endpoint an operator reaches for during a suspected tamper incident, over a table the append-only triggers make unprunable, so its cost and memory only ever rose. A sweep now holds one page at a time and stops between pages when the caller goes away. Concurrent requests are refused with **429** and a `Retry-After` rather than multiplied into several full re-hash passes; the verdict itself is unchanged, and is pinned against the previous full-scan implementation row for row.
- An audit write no longer waits forever for the audit-chain lock. Since the chain trigger began taking that lock on every insert into `audit_events`, one transaction that inserted an audit row and stayed open - an operator's psql session is enough - stalled every audit-emitting request indefinitely, each holding a database connection until the pool ran dry and unrelated queries blocked behind it. A request now gives up after five seconds and the event goes to the local spool to be replayed once the lock clears; a credential mint, whose audit row shares its transaction, is refused instead, because a credential that could not be audited is not one to issue. `docs/OPERATIONS.md` now also asks operators to set `idle_in_transaction_session_timeout`, which ends the stray transaction that causes this.
- An audit event written to the local fallback spool after a **torn write** could be destroyed instead of preserved. The spool separates a partial line from the event that follows it so the good event survives on its own line — but the probe that detects a torn tail needs to read the file, and after the first time the spool was compacted it was reopened write-only, so the probe silently stopped answering. From then on a good event appended behind a fragment was read back as one unparseable line and dropped, which is the one outcome the fallback exists to prevent.
- Recovering a spooled audit backlog no longer costs quadratically more than the backlog. Each drain pass rewrote and fsynced every remaining line while replaying a bounded batch, so clearing 64,000 spooled events wrote about 160 times their own size — onto the same volume as the database that had just come back. A pass now retires its work by advancing an offset and compacts only when the reclaim pays for itself, so a full drain writes about the backlog once. Mid-drain the spool file can hold lines already replayed; `wardyn_audit_spool_lines` remains the backlog.
- `/metrics` no longer disappears during an audit-store outage. The spool gauges were read under the same lock the drain holds across every store call, so a store that never answered — an external session holding the audit-chain lock is enough — blocked a scrape past its timeout and lost the entire response, `wardyn_store_up` included, once per tick for the duration of the outage the gauges exist to report.
- `wardyn_audit_spool_quarantined_total` survives a restart. The quarantine sidecar it reports is on disk, but the counter was per-process, so any deploy or crash loop reset the alert to zero while the events were still missing from the queryable trail.
- A member could name a base image they were not granted, by putting it on a workspace they own and launching against it — the image was copied onto the run after the check had already run. The check now runs again on the seeded value, scoped to member-owned workspaces so operator-authored ones behave exactly as before. Members also can no longer bind a model provider at workspace creation, which was admin-only on every other path.
- An egress redirect's target is trusted where the operator declared it. It had to ALSO be pasted into a policy's allowed domains or the run was refused, which nothing documented — so an operator following the mechanism the product pointed them at still got a denial.
- The redirect probe no longer fails a correct configuration. It dialled a literal address directly, presenting that address for TLS against a certificate scoped to the hostname; it now dials the target while presenting the original hostname, which is what a real run does.
- The AWS toolchain now trusts a corporate CA. AWS CLI v2 ships its own Python and its own certificate store and reads none of the four trust variables the sandbox already set, so on a TLS-inspecting network the one image built for the AWS CLI — and every intercepted Bedrock or STS call — failed certificate verification while the operating system's trust store was perfectly correct. The variable list had been written out twice, once per caller, and the two copies had already drifted; both now read one list.
- The `agent-aws-sso` image now ships AWS's `THIRD_PARTY_LICENSES` attribution
  file at `/usr/share/doc/aws-cli/THIRD_PARTY_LICENSES`. The AWS CLI installer
  copies only its `dist/` tree, so every previously published tag of this image
  conveyed the CLI's bundled third-party components without their attribution
  text; the build now preserves the file and fails closed if the installer zip
  stops carrying it.
- `GET /runs/{id}/files` reported `vcs:"none"` for every `--repo` run because it inspected the workspace mount target instead of the clone one level below it — the console's files widget claimed "no git repository" for a sandbox holding a full clone. It now finds the run's clone.
- Console surfaces that referenced a nonexistent `bg-surface-1` token painted no background (new-run rail, settings and connection cards).
- Focus rings now clear WCAG 1.4.11's 3:1 floor in both themes (`--ring` raised; the shared focus recipes no longer dilute it to 50%).
- **A wardynd restart could kill a healthy, just-started run on Kubernetes and
  report it FAILED.** Between the apiserver accepting the agent's ephemeral
  exec container and the kubelet publishing that container's first status, the
  pod carries the exec in `Spec.EphemeralContainers` with no matching entry in
  `Status.EphemeralContainerStatuses`. `AgentStatus` read that window as a
  **definitive terminal state with a nil error** — indistinguishable from "the
  exec is gone" — and both reconciler consumers finalize on exactly that pair,
  so a restart or a watcher-lease handoff landing in the window turned a live
  run into `FAILED` and tore its sandbox down. The docker driver already
  refuses the equivalent call (an exec-404 while the container still runs
  returns an ambiguity **error**, so the reconciler retries); k8s had strictly
  better evidence available — the pod `Get` succeeded and the exec is in
  `Spec` — and used it worse. Present-in-Spec-but-not-in-Status is now
  `STARTING`; an exec id absent from `Spec` was never exec'd against that pod
  and stays terminal.
- **`docs/PLUGGABILITY.md` claimed a selection convention that does not hold.**
  Its rule — *"every seam selects via a `WARDYN_<SEAM>` env var"* — is false for
  `egress.Evaluator`, which has a real interface and a conformance suite but **no
  registry, no selector, and exactly one implementation**; the only override is an
  in-process field nothing outside tests sets. `/healthz` was already honest about
  this (`policy_engine` carries no `available` list); the doc was not. The same
  claim was in `internal/component/registry.go`'s package doc. Both corrected, and
  the doc now states the distinction it exists to keep straight: an interface plus
  a conformance suite is a head start on pluggability, not a swappable seam.
- **The threat model's own citation rule had no gate, and had rotted again.**
  `threatmodel/THREAT-MODEL.md` §8 states that citations must name symbols, not
  line numbers — *"an earlier pass pinned line numbers and six of nine had rotted
  onto unrelated code (one past EOF)"* — and then kept nine of them, at least two
  of which had rotted by 0.7. All nine now cite symbols, and
  `TestCommentsCiteSymbolsNotLineNumbers`'s sibling extends the ban to
  `threatmodel/*.md`. Also fixes four `Tier-1`/`Tier-3` occurrences (the pre-`CC`
  names) and a security-doc contradiction: `docs/DATA-FLOW.md` called
  `wardyn-proxy` the **L1** egress gateway; every other document calls it L2.
- **The GPL corresponding-source offer covered the wrong images.** Its hardcoded
  list still named `agent-claude-code`, unpublished since 0.6.2, and omitted
  `agent-base`, which publishes in its place — so the loop errored on a ref that
  does not exist while the image that IS published **was never scanned and had
  no offer at all**. Publishing an image conveys its GPL/LGPL binaries, so that
  is a real obligation, and it failed silently: a missing image produces no
  output rather than an error. The same two errors were in `RELEASING.md`'s
  manual cosign verification loop and in `release.yml`'s own header.

  Regenerated against the published digests — and the first regeneration
  **deleted** the section covering `agent-claude-code` 0.5.0/0.6.0, whose
  copies had been conveyed and were still owed an offer. The generator now
  retains a historical-offer section anchored to the last conveyance date
  (0.6.1 was never published; a comment in `release.yml` claimed it was), and
  that package's later removal from GHCR starts the offer's three-year clock
  rather than ending it. Its stale default tag is gone — a default
  silently regenerates the offer for the wrong release — and a new guard fails
  when the offer's image list and `release.yml`'s publish matrix disagree in
  either direction.
- **`RELEASING.md`'s tag-gate job list was wrong in both directions.** It named
  `sbom-stub`, which was **deleted** along with `make sbom` — so a maintainer
  following it literally waited on a job that can never report — and it omitted
  **`notices`**, the copyleft / unreviewed-dependency gate, telling them to skip
  the one job that catches a GPL regression on a release that adds an X stack.
  Both corrected, and a new guard fails when `ci.yml` and that list disagree in
  either direction; this list had already drifted twice.
- **`tool_approvals=hold` on an interactive run was accepted and silently
  discarded.** Dispatch writes `WARDYN_TOOL_APPROVALS` only for non-interactive
  runs, so the caller got a 201 and none of the supervision they asked for. It
  is now a 400 naming the field. The run does not become unsupervised —
  interactive tool use is already supervised in the attach pane — so this
  refuses a contradiction rather than closing a hole. The guard sits **after**
  the empty-task→interactive coercion, because a guard placed before it passes
  and the field is still dropped.
- **The custom base-image `steps` surface pretended to do something.**
  Validation, caps and UI copy implied operator-authored Dockerfile lines would
  be applied; nothing applies them, and nothing may — operator `RUN` lines would
  execute on the **host** daemon during the wrap, outside every confinement
  tier. The user-facing surface is gone. **The field itself stays**, documented
  as catalog identity: it is part of the `base_images` UNIQUE index, so deleting
  it would make every upsert write NULL and mint duplicate catalog rows.
- **`env_get` killed its caller when a key was absent.** Its contract says
  *"("" when absent)"*, but `grep` exits 1 and every caller runs under
  `set -euo pipefail`, where `PIPEFAIL` propagates that — so
  `v="$(env_get "$f" KEY)"` for an absent key **terminated the script silently
  at the assignment**. It went unnoticed because the pre-existing call sites all
  used the value inside an `if`, which `set -e` exempts; 0.7's new
  `WARDYN_DOCKER_SOCK` lookup is the first plain assignment, and it stopped the
  desktop launcher dead with no output.
- **The desktop tier's image pin did not work at all.** `wardyn-desktop.sh`
  `export`ed `WARDYN_WARDYND_IMAGE` with a `:latest` default *before* running
  `compose --env-file`, and **compose prefers the shell environment over
  `--env-file`** — so the launcher's `:latest` always beat whatever digest the
  org shipped, on a 300s timer, while the org believed the fleet was pinned.
  `publish-image.yml` pushes `wardynd:latest` on every push to `main`, so that
  default had managed laptops tracking tip-of-main, unreleased, several times a
  day. The launcher now reads both pins **from the envelope**.
- **Every run's egress sidecar was unresolvable on a managed laptop, and
  nothing pulled it on either install path.** `deploy/desktop/` set
  `WARDYN_PROXY_IMAGE` nowhere, so the base compose file handed `wardynd`
  `wardyn/wardyn-proxy:local`. Worse, *nothing ever fetched the proxy image*:
  the `proxy-image` stanza sits in `profiles: ["build-only"]` so `compose pull`
  skips it, `--no-build` cannot build it, and the driver called
  `ensureImage(spec.Image)` only. The stack reached healthy, the console
  loaded, and the **first run failed at sandbox creation** — on the one-line
  install too, where the ref was pinned correctly and fetched by nobody. Both
  envelopes now pin it by digest, and the driver pulls it at the chokepoint that
  already fails closed. Verified both directions: with the fix the image is
  absent, gets pulled, and the run COMPLETEs; without it the run fails with
  `create proxy: No such image`.
- **`--pull always` on a 300s timer bricked an offline laptop.** Under
  `set -euo pipefail` an unreachable registry killed the launcher, so the stack
  did not come up **even though every image was already local**. Now
  `--pull missing`; with envelope pins there is nothing for `always` to catch.
- **There was no way to stop the stack.** `up` was the only subcommand while the
  daemon re-asserted every 300s — so uninstall, rollback, the offline lane and
  `wardynd -rotate-age-key` had no way to reach a stopped daemon.
  `wardyn-desktop.sh down` keeps all data; `down --purge` destroys the Postgres
  volume and says so first. It never removes `age.key`.
- **Site-config never applied on the member-mode profile, for a reason that was
  not true.** The apply was gated on `WARDYN_LOCAL_MODE=true`, skipping m′ with
  *"the SSO envelope variant has no CLI-usable credential here"*. It has one:
  `WARDYN_ADMIN_TOKEN` ships in `secret.env`, compose interpolates it into the
  container, `compose exec` inherits it, and it authenticates even with OIDC
  configured. The gate is deleted — no new flag, since the container already
  carries the variable.
- **Re-running the one-line installer at a new version ran the new topology on
  the old images.** It overwrote `docker-compose.yaml` at the new tag but its
  `if [ ! -f .env ]` guard skipped the entire `.env` write *including the image
  pins* — and told the user "Wardyn is running". The upgrade path now rewrites
  exactly the version-derived lines, adds listeners an older install lacks, and
  leaves the age key, admin token and ports untouched.
- **Only one of three agent names resolved on a published install.**
  `internal/api/harness.go` ships three catalog rows; `agent-claude-code` and
  `agent-none` both 404 on every published deployment, leaving `codex-cli` as
  the only `--agent` a user could actually pick. 0.6.2 stopped publishing
  `agent-claude-code` and publishes `agent-base` in its place — but **nothing
  selected `agent-base`**, and production callers pass the literal
  `"claude-code"` when what they want is simply the default general-purpose
  image (`source_scan.go`, `workspace_run_image.go`, `setup.go`; 0.6.6 fixed
  the site-config probe separately, by dispatching the `base` key outright).

  The `claude-code` row's `ImageKey` now points at `base`, which fixes the
  rest at once — an override in `WARDYN_AGENT_IMAGES` is still consulted first and
  is keyed by agent name, so an operator who pins the image is unaffected. The
  desktop envelope and the one-line installer both pointed at the retired ref
  too, and now name `agent-base`; `install.sh` seeded no `claude-code` entry at
  all.

  **The container-login lane deliberately does not follow the re-point:** a
  "Log in with Claude" sandbox must carry the vendor CLI it is logging into, and
  `agent-base` ships none, so that lane keeps a `loginImageKey` of
  `claude-code`. **Named gap:** that ref is unpublished, so on a published
  install the login lane needs a locally-built image named in
  `WARDYN_AGENT_IMAGES` — unchanged from before, not a regression.

  Two consequences that would otherwise have gone quiet: the unpublished-image
  warning now describes the symptom operators actually hit (the CLI missing from
  `PATH`, not a registry 404), and `agentImageCheck`'s limited-toolchain warning
  had silently downgraded to `info` for the default install — `agent-base`
  carries node/npm/python3 but no Go, Java or Rust, so the warn still applies and
  now fires.
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

### Changed

- **CLI: a mistyped subcommand under any `wardyn` command group now exits non-zero with an error on stderr** instead of exiting 0 with help on stdout (eleven groups; `Args: cobra.NoArgs` alone was inert because a parent with no `RunE` is never Runnable). A bare `wardyn <group>` still prints help and succeeds. (R6 F009) **A run policy with more than 256 `allowed_domains` entries is refused with a 400** (`allowed_domains: at most 256 entries`) — a new 4xx a member can hit, applied before the per-entry narrowing that walks the list. (R6 F061) **`auto_stop_after_sec: -1` is left alone** under a ceiling with no positive maximum — the old clamp rewrote it to 0 with a warning without changing the outcome (the reaper skips every value ≤ 0); the warning is gone. (R6 F062) **`disk_mib` on an overlay2 host still refuses to dispatch unless the backing filesystem is xfs**, and now says so before the daemon refuses — the code's claim that ext4 enforced the quota was false (owner: keep fail-closed). (R6 F064)
- **Setup checklist and Runs board copy now name commands that run.** The checklist's Helm "Fix" hints name the namespace, release and chart and re-pass the values file (a bare `helm upgrade --set` exits with a usage error and, when made runnable, resets every other value); the age-key hint steers the master key into the chart's Secret-backed options instead of a plaintext `env.WARDYN_AGE_KEY`; the Runs board's no-barrier banner offers `wardyn setup status` (no `sudo`) instead of a `wardyn setup fence` subcommand that never existed. Canon: docs/design/ui-batch3-mock.md. (R5 F236, F159, F190; R6 F001)
- **User drives, follow-ups (R7 review).** The design doc's table of server-composed refusals is re-derived from the code (two 400s were 422s, one string existed nowhere, three refusals were undocumented) and a test now checks every row against the Go literals; the drives screen's honesty sentence renders as one paragraph again; the PostgreSQL gate that decides whether group-tier allocations exist is tested in the false direction. No behaviour or string changed. (R7 F017 F089 F098)
- **Console, user drives (R7 review).** The drive editor cannot save on a deployment whose runner mounts no drive backend (it used to compose a POST naming a backend never shown, refused by the server every time); after a delete refused with 409 the list re-reads, so the next Delete opens with the true allocation count and its confirm disabled; the New Run read-only toggle renders only while the mount is on (it was a live control over a mount that never happened, rewriting the offer into a read-only promise); a member whose profile shuts the drive door sees neither the Getting Started drive chip nor its "mount it from New run" sentence; the allocation form sends `home_override` only when the field was touched, so an untouched repoint keeps the pinned directory name. (R7 F099 F100 F103 F104 + the F001 console half; F101 filed for a mock round)
- **Upgrading to 0.7 with user drives: rebuild the agent images** (`make agent-images-core`, then re-pin `WARDYN_AGENT_IMAGES` if you pin) — the 0.7 images pre-create `/home/agent/drive` owned by the agent user; on a pre-0.7 image an allocated drive appears as a root-owned directory the agent cannot write. BYOI images must do the same. 0.7 also refuses `/home/agent/drive` as an authored mount target: a pre-0.7 policy or workspace row that names it is dropped with a WARN at dispatch until you edit it (OPERATIONS.md Upgrades has the sweep). Docs now match the shipped code for the drives feature: managed vs share `fsGroup`, read-only binds under gVisor, the member-workspace ceiling vs drive isolation, offboarding order (preview first), the dispatch refusal strings (frozen as canon in the design doc §7.9), and preflight auditing. (R7 F023 F029 F031 F064 F065 + follow-ons; F016 F041 F063 were already correct)
- **User drives — control-plane side (R7 review).** A grant repoint that says nothing about `home_override` now keeps the pinned override and, if the row has one, answers **409** instead of silently clearing the column (`home_override` is optional on the wire: absent ≠ `""`; **compat:** a client that wants to clear must send `""`). Refused drive dispatches are recorded once, at one chokepoint, with a closed reason set and a new metric `wardyn_drive_refusals_total{reason}`. The share-bindability probe at run create is bounded (5 s, cancellable) so an unresponsive NFS server cannot hang the request. A stored policy target on the reserved drive path is dropped with a WARN at dispatch instead of failing the whole sandbox create. The drives preview consults the same unusable-groups arm as the resolver (403 on a truncated snapshot with group-tier grants). `host_roots_configured` in setup status is true only when a configured root is one a drive could actually bind. Host-root nesting is judged on resolved paths, not stored strings, and discloses the colliding drive. Drive integer fields are bounded to the column's range (a raw SQLSTATE 22003 500 is gone); the k8s home-segment rule rejects `.-`/`-.` labels; the workspace-collision warning at run create asks a bounded store query instead of listing every run. The share ownership rule and the reserved drive target are enforced on every composition seam. (R7 F001 F003 F008 F019 F021 F027 F040 F044 F050 F052 F060 F071 F081 F088 F092 F093; F002 F022 F026 F047 were fixed by earlier lanes)
- **User drives — runner side (R7 review).** A `k8s_pvc_static` share pod no longer carries a pod-level `fsGroup`: the upstream CSI NFS driver applies fsGroup regardless of fstype, so the kubelet would have re-owned a shared export to gid 1000 on the first run whose root did not match; only a managed `k8s_pvc` claim keeps `fsGroup: 1000` with `OnRootMismatch`. **Compat:** an admission policy or check asserting `fsGroup` on drive pods must scope itself to managed drives. A read-only `host_path` drive at CC2/CC3 now starts — wardynd asks the daemon for a recursively read-only bind only where the runtime declares the OCI `rro` mount option (runc does, gVisor does not) and WARNs when it drops the request; previously every such run failed at ContainerCreate. Drive dispatch refusals shown to a member name the drive and the directory, never a host path or another person's home; the paths move to wardynd's log (`wardyn: user drive: this share mount was refused at bind time`). **Compat:** a runbook that read the path from the member's failure hint must read the log. The drive editor's denied-prefix 400 uses the design doc's frozen `host_root` wording. wardynd WARNs at boot when `WARDYN_MEMBER_WORKSPACE_ROOTS` and `WARDYN_USER_DRIVE_HOST_ROOTS` name or contain one tree (`wardynd: mount ceilings overlap — …`) — a member could otherwise onboard the share as a workspace and bind every home; it does not refuse. The image guard that checks `/home/agent/drive` is created AND chowned now reads the order of the two, not just their presence. (R7 F072 F076 F069 F015 F106 F013 F082 F094)
- **A managed drive refuses every non-`hash` home template at resolve time as well as at validation.** One predicate serves both; a managed backend with a `sub`/`email_local` template that the validator already refused is now also refused (422) by the run-time resolver instead of deriving an object name from the sign-in subject. (R6 F060) **A workspace edit that clears the approved-egress list now stamps `egress_edited_at`**, so the boot-time heal no longer re-widens a list an operator deliberately emptied. (R6 F018) The capability-grant batch is indexed by kind and each distinct host resolved once, so a large `allowed_domains` list no longer multiplies grant-row comparisons — the CPU half of the growth law, the database half having landed earlier. (R6 F065)
- **Helm: `k8s.runsNamespace` must be set (new render refusal).** `k8s.enabled=true` with an empty `k8s.runsNamespace` is refused at render. The empty default put every run's pods, and the k8s-runner Role (`pods` create/delete/exec, `secrets` create/delete, `networkpolicies` create/delete), in the control-plane namespace, where those verbs applied to every other workload sharing it; RBAC cannot narrow it, so the namespace is the only lever. **Upgrading a k8s install that relied on the default:** set `k8s.runsNamespace` to a dedicated, pre-existing namespace (create it first — the chart never does) and the Role, RoleBinding and the control-plane NetworkPolicy's callback peer move there with it; or set the new `k8s.allowRunsInReleaseNamespace: true` to render as before and accept the shared blast radius. Installs that already set `k8s.runsNamespace` are unaffected. `deploy/kind/quickstart.sh` now creates and uses `wardyn-runs`. (R5 F194)
- **`make setup` stops rebuilding the images it just pulled.** When the published images are pulled successfully, only `agent-claude-code` (never published) is built; the proxy and three agent images are used as fetched. (R5 F099) **Maintainers:** `RELEASING.md`'s required-status-checks list now includes `notices` and the five `trivy` cells — until the documented `gh api PATCH` is re-run, both gates remain advisory on `main`. (R5 F121)
- **CLI and daemon: seven behaviour changes from the R5 hardening round, all tightening.** (1) `wardyn --help` and every usage dump no longer print the value of `WARDYN_ADMIN_TOKEN`/`WARDYN_TOKEN` as `--token`'s default; the token is resolved when used, precedence unchanged (F221). (2) `wardyn ssh <run-id>` refuses a run id that is not a UUID before contacting the daemon — the only ids the SSH gateway could ever authenticate; `--print`, `--json`, `--config` refuse identically (F198). (3) An empty `WARDYN_*` environment variable is read as "unset, keep the default" for every string setting, matching the boolean/duration/integer settings; previously `WARDYN_LISTEN=` erased the compiled default and bound 0.0.0.0:80 with all three listen refusals silenced — to blank a value deliberately, pass the flag (`-listen=`) (F011). (4) `wardynd` refuses a publicly-routable listen address in `-local-mode` when given as a HOSTNAME, not only as an IP literal (F067). (5) `GET /setup/status` no longer returns host git-credential posture (`scm`), host proxy detection (`host_proxy`) or `deployment` to non-operators, and reduces `harness` rows to `provider`/`captured`/`expired`; operators see the full response (F196). (6) `wardyn support-bundle` redaction also covers `APIKEY`, `PASSWD`, `PASSPHRASE`, `AUTH`/`AUTHORIZATION`, `COOKIE`, `BEARER`, secrets nested inside another variable's JSON value (e.g. an audit sink's `bearer_token`), and `--flag=value` argv entries (F070, F143, F166, F201). (7) `wardyn ssh`, `wardyn setup`'s `docker info`, and the `setup wall/vault --run` installer no longer inherit `WARDYN_*` or `ANTHROPIC_API_KEY`; `docker compose config` inside `support-bundle` still does, deliberately, and its output is redacted (F200). `wardyn setup proxy-relay` keeps its `0.0.0.0` default (the command exists because a VM-backed Docker host cannot reach loopback) but now says what that exposes and warns on a non-loopback bind (F202).
- **Helm: two new render refusals, one NetworkPolicy change, and image pins — all upgrade-affecting.** (1) A `WARDYN_AGE_KEY` in `env`/`extraEnv` now counts as an age-identity source, so naming it together with `secrets.ageKeySecretRef` / `ageKeyFromSecret` / `ageKey` fails at render where it previously rendered and the plaintext literal silently won — drop one (R5 F189). (2) `env`/`extraEnv` `WARDYN_ADMIN_TOKEN` together with `auth.adminToken.secretRef.name` or `.value` fails at render; either alone still renders, and the 401 refusal no longer recommends the plaintext door (R5 F192). (3) With `ssh.enabled` or `uiSandbox.enabled`, those ports now ride a NetworkPolicy rule that excludes pods labelled `wardyn.managed`, so a run pod in the release namespace can no longer reach wardynd's SSH or UI-sandbox port — deliberately; HTTP is unchanged and operator-supplied peers other than a bare `podSelector: {}` pass through untouched (R5 F018, F193). (4) Agent images: `NPM_VERSION` 11.19.0 → 11.19.1 — 11.19.0 vendors node-tar inside CVE-2026-73566 (R5 F177); `agent-codex-cli` pins `CODEX_VERSION=0.149.1`, the first release in which that image is reproducible (R5 F120, F165, F178).
- **Helm: the chart's cluster-scoped RBAC objects now carry the release namespace in their names.** The ClusterRole and ClusterRoleBinding were named without it (release `wardyn` in namespace `wardyn` rendered `wardyn-k8s-runtimeclasses`), so two releases in one cluster contended for one object. They now render as `wardyn-wardyn-k8s-runtimeclasses`; `helm upgrade` replaces them in place. Anything outside the chart that referenced the old names — an RBAC audit, an admission policy — needs the new ones. (R5 s2-ops-2:ops2-04)
- `GET /access` no longer carries `issuer`; `provider` is unchanged. The raw OIDC issuer URL was never read by the console — the human-facing IdP name it shows is derived from the issuer server-side and sent as `provider`.
- **Everyone signs in once after upgrading.** The session cookie's format version is stamped, so cookies issued by an older daemon are re-derived rather than accepted. This is deliberate and it is one field's fault: the cookie now records whether a member's group list was truncated, and an absent bit would decode as "not truncated" — the exact wrong answer, since group membership decides which governance profile applies. API tokens carry the same marker from the moment they are minted.
- The GPL corresponding-source offer (`deploy/images/THIRD-PARTY-GPL.md`) is
  regenerated against the 0.6.6 published digests, and offers owed for
  withdrawn tags now live as frozen text in
  `deploy/images/third-party-gpl-historical.md` — the generator refuses to run
  without that file, so a regeneration can never again silently delete an
  offer that is still owed.
- The default workspace-build image is pinned by tag **and** digest
  (`ghcr.io/coder/envbuilder:1.3.0@sha256:…`) instead of floating on `:latest`,
  so the third-party executable a build runs cannot change underneath a
  deployment; `WARDYN_ENVBUILD_IMAGE` (`-envbuild-image`) still moves the pin.
- Console type scale collapsed to four body rungs (`--text-meta` 11px, `text-xs`, `--text-body` 13px, `text-sm`) replacing ~260 ad-hoc sizes; three elevation levels with one `--shadow-floating`; body tracking `0.01em`; helper text at 12px; thin scrollbars on every scroller; radius one-offs onto the card scale.
- `KILLED` counts as needing attention on the board and badge (rank beside `FAILED`); rule-decided tool calls file under the Audit screen's **Tool calls** facet rather than **Egress**.
- The `secrets` table's primary key widens from `name` to `(owned_by, name)` (migration `0050`) so a member can hold their own copy of a name the operator already uses; existing rows are unaffected and keep working exactly as before.
- **In-editor extension installation is documented as unsupported by default.**
  It reaches marketplace CDNs no shipped policy allowlists. The fix is baking
  extensions into the image, not pasting a rotating CDN list into a policy.
- **Scan-seeded egress now requires operator provenance, by default.**
  `WARDYN_REQUIRE_OPERATOR_SET_EGRESS` shipped in 0.6 fully built and **off**,
  because turning it on narrows egress for existing workspaces. 0.7 turns it on:
  the asymmetry was the anomaly. The **secret** side of the very same `switch`
  has always applied this provenance check unconditionally and calls the
  boundary *"security-critical — do not relax"* — and the reason is identical
  for egress. A hostile, or simply never-reviewed, repo could widen its own
  run's allowlist just by naming a host in a committed file, with no operator
  ever acting. **Upgrade impact:** a workspace whose egress requirements are
  `scan_seeded` stops having them auto-added, and the run's warnings name what
  was skipped; an operator declaring the host is the intended fix. Set
  `WARDYN_REQUIRE_OPERATOR_SET_EGRESS=false` to restore the old behaviour.
- **`--agent` is no longer required for a `task_mode=exec` run that names an
  image.** exec runs the task as a plain shell command — no agent harness, no
  model call — so naming an agent was a formality, and `docs/CI.md` documented
  the workaround it forced (`--agent claude-code --image ubuntu:24.04
  --task-mode exec`, in the document whose whole audience is exec). It is **not
  a blanket default**: defaulting to `claude-code` would make every agentless
  run eligible for the operator's live subscription credential, and defaulting
  to `byoa`/`none` resolves to an image that does not exist (both 404 on the
  registry). Harness mode still requires an agent.

### Removed

- The root `GET /auth/logout` is gone. Signing out is `POST /api/v1/auth/logout` — what the console calls, and the only one that clears the session cookie. The root GET was the path that POST never reached (404) before that fix landed, and nothing has called it since: no console, CLI, doc, script or deployment referenced it. `GET /auth/login` and `GET /auth/callback` are unchanged.

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
  CLI cross-builds and the chart package, runs the SBOM merge and its zero-npm
  assertion, and pushes, signs, attests and uploads **nothing**.

  This pipeline could previously only be exercised by tagging, so its bugs were
  unobservable until a real tag pushed — which is why 0.6.2 shipped images with no
  provenance and 0.6.3 shipped an SBOM that understated its own contents. Both
  would have failed a dry run. `make release-check` was green every time, because
  it validates the repository, not the workflow.


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
