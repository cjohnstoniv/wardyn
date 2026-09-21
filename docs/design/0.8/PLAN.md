# Wardyn 0.8 — design

Wardyn 0.8 is the alpha release candidate — the last planned candidate before the alpha
go-live. It adds two features: **posture-gated autonomy**, which maps a sandbox's containment
posture to the autonomy a run is permitted, and **hybrid local and remote**, which makes one
control plane and its enrolled laptops a single product. It also discharges the commitments the
repository's own documentation already made to 0.8. The work is tracked as issues on the `0.8.0`
and `0.8.1` milestones — one issue per work item, grouped under fourteen epics — and how a
branch becomes a pull request and how a release is cut is written in
[CONTRIBUTING.md](../../../CONTRIBUTING.md) and [RELEASING.md](../../../RELEASING.md).

## Goals and scope

**Headline feature 1 — posture-gated autonomy.** An organisation-defined rubric maps a sandbox's
posture (egress reach, secrets present, confinement class) to a permitted autonomy level, enforced
at the Wardyn boundary and, for agents that read a managed-settings file, also expressed as that
agent's enterprise policy file, generated per run and delivered read-only into the sandbox. The
first phase — wire types, one resolution site, managed settings, delivery on both substrates and
the console surfaces — is in 0.8.0.

**Headline feature 2 — hybrid local and remote, phase one.** Today an organisation's control plane
on Kubernetes and the MDM-managed daemon on a laptop do not know about each other. Phase one
enrols the laptop: a device credential, an upward audit forwarder, an organisation-side device
inventory with revocation, and one audit stream with chain evidence on both ends. Placing a run on
either substrate is phase two (0.8.1).

**What the repository already promised to 0.8.** Each of these is a sentence in a shipped
document, so 0.8 either ships it or retires the sentence:

- publish the UI sandbox images (`agent-vscode`, `agent-novnc`);
- subscription and Wardyn-managed runs honour an internal model gateway base URL;
- `POST /runs` answers before the sandbox is up;
- the stored AWS SSO credential warns rather than raising a hard confinement floor;
- drive storage objects get a fixed-width identifier;
- a second terminal escape chord for keyboard layouts where the current one is untypeable;
- Bedrock SigV4 through the inspecting proxy — restated as a documented ceiling, with its reason;
- GitLab and Bitbucket as git-provider kinds — restated as a later minor, not 0.8.

**Known gaps carried from 0.7.x.** The corp-proxy estate whose cause is still unidentified;
terminal observers that become writable only on reconnect; the terminal's ordinary-use corpus and
the Kubernetes attach path, both untested; the setup Review step grouping by grade rather than by
whether a check blocks; a credential-renewal outage refusal with nowhere to show itself; a
per-person sign-in race that can leave two live credential-bearing sandboxes; toolchain caches
outside the metered volumes on unattended Kubernetes runs; a spent-token mark that does not
survive a restart; server-error bodies that still carry driver text; the credentialed daemon
proxy; the Runs board group header; the secret-masking lifetime.

**Four more strands.** The 0.7.x readiness-review follow-ups, where every item that review raised
and 0.7 did not handle is planned, deferred or rejected with its reason (#86). A quality sweep
across comments, documentation form, console UX and code, the configuration and API surface, and
the test estate (#89), so the tree reads as a product rather than as a work log. Dependency hygiene
(#88): the open dependency pull requests landed, the development-time advisories closed, the update
configuration taught the node LTS rule. And push content rules (#87, issue #57) — what a sandbox
may push, not only where.

### What 0.8 is not

Nothing on the v1.0 row moves up. Restated as four named gaps, not a placeholder, because
per-pod PIDs in particular keeps being picked up as though it were a one-liner: bring-your-own
and devcontainer image builds, `local_dir` host-path mounts, a per-pod PIDs limit, and a k8s
ground-truth correlator, each carried in [deploy/helm/wardyn/README.md](../../../deploy/helm/wardyn/README.md)'s
"Known gaps" as chart v1.0 scope. The PIDs one is not a one-line change: Kubernetes exposes no
per-container "max pids" resource name — only the kubelet's node-level `podPidsLimit` — as
`naming.go` and `sandbox.go` in `internal/runner/k8s` record, which is why Wardyn accepts the
value, warns, and runs without enforcing it rather than claiming a cap that was silently
dropped. Packaged team mode, SAML and SCIM stay unscheduled.

**No refusal that exists today is relaxed**, with three exceptions, each an operator- or
admin-controlled widening and never sandbox reach:

1. a member may write their own `bedrock-api-key`; the three SigV4 secret names stay admin-only;
2. three drive routes that name no host path move from admin to security-admin (the route table
   already pre-authorised that tier);
3. the subscription injection host accepts the operator-configured gateway host beside
   `api.anthropic.com` — from configuration, never from a request.

## Release shape

**0.8.0** carries the first phase of both headline features — the autonomy rubric (types,
resolution, managed settings, delivery, console) and hybrid phase one (enrolment and audit
federation) — plus every promised item that is provable locally. The alpha-RC claim is made at
0.8.0 and is honest because both features ship their first phase and the roadmap says which
phase.

**0.8.1** carries hybrid phase two (per-run placement), phase three-a (drive-as-source), the
identity follow-ups, and the React 19 upgrade.

**Patch releases** are cherry-picks from `main` onto the `release/0.8` branch, unchanged from the
existing practice. Rejected: one large 0.8.0 holding everything — field reports arrive faster than
a monolith can be cut, and a dozen parallel changes conflict on the changelog alone.

## How the work is organised

Every work item is a GitHub issue on the `0.8.0` or `0.8.1` milestone. Each issue carries its
goal, the files it touches, the change, the command that checks it, a binary done-when, and its
risks. The milestone burn-down is the status of the release.

Fourteen epics group the issues, and each epic holds the design record for its area:

| Epic | Area |
|---|---|
| [#77](https://github.com/cjohnstoniv/wardyn/issues/77) | Posture-gated autonomy |
| [#78](https://github.com/cjohnstoniv/wardyn/issues/78) | Hybrid enrolment and audit federation |
| [#79](https://github.com/cjohnstoniv/wardyn/issues/79) | Hybrid placement and drive-as-source |
| [#80](https://github.com/cjohnstoniv/wardyn/issues/80) | Asynchronous run creation |
| [#81](https://github.com/cjohnstoniv/wardyn/issues/81) | Terminal and attach |
| [#82](https://github.com/cjohnstoniv/wardyn/issues/82) | Deployment gaps |
| [#83](https://github.com/cjohnstoniv/wardyn/issues/83) | Identity and credential residuals |
| [#84](https://github.com/cjohnstoniv/wardyn/issues/84) | Console readers and UI residue |
| [#85](https://github.com/cjohnstoniv/wardyn/issues/85) | Drives, verification debt and release mechanics |
| [#86](https://github.com/cjohnstoniv/wardyn/issues/86) | 0.7.x readiness-review follow-ups |
| [#87](https://github.com/cjohnstoniv/wardyn/issues/87) | Push content rules and held pushes |
| [#88](https://github.com/cjohnstoniv/wardyn/issues/88) | Dependency hygiene |
| [#89](https://github.com/cjohnstoniv/wardyn/issues/89) | Code, docs and test quality sweep |
| [#90](https://github.com/cjohnstoniv/wardyn/issues/90) | The 0.8 working practice |

**The order of the work.** The dependency bumps and the change that makes documentation citations
point at symbols instead of line numbers go first: the first unblocks a nightly job and closes
three advisories, the second removes the friction where inserting a line above a cited line reds a
required check on every later pull request. The mechanical cleanups — comment sweeps, document
restructuring, file splits — land before the feature work, so nothing rebases across a
ten-thousand-line sweep. Console work goes through a mock round first: the states and the exact
strings are agreed before implementation, and the strings in the mock are the strings in the code.
Security-sensitive changes are reviewed before merge, not after.

**Migration numbering.** Each pull request takes the next free migration number when it rebases,
not a number reserved in advance; a change that lands out of order renumbers its migration file
and its changelog entry before merging. No test may hardcode a number: one guard forbids gaps and
duplicates, another requires every migration to be named in the changelog.

## Workstreams

One subsection per epic: what it delivers, the decisions behind it, the main risks, and any open
question with its proposed answer. The individual work items live on the epic's checklist.

### Posture-gated autonomy — [#77](https://github.com/cjohnstoniv/wardyn/issues/77)

**Delivers.** Four autonomy levels on the wire (`L0` attended, `L1` gated, `L2` unattended, `L3`
also permitting `task_mode=exec`) with plain labels in the console; a rubric on the governance
profile; one resolution site shared by launch and the Review step; a generated managed-settings
file delivered root-owned into the sandbox; provenance on the create audit row, the run record
and a new delivery row.

**Decisions.**

- Four bespoke rungs rather than reusing the confinement classes, because the question is what a
  run may do unattended, not how it is contained. `L1` derives an explicit `auto` down to `hold`;
  an agent with no hold lane is refused a non-interactive run at `L1`.
- The rubric lives on the governance profile as nine closed fields — three egress states, three
  secret states, three confinement classes, each unset or a level — rather than on site
  configuration (operators are exempt from it) or on the run policy, which would fan out to the
  composer clamp, the policy-document guard, the proxy and three mirrors for nothing the proxy
  can enforce. The resolved level is the minimum over the applicable caps; all unset means no
  level and today's behaviour, and limits are already JSON, so there is no schema change.
- One resolution site shared by launch and the Review step rather than two, so the existing
  parity test sees one gate on both doors: arithmetic in `internal/composer/autonomy.go`, the
  gate in `internal/api/runs_autonomy.go`.
- Enforcement reuses the existing member-refusal shape and the existing levers rather than a new
  refusal vocabulary, so the closed reason enum stays closed. The derived value is written into
  the request before the audit row and the dispatch parameters, so both agree.
- Managed settings are generated from a pure package for the agent that reads one; the other
  agent gets no file and one honest warning, rather than invented deny rules it would not honour.
  The unattended level must not disable the permission-skipping mode, or that launcher branch
  dies at its first tool call.
- Delivery is root-owned through the runner contract: Docker copies a root-owned archive between
  container create and start, Kubernetes mounts a per-run Secret read-only. Rejected: writing the
  file as the agent user, and a one-shot root exec, which races the main process.

**Risks.** The managed-settings keys are only as good as the pinned CLI, so each is checked against
a built `agent-claude-code` container before the generator freezes.

**Open question.** Posture computed before or after the run's egress union → **before**, because
the Review step cannot union; the residual is named in the release notes.

### Hybrid enrolment and audit federation — [#78](https://github.com/cjohnstoniv/wardyn/issues/78)

**Delivers.** A laptop daemon that enrols into an organisation's control plane and forwards its
audit rows upward; a device credential on its own authentication lane; organisation-side minting,
inventory and revocation with a CLI; federation lag on the health endpoint; a two-database gate.

**Decisions.**

- The laptop keeps its full daemon in member mode and gains exactly two things — a device
  credential and an upward forwarder — rather than a thin client mode, which would be a new
  execution component and would leave the laptop useless offline.
- The device credential is a new row plus a distinctly prefixed bearer accepted only under the
  device routes, rather than the human token prefix (which republishes a human identity) or an
  attach ticket (thirty seconds, run-bound). It can never create a run.
- Enrolment: an admin mints a single-use token, MDM renders it into the laptop's secret file, and
  first boot exchanges it for a device credential held in the laptop's age-encrypted secret store
  under a reserved name — rather than mutual TLS (there is no CA in the tree) or a long-lived
  MDM-delivered token, the mistake the encrypted store already avoided.
- Offline runs continue under the last delivered ceiling, and evidence is durable rather than
  buffered: the laptop's own chained table is the buffer and the forwarder advances a durable
  cursor. Rejected: a new sink kind, at-most-once past its buffer and streaming rows without their
  hashes. Revocation fails closed — new run creation answers 503 naming re-enrolment.
- One chain per writer: the organisation ingests each laptop row as its own chained row with the
  origin inside the row's data, rather than wrapping them in a new audit action that would break
  every filter and SIEM rule. At ingest it recomputes the claimed hash and requires the previous
  hash to match the last one it recorded; a mismatch refuses the batch, and a genesis row after a
  purge is accepted, counted and audited as a chain reset.
- Phase one federates audit rows, not run records — the organisation's run list does not show
  laptop runs — and the forwarder pushes from the table rather than the organisation pulling,
  because there is no inbound path to a laptop behind NAT.
- Device routes mount unconditionally behind an optional store capability that fails closed, 501
  or 401, rather than conditionally, which would make the authorization matrix skip them.

**Risks.** A run created between revocation and the next forwarder tick is legitimately local, and
the documentation says so. Self-reported rows prove links are intact, not that the set is complete
— the chain-reset row is the evidence, and the residual is in the threat model.

**Open questions.** Single-use tokens per device, not a fleet token with a use count. No console
device inventory in phase one — CLI and API only. On revocation, refuse new local runs.

### Hybrid placement and drive-as-source — [#79](https://github.com/cjohnstoniv/wardyn/issues/79)

**Delivers** (0.8.1). A `placement` field on run creation, one resolution site, a ceiling term,
placement-scoped capabilities, drives that resolve per placement, a CLI flag, a routing
conformance suite, and drive-as-source as one drive row with a second backend.

**Decisions.**

- `placement` is optional and its empty value resolves to the deployment's own executor with a
  provenance bit, rather than a refusal that would break every existing caller. At a laptop
  daemon the default is local; an organisation that wants remote-only forces it with the ceiling.
- One resolution site, before the run row exists, rather than at dispatch — a 201 followed by a
  dispatch failure is the shape the existing tests forbid.
- A new column rather than reinterpreting the runner target, because the runner target names the
  substrate and placement names where; under hybrid both sides can be the same substrate.
- The ceiling is a deny list, nil meaning unlimited, rather than an allow list (whose zero value
  would deny) or two booleans (a third value would be a third branch). A profile that denies
  every placement is refused when it is written.
- Capabilities intersect while the placement is unresolved and use the real placement's
  capabilities once resolved; the union survives only for display. The orchestrator's union
  comment is rewritten, because per-run placement voids its justification.
- A placement-eligible drive is one whose enforcement is external; that is said at registration as
  a label rather than a refusal, because refusing a backend on a shipping path would be new.
- Drive-as-source widens one drive row with a second placement-keyed backend rather than adding a
  second row, because grant uniqueness is per subject across all drives and the resolver's
  single-row pick would silently choose one.
- An unresolvable or stale placement capability answers 503, never a substituted local answer.

**Risks.** Intersecting the ephemeral-disk enforcement must keep the weakest-wins direction rather
than a string minimum. Confinement resolution skips its membership check when no runner is wired,
so a headless organisation daemon would admit every class while every existing test passes.

**Open question.** How drive-as-source is positioned → **half, named**: it links a shared export,
not the laptop's working tree, and synchronisation to a laptop is a stated gap.

### Asynchronous run creation — [#80](https://github.com/cjohnstoniv/wardyn/issues/80)

**Delivers.** `POST /runs` answers when the run row exists; the image build and dispatch move into
a worker; the console navigates to the run page immediately; reaped never-dispatched runs carry a
reason; the sign-in supersede's teardown detaches from the request.

**Decisions.**

- Keep `201 Created` and add a `Location` header rather than switching to 202, because the run row
  does exist at answer time; 202 breaks status-code checks and buys nothing a decoder reads.
- The split point is the boundary the code already names for client-disconnect isolation. No error
  is written below it, so by construction no refusal can turn into a failed run; every refusal and
  every clamp stays synchronous.
- An in-process worker rather than a durable queue, because the run token is never persisted — a
  queue would either store a live credential or mint a second one, breaking the token identifier
  stamped in the create audit row. The same shape already ships for the sign-in launch.
- `PENDING` is the queued state — no new state and no migration — because the dispatcher already
  transitions out of it, the reconciler reaps a pending run with no sandbox, and kill accepts one.
- Moving the image build into the worker is the real prize: the build timeout is thirty minutes
  against a console launch deadline of five, so a bring-your-own-image launch that takes longer
  than five minutes is already broken in the console today.
- The console navigates immediately and carries advisories in router state; the held "Open run"
  screen is retired, closing a 0.7.7 gap. `wardyn run --wait` does not move; the non-waiting print
  shows a pending run and no image yet.
- The supersede's kill cascade splits into a claim half — the transition that frees the
  concurrency slot and cancels approvals — and a teardown tail that runs detached, rather than
  holding the sign-in request open for the teardown.

**Risks.** Forty-one Go test files create runs and almost none read terminal state, so a worker
that never starts would leave them green while runs strand pending; every test that reads
post-dispatch state must wait for the sandbox or it flakes rather than fails. A reaped run needs
its reason written only on the failure transition, or a success shows a red chip.

**Open question.** Do launch advisories survive a reload → **no**, recorded as a known gap; the
durable record is the create audit row.

### Terminal and attach — [#81](https://github.com/cjohnstoniv/wardyn/issues/81)

**Delivers.** In-place observer promotion; a test corpus for ordinary use in Go, in unit tests and
in the browser; a second escape chord; a Kubernetes attach test; a read-only notice in the CLI;
SSH cases on both substrates.

**Decisions.**

- One writer and an ordered observer list, with the observer marker moved off "there is no holder"
  onto a per-client writable flag, because both pumps null their own holder today, so a promoted
  observer has no object to flip. Every write and resize already funnels through one predicate, so
  promotion becomes a single atomic store.
- First-in-first-out promotion on ordinary release, and a take-over promotes only the taker's own
  observer — promoting a bystander would misattribute an audited act. Every observer passed the
  same authorization check when it connected, so ordinary promotion grants no new capability.
- The promotion frame is sent after the registry lock is released, because a client write can
  block for thirty seconds and the library permits concurrent writes.
- Promotion carries no geometry; the promoted client re-asserts its own size.
- The second chord is layout-independent rather than `Ctrl+Alt+]`: AltGr *is* Ctrl+Alt on Windows
  and Linux, so on a German layout an ordinary `]` would become untypeable. Keying on the physical
  key is also wrong — that key is `+` on QWERTZ, and Ctrl and `+` is browser zoom.
- The key handler moves into a pure module and the terminal copy block into its own file first,
  because both files sit just under the size cap and a commented branch costs twenty lines.
- The Kubernetes attach test runs against a fake client set behind a build tag, matching the
  Docker twin, rather than gating on a live cluster; the browser corpus is hermetic by routing the
  WebSocket, because the end-to-end backend runs with no runner and a real attach is refused.

**Risks.** A promoted observer whose socket is already dead holds the slot for up to two ping
intervals — named, not fixed. Claiming "true in-place promotion" while a taker with no observer
socket still reconnects would overclaim.

**Open questions.** Which chord → **`Ctrl+Shift+Backspace`** as the single hinted spelling, with
the existing chord kept as an undocumented alias. Does a take-over promote the taker's own socket
→ **yes**. Does promotion get its own audit action → **yes**, alongside take-over.

### Deployment gaps — [#82](https://github.com/cjohnstoniv/wardyn/issues/82)

**Delivers.** The UI sandbox images become publishable and published, with their licence files and
SBOM coverage; the model gateway carries subscription and Wardyn-managed runs; a gateway may
declare its own auth header; the direct-dial bypass gets documentation and a boot lint; three
corp-proxy instruments; a credentialed daemon proxy; an air-gapped video mirror.

**Decisions.**

- `agent-vscode` rebases onto `agent-base` rather than onto the unpublished vendor image, which
  bundles a proprietary CLI whose licence text is in neither the image nor the repository — and
  the published catalog already resolves that agent to the base image, so this is consistent
  rather than a downgrade. A developer checkout keeps the vendor variant through a build argument.
- The GPL written offer is generated from a local SBOM run at release preparation and its image
  list derives from the release pipeline, rather than a hand-maintained list scanning the
  *previous* release's digests — a bootstrap deadlock for a first publish, and publishing first
  would convey an X stack with no offer, the one failure a later commit cannot undo.
- Gateway support is three-sided: dispatch pins the base URL, the in-image launcher unsets it
  again for the subscription and Bedrock modes, and the injection host check refuses any host that
  is not the vendor's. The fix points the client at the gateway and lets the brokered route carry
  it, rather than rewriting the tunnel's CONNECT target, which would re-point an opaque tunnel at
  a destination the evaluator never allowed.
- The daemon proxy credential is a file reference rather than a secret-store reference, because
  the boot transport is installed before the database connection and the secret store exist.
- The air-gapped mirror is one operator-set base URL published on the health endpoint, with the
  media-source header built through the existing host-sanitising helper, not bundled video files.
- The corp CA is staged into the runtime image concatenated with the system roots, because a bare
  CA would replace public trust and take every vendor dial down; the build fails closed if only a
  bare CA is staged. A configuration warning and support-bundle fields name the upstream host and
  the compiled bypass list; which cause belongs to which estate stays a field question.
- Per-pod PID limits stay v1.0, because Kubernetes has no per-container PIDs resource.

**Risks.** The release dry run writes empty SBOM stubs, so it proves nothing about SBOM content.
The UI-sandbox end-to-end test tags a local image and can pass against a stale image id.

**Open questions.** Ship the UI images in 0.8.0 or 0.8.1 → **0.8.0**, with the local SBOM bootstrap
stated plainly and regenerated from published digests at 0.8.1. Does the gateway re-point the
subscription lane by default → **yes**, said in the docs and the boot log, including that it sends
the operator's token to the gateway.

### Identity and credential residuals — [#83](https://github.com/cjohnstoniv/wardyn/issues/83)

**Delivers.** A per-person sign-in advisory lock; a persisted spent-token mark; warn-first
confinement advice for the stored SSO credential; the capture closing its own sandbox; a refreshed
group snapshot on API tokens; a per-user Bedrock bearer; two defects the review left open.

**Decisions.**

- The lock is a session-scoped advisory lock keyed on the login run's creator, taken by both the
  launch and the capture, rather than a transaction-scoped one: the launch spans four independent
  statements and the capture is a read-modify-write in a different request minutes later. It is
  not keyed on the credential scope's principal, which is empty for every shared sign-in.
- The lock fails open to today's behaviour on timeout or on a store that does not offer it, rather
  than introducing a refusal. The bounded wait is load-bearing: a connection-per-hold lock at the
  documented pool minimum self-deadlocks, because the guarded work needs a second connection.
- The spent-token mark persists in its own table behind an optional store seam, with the in-memory
  map as a write-through cache — not a field on the stored credential, because the mark exists
  precisely for the case where storing the credential failed.
- Stored-credential confinement ships as a warning on three surfaces — preflight warnings, the
  create response's warnings, and a closed-vocabulary field on the create audit row — rather than
  raising the confinement floor, which is a pure function over eligible grants and does not see
  this credential at all. It is never a refusal on a host that offers only the weakest class.
- Bedrock SigV4 through the inspecting proxy stays the 0.8 ceiling: it would make the SSO bearer
  non-resident, but the derived role credentials stay resident because signing happens in process,
  so the residency the console reports would not actually move.
- The per-user widening covers the bearer lane only; the three SigV4 secret names stay admin-only,
  and full member-supplied Bedrock stays the ceiling.
- The capture kills its login sandbox only after the upload response is written, because the
  in-sandbox helper must receive its answer and emit the marker the console corroborates before
  teardown. The shell-owned pane becomes the only sign-in mount, so the orphan class disappears
  rather than being swept, and the server-side kill stays as the belt for a closed tab.

**Risks.** The console pane's own kill races the server's; the cascade is idempotent, but the copy
must not report the server's win as a failure. The spent-token fingerprint must be non-reversible.

**Open questions.** A hard refusal when the lock cannot be taken → **fail open**. The confinement
advisory as a field or its own audit action → **a field on the create row**. An opt-in flag for the
bearer widening → **none**: the combination is refused today, so no existing record is surprised.

### Console readers and UI residue — [#84](https://github.com/cjohnstoniv/wardyn/issues/84)

**Delivers.** A substrate-honest confinement posture chip and banner; the setup Review step grouped
by whether a check blocks; a visible chip for the not-applicable model-access state; the Runs
board's group header, member empty state and stale holds; the Recordings screen's own pagination;
canon tables for the console surfaces that shipped without one.

**Decisions.**

- The banner reads the anonymous health endpoint the shell already fetches at mount, because it is
  the only source honest for every deployment tier.
- The confinement chip reads a context rather than a prop, because there are twelve call sites.
- Four posture states — enforced, acknowledged, unenforced, unknown — because an acknowledgement
  flag exists and grades as a warning, never as ready; on Docker the field is absent and the chip
  does not change.
- New strings go in per-screen copy modules; exactly one change touches the shared copy module,
  and only to reword one string in place, because that file is near the size cap.
- The Review step groups by whether a check blocks and keeps the grade as the row's own chip,
  because "does this block?" and "how did it grade?" answer different questions, and the gate
  already says a grade alone never blocks.
- The Runs board's group header gets one counted chip per wait reason rather than a sentence,
  built from data already in hand, and renders a pinned "checking" chip before the approvals
  promise resolves instead of an empty map that reads as "nothing is held". A hold older than a
  named ceiling degrades to "was held — check the run".
- Recordings pagination is server-side offsets rather than a client-side cap, because the screen
  filters after the fetch, so a cap re-slices a window that already dropped everything past the
  first thousand rows. It is the console's first paged screen, so it gets a full mock round.
- No change edits the roadmap, and none edits the changelog beyond its unreleased section; the
  release change reconciles both.

**Risks.** Six of the twelve historic console-residue items have no surviving description; the rest
are recovered from commit messages. If the remainder cannot be re-supplied, five are covered and
the others are struck from the roadmap rather than carried as unexplained ids.

**Open question.** The group header → **ship the breakdown**, not a sentence.

### Drives, verification debt and release mechanics — [#85](https://github.com/cjohnstoniv/wardyn/issues/85)

**Delivers.** A fixed-width drive object id; a third Kubernetes cache volume; a drive probe on the
runner contract; drive reclaim; a drives CLI; the grant and preview tier move; the server-error
text sweep; the test-gap backlog and coverage floor; a nightly SSO walk; the release tooling.

**Decisions.**

- New drive objects mint a fixed-width identifier derived from the drive's own id, with the scheme
  persisted per row; existing rows keep their names forever rather than being re-homed, because
  neither substrate can rename a storage object and copying bytes is a capability the product
  refuses. Moving one is an operator data move plus a row edit, and the runbook says so. The
  scheme flip reuses the existing re-home machinery and its confirm-or-conflict door.
- The Kubernetes disk residual closes with a third ephemeral volume at the cache path, with the Go
  module cache, the Go temporary directory and the npm cache moved under it — not by changing
  `GOPATH`, which would lose the installed tool binaries. The readiness review's constraints hold:
  the full image's login profile re-exports these variables unconditionally, so either the image
  changes or the volume sits where the profile points; a leaf volume hides image-baked content
  beneath it; and sub-paths are refused by ephemeral containers.
- Drive probing is an optional capability interface rather than a widening of the runner
  interface; the probe runs as the agent user, so a share readable only by root refuses
  identically at create, at preflight and on the member's own view.
- Reclaim ships as a daemon verb, a CLI verb and an audit row behind a chart value defaulting off,
  with no console button, because a destructive confirmation needs a screen with a mock.
- Byte enforcement on Docker volumes closes as a documented ceiling with a filesystem
  project-quota recipe — the case the enforcement enum reserved a value for.
- The server-error text sweep ends in a guard test with a shrinking allowlist rather than a
  one-time edit, because ninety-three sites still concatenate driver text into a server error.
- The Kubernetes SSO walk becomes a nightly job on a hosted runner, excluding the one case that
  spends ten minutes by design, and the job asserts the walk actually executed — an unset switch
  would otherwise exit zero and prove nothing.

**Risks.** An ephemeral volume at the cache path shadows the image's pre-created directories, and
the filesystem group is set only when a drive attaches — so a fake test passes while every real
build fails on permissions, and the conformance fill target is the gate that matters.

**Open questions.** Legacy drive names forever or a data-move change → **accept the ceiling** and
ship the recipe. Reclaim by API and CLI now, console later → **yes**. Nightly SSO walk → **hosted**.

### 0.7.x readiness-review follow-ups — [#86](https://github.com/cjohnstoniv/wardyn/issues/86)

**Delivers.** A disposition of record for every item the 0.7.x readiness review raised — planned,
deferred with its reason, or rejected with its reason — mirrored into the 0.8 release notes and
back onto the review branch, so both sides read the same truth.

**Where it stands.** Of twenty-eight numbered rows, twenty-one shipped in v0.7.6 and are closed.
Three were deferred and are now planned: the per-person sign-in race, the toolchain caches outside
the metered volumes, and the orphaned login sandbox — each in the epic that owns its code. Two
rejections stand. One row was a duplicate of a pull request that is still open and is a
prerequisite for the nightly SSO job, so that job depends on it rather than re-implementing it.

The review's security notes without a number become three 0.8.1 candidates: session revocation
cascading to registered SSH keys, with the gateway re-checking key validity on every new channel;
an opt-in requirement that the identity provider's email claim be verified, default off so it adds
a refusal only where it is set; and a loud warning — a refusal in 0.9 — on a plaintext internal
issuer outside a loopback or demo posture.

Three residuals defer as documented ceilings with their reasons: a support-bundle entry redacted
for reading rather than re-use; symlinks inside the recording root; export modes on filesystems
that ignore them.

From the review of the previous release plan, thirteen of fifteen findings shipped. Both partials
are real and both planned: a card title that asserts a run is paused from a pending row alone, and
an AWS sign-in door that can open for a refusal that came from a different credential path. One
requirement is open in the tree — the console surfaces that shipped in 0.7.6 and 0.7.7 have no
canon artefact, and they get one before the next visual change touches them.

**Decisions.** This disposition table is the record rather than a re-litigation, because every item
must stay trackable between releases. A deferred or rejected item is filed as an issue and closed
with its reason, so it stays searchable rather than being lost.

**Risks.** The review's acceptance gaps — live identity and revocation scenarios, a real concurrent
SSO walk, restart, reconcile and restore, a fresh chart install and upgrade, outage recovery with
spool replay, and production role separation — are exercised by the end-to-end gates but are not
release-gating, and the release notes say so, so they are not mistaken for certified.

### Push content rules and held pushes — [#87](https://github.com/cjohnstoniv/wardyn/issues/87)

**Delivers** (issue #57, two changes on one issue). Phase one: a closed `push_rules` policy field,
a packfile inspector, `no-thin` on the receive-pack advertisement, and enforcement in the broker —
deny, uninspectable, too large. Phase two: a held `push_content` approval kind and its console card.

**Decisions.**

- `push_rules` is a closed structure on the run policy rather than a rules map, because strict
  decoding and the policy-document reflection guard cannot police a map. Phase one carries denied
  paths and an inspection size ceiling; phase two adds review-required paths, new-executable
  denial, a file-size cap and a hold duration.
- The proxy advertises `no-thin` on the receive-pack advertisement, because the agent images clone
  shallow, so a real push's root tree is normally a delta against a base that is not in the pack —
  without it the inspector would be a false-refusal machine. Rejected: fetching base objects from
  the forge.
- Content rules are entered independently of branch-namespace confinement, so opting out of
  *where* a push may go never silently disables *what* may be in it.
- An oversize pack is refused rather than held, because holding asks a human to approve a push
  nobody inspected — the blanket bypass the issue exists to close. The remedy is an operator
  raising the ceiling, which is an auditable authoring act. As a consequence phase one needs no
  approval kind, no migration and no console change.
- The retry cache is the approval state machine: deduplication keys on the sorted paths and commit
  ids rather than a pack digest, which is unstable across repacks.
- Refusals are recorded as decision-log rule sources rather than new dotted audit actions, joining
  an existing family; offending paths ride the approval scope and the structured log, never the
  free-text fields reserved for dial-shaped refusals.
- Phase two's approval kind is admin-decidable only, because a member approving their own
  workflow-file edit is exactly the exfiltration the self-approval rule stops; unattended runs deny
  outright rather than holding for a human who is not there.
- Both brokered lanes are governed, on one trigger — the run's policy carries a rule — and
  neither waits on a branch-namespace switch. The token lane terminates and holds the whole
  request either way, so gating what a push may contain on a switch about where it may land
  would leave a policy reading as governed while enforcing nothing, and the `no-thin`
  advertisement follows the enforcement onto that lane so its rules are not a false-refusal
  machine. The key lane is never governed, because it is an opaque tunnel — a medium-risk row
  warns when rules are set and the only git grant is a key.

**Risks.** A short delta buffer yields a plausible tree, so the reconstructed size is checked
against the delta header; and "push any branch" must not silently disable content rules.

**Open question.** `no-thin` on by default when rules are set → **yes**, off when they are nil; a
fourth switch would create a posture where rules are set and silently uninspectable.

### Dependency hygiene — [#88](https://github.com/cjohnstoniv/wardyn/issues/88)

**Delivers.** The open dependency pull requests triaged and landed, three development-time
advisories closed, and the update configuration taught the node LTS rule.

**Decisions.**

- The audit target is production-only by design, which is why a development-time advisory never
  reddened CI; the fix is the bump, not a wider audit target.
- Order: the nightly-harness fix first — it is the prerequisite for the nightly SSO job and it
  fixes the racy test that reds another pull request's build — then the actions group, then the
  rebased checkout bump. One bot pull request closes as already superseded by the tree.
- Two groups land by fix-forward rather than waiting for the bot: regenerate the notices file the
  bot cannot regenerate, and replace four struct comparisons that a client-library version made
  non-comparable. If the container-client bump breaks the Docker driver, it splits into its own
  change rather than holding ten safe bumps.
- The base-image group splits: take the compiler digests now, defer the node major until it enters
  long-term support with a dated ignore entry naming the release schedule as its source — rather
  than the hand-listed odd-major rule, which let an early even major through.
- The test-runner major is taken rather than dismissed, so every new test in 0.8 runs on it;
  dismissal with a recorded reason is the fallback only if the migration will not fit.
- React 19 defers to 0.8.1 as its own change after the console work merges, because a major
  mid-cycle moves every test file; it unblocks the router major that follows it.

**Risks.** The browser image pin must match the test-runner bump, and a matcher library
peer-depends on the test runner, so its range is confirmed first.

### Code, docs and test quality sweep — [#89](https://github.com/cjohnstoniv/wardyn/issues/89)

**Delivers.** Six areas surveyed and fixed: documentation and narrative, the API package, the data
plane and daemons, console UX and code, the configuration and API surface, and the test estate.
Where it starts: 507 markdown files over 49,055 lines, a 7,021-line changelog and a 5,645-line
runbook; 48.6% comment lines in the API package; 4,135 Go tests; one concept named three ways.

**Decisions.**

- Documentation citations become symbol-anchored rather than line-anchored, because a zero-line
  window over 209 citations means any insertion above a cited line reds a required check. What the
  gate loses is recovered by asserting the literal count inside the symbol's body. This is first.
- The changelog becomes terse in the Keep-a-Changelog sense with the narrative moving to
  per-release pages, but every migration name stays in the changelog file, because the guard reads
  only that file. The runbook splits into task pages behind a hub kept at its path: thirteen guard
  sites read it by path, so the scraped tables stay at their literal headings.
- One word per concept: **confinement class** in the API, documentation, audit and CLI, with
  "barrier" allowed as the console's plain-language label beside the class code; "tier" is retired.
  "Operator" is the person who deploys; admin, security-admin and member are the roles.
- Documentation gets a form rule with measurable targets — a summary block, a diagram for every
  topology, flow or state machine, numbered steps with the command and its expected output, tables
  for options and states, a paragraph-share cap for runbooks and a sentence cap in normative
  documents. A script prints the measurement and its targets become assertions.
- The console adopts a linter with the hook and floating-promise rules wired into the build, rather
  than keeping twelve suppressions written for a linter that does not exist.
- Defects the survey found are fixed with tests, not merely noted: server errors logged without
  method, path or trace at twenty-one sites; a requirement-audit record dropped on one launch path;
  an unlocked write of the decision log from the request path, where a long line can interleave;
  one refusal spelled two ways across a body and a header; the one detached goroutine that skips
  the panic-safe wrapper; an unhandled rejection at session lapse; and four console types that
  have drifted from their Go definitions.
- Hand-rolled machinery is replaced where the replacement is smaller and already available: a byte
  budget becomes a weighted semaphore and eight copies of a loopback predicate become one package.
- Pure-move splits land before the changes that would otherwise rebase across them, every
  behaviour-preserving change runs the full gate before and after with identical results, and why
  code exists is researched before it is deleted.

**Risks.** Roughly ten thousand lines of tests assert prose, so a wording change is a multi-file
change, and renaming a test disables a zero-executed floor unless the floor's matcher moves with it.

### The 0.8 working practice — [#90](https://github.com/cjohnstoniv/wardyn/issues/90)

**Delivers.** The practice written into CONTRIBUTING.md and RELEASING.md, issue and pull-request
templates, the `0.8.0` and `0.8.1` milestones, the label set, and the issues themselves.

**Decisions.**

- Pull requests target `main` directly rather than an integration branch, because the branch
  protection and the required checks are built for `main` and every change in 0.8 is additive —
  unset behaves exactly as today, so `main` stays releasable between the parts of a multi-part
  feature.
- One issue per pull request by default, with tightly coupled issues allowed together up to three,
  because some changes cannot be reviewed apart. Dependent work stacks: branch from the previous
  branch, name the dependency in the body, retarget after it merges.
- Merge commits for multi-commit feature changes, so the reviewable commits survive; squash for
  single-commit chores.
- Each epic carries the design record for its area and a task list of its issues. Deferred and
  rejected items are filed and closed with their reason, so nothing is lost between releases.
- Every change edits the changelog's unreleased section itself; the release change moves that
  section to its version, updates the roadmap row and the version pins, and the release branch is
  then cut from that merged commit and tagged on it. Point releases are cherry-picks.
- Evidence is certified against the commit it ran on: any commit after it re-opens the gates it
  invalidated. That rule is written into RELEASING.md rather than practised informally.

## Decisions taken

| Decision | Substance |
|---|---|
| 0.8.0 scope | Both headline features' first phase — the autonomy rubric with managed settings and console, and hybrid enrolment with audit federation — plus every 0.8-promised item provable locally. Placement, drive-as-source, the identity follow-ups and React 19 go to 0.8.1. |
| Console work goes through a mock round | States and exact strings are agreed as canon before implementation, and the strings in the mock are the strings the code ships. Waivable per screen only where a shipped surface gains no new copy. |
| The forced landing on Getting Started is retired | No code change; the affected demo narration and captions are recut in place, because that episode has no recording and its narration lives in the spec. |
| Review before merge | Every change is read by a second reader before it merges; security-sensitive changes additionally get a security review, and the review's findings are posted on the pull request. |
| One word for the isolation concept | "Confinement class" in the API, documentation, audit and CLI; the console may show "barrier" as a plain-language label with the class code beside it; "tier" is retired. |
| Three widenings, and only three | A member may write their own bearer key; three drive routes move to security-admin; the injection host accepts an operator-configured gateway host. No other refusal is relaxed. |

### Open decisions

| Question | Proposed answer |
|---|---|
| Bedrock SigV4 through the inspecting proxy | Keep as a documented ceiling in 0.8.0 — signing happens in process, so the residency the console reports would not move. |
| How issue #57 is phased | Deny-only first, with holds as a second change on the same issue; phase one then needs no approval kind, no migration and no console change. |
| Environment-variable renames and merges | Do them in 0.8.0: seven renames, one merge and one polarity flip, each keeping the old name for one minor with a boot warning, landing with a status column and a deprecation policy. |
| Audit action renames (nineteen past-tense names, visible to SIEM rules) | Emit both names for one minor with a permanent "renamed in 0.8" appendix; defer to 0.9 if any known SIEM rule keys on the old names, with the appendix landing now. |
| Route renames and the CLI verb tree | Do them with aliases for one minor and a rename table in the SDK documentation; flip the one inconsistent `--json` default with an upgrading note. |
| Where the design prompts and mocks live | Keep them in the public tree as an archive of decision records rather than deleting them; move the demo script and the review notes out of the repository. |
| The roadmap's internal deferral ledger | Delete it — roughly ninety internal ids citing a path that is not in the repository. This document and the epics carry the same content; the roadmap keeps its shipped and planned rows and six named gaps. |
| Required status contexts | Re-set them so the list matches reality: the notices and image-scan checks are advisory today by the release document's own admission; shard the browser suite, move the image smoke build to nightly, and give every job a timeout. |
| Documentation diagram targets | Adopt the per-document counts and paragraph-share caps as written, with every diagram read before it merges — a wrong diagram is worse than prose. |
| The test-runner major | Bump rather than dismiss; dismiss with a recorded reason only if the migration does not fit the window. |

## Verification

Per change: the command named in the issue, run with its test names quoted — a filtered Go run
exits zero when it matches nothing, and the database-, Docker- and Kubernetes-gated suites skip
green when their switch is absent, so a gated check also reports zero skips for those names.

Before a release is cut, on a clean tree:

| Gate | Covers |
|---|---|
| `make ci` | Build, unit tests, race, lint, file-size cap, coverage floor, and every documentation and citation guard |
| the Postgres lane (`WARDYN_TEST_PG`) | Migrations, chained audit storage, advisory locks, and the two-database federation gate |
| `make test-conformance-docker` and `-k8s` | Anything touching the runner or dispatch: both substrates answer the same contract |
| the Kubernetes SSO walk, with an image rebuild | Anything touching sign-in, launch or setup — waiting for its summary line, never a transient chip |
| the browser suite (`scripts/run-ui-e2e.sh`) | Every spec a change adds or touches; a spec that executes zero cases fails |
| `make release-check` | The release preconditions, run after the version bump, not before |

**Evidence is certified against the commit it ran on:** any commit after it re-opens the gates it
invalidated.

## Sources

- **[docs/design/hybrid-0.8.md](../hybrid-0.8.md)** — the hybrid design brief written during
  0.7.2: the two tiers as they exist today, the four phases, and the open questions. Phase 0 is
  now decided and the brief carries a status line saying so.
- **The 0.7 autonomy research** (notes kept locally, not in this repository). In summary: no
  managed-settings generator exists anywhere in the tree; today's autonomy levers are the two
  branches of the in-image launcher, one routing tool calls through an approval server and one
  skipping permission prompts entirely; the run-creation request enums are closed and carry no
  autonomy field; the approval state machine and the proxy's parked-connection path already exist;
  and the posture inputs — egress reach, grant power, confinement class — are already computed for
  the risk grade. A rubric is therefore a fold over values the system already has, plus one gate
  and one generated file, rather than new machinery.
- **The 0.7.x readiness-review ledger**, on the `review/0.7x-ledger` branch: twenty-eight rows, a
  review of the previous release plan, and a set of security notes. Its disposition for 0.8 is
  [#86](https://github.com/cjohnstoniv/wardyn/issues/86), and the branch gains a matching section
  so both sides read the same truth.
- **[Issue #57](https://github.com/cjohnstoniv/wardyn/issues/57)** — push content rules and held
  pushes, filed by a community contributor: the source of the requirements in
  [#87](https://github.com/cjohnstoniv/wardyn/issues/87), including the one place this design
  deviates from the issue (an oversize pack is refused rather than held) and why.
