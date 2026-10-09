# 0.9 hybrid: client mode, placement, credential delivery and the disk link

Status: **Approved 2026-10-09.** It turns decisions OD-6 to OD-12 into a buildable design and does not reopen them.
It supersedes [hybrid-0.8.md](../hybrid-0.8.md) and the #78/#79 sections of [0.8/PLAN.md](../0.8/PLAN.md).
Where this doc departs from the release plan's wording, the row is marked **deviation** and repeated in [Open questions](#13-open-questions).

## 1. Status and supersession

The pilot reply is [`docs/adoption/hybrid-09-pilot-reply.md`](../../adoption/hybrid-09-pilot-reply.md), the deployment's answer to hybrid-0.8's owner questions. Citations below link its headings.

### 1.1 hybrid-0.8 questions and decisions

| Item | 0.8 answer | 0.9 answer | Reason |
|---|---|---|---|
| O1 | `m′` pointed at the org (full daemon, borrowed identity) | **Client mode**: `wardyn-runnerd` registers outbound; the org schedules, enforces and audits (OD-6) | [Pilot reply, O1](../../adoption/hybrid-09-pilot-reply.md#2-o1--client-mode-and-the-objection-that-would-otherwise-sink-it): one scheduler; `m′` ships four MDM files and one shared OIDC client secret; the federation client has no policy fetch |
| O2 | Offline local runs continue; evidence buffered durably | No governed run is created while the link is down; an in-flight run is never torn down by a drop (OD-7) | [Pilot reply, O2](../../adoption/hybrid-09-pilot-reply.md#3-o2--a-local-run-while-the-org-is-unreachable): an unrecorded authorization is unacceptable; killing work on a blip is too |
| O3 | One audit chain per writer | **Dissolved**: one writer, the org | [Pilot reply, O1](../../adoption/hybrid-09-pilot-reply.md#2-o1--client-mode-and-the-objection-that-would-otherwise-sink-it): "only a question if there are two writers" |
| O4 | No placement field in phase 1; default open | No default between two eligible placements; local eligibility derived from the dispatch plan (OD-8) | [Pilot reply, proposed rule](../../adoption/hybrid-09-pilot-reply.md#22-our-proposed-rule-placement-eligibility-is-a-function-of-the-grant-set): derive from the grant set; refusals name the credential |
| O5 | Drive-as-source is "the disk link" (phase 3a) | Sync over the existing sftp channel is the disk link; drive-as-source is the shortcut (OD-9) | [Pilot reply, O5](../../adoption/hybrid-09-pilot-reply.md#4-o5--is-drive-as-source-enough-no-not-for-us): no shared storage, and none will be provisioned |
| D1 | Hybrid is `m′`-only | Superseded: the laptop runs no control plane, so there is no local admin to exclude | Client mode removes the local authority D1 guarded against |
| D2 | Placement is a property of a run | **Kept** | Unchanged |
| D3 | Drive-as-source first, sync second, reverse tunnel rejected | Sync first, drive-as-source second; reverse tunnel stays rejected | [Pilot reply, O5](../../adoption/hybrid-09-pilot-reply.md#4-o5--is-drive-as-source-enough-no-not-for-us) |
| D4 | No refusal is relaxed; sync adds one case and one action | **Kept**: sync adds one subsystem case and one action ([§9](#9-disk-link-od-9)) | [Pilot reply, O5](../../adoption/hybrid-09-pilot-reply.md#4-o5--is-drive-as-source-enough-no-not-for-us) agrees with the refusals |
| D5 | Evidence flows laptop → org | **Kept in substance**: the org writes every row; the runner only carries bytes | One writer makes direction structural |
| [Mint rule](../hybrid-0.8.md#52-what-the-enrolment-mint-pulls-and-what-it-must-never-pull) | The mint pulls an image and mints a device-local key, nothing else | **Kept** for `wardyn-runnerd`: the key pair is generated locally; no org credential is ever pulled | Same reasoning as `age.key` |
| [MDM envelope](../hybrid-0.8.md#9-ops-docs-and-the-mdm-envelope) | One file's contents change; no new files | Two binaries plus one registration token | [Pilot reply, O1](../../adoption/hybrid-09-pilot-reply.md#2-o1--client-mode-and-the-objection-that-would-otherwise-sink-it), point 3 |
| [Phases](../hybrid-0.8.md#11-phased-plan) | Phase 0–3 | Replaced by checkpoints CP1–CP3 on one tag (OD-10) | Single `v0.9.0` tag |

### 1.2 0.8/PLAN.md rules

| Section | 0.8 rule | 0.9 disposition |
|---|---|---|
| #78 | The laptop keeps its full daemon and gains a device credential and a forwarder | **Superseded** (OD-6): the laptop runs `wardyn-runnerd`, no Postgres, no OIDC client |
| #78 | Device credential: a `wdd_` bearer accepted only on device routes; never creates a run | **Shape superseded, rule kept**: a runner holds a key pair, not a bearer; it still never creates a run |
| #78 | Admin mints a single-use token; MDM delivers it; first boot exchanges it | **Superseded**: the token yields an `unclaimed` runner; only the owner completes the binding ([§3.1](#31-registration-claim-and-credential)) |
| #78 | Offline runs continue under the last ceiling; revocation answers `503` | **Superseded** by OD-7 ([§5](#5-offline-semantics-od-7)) |
| #78 | One chain per writer; ingest re-chains each device row | **Superseded**: spooled rows are chained at the org in arrival order with `data.observed_at` |
| #78 | Federate audit rows, not run records | **Superseded**: every local run is an org run record |
| #78 | Device routes mount unconditionally behind an optional store capability | **Kept** as the pattern for runner routes |
| #78 open | Single-use token per device; no console inventory; refuse new local runs on revocation | Single-use kept; console inventory added (H10/H11); revocation refuses everything for that runner |
| #79 | Empty `placement` resolves to the deployment's own executor | **Superseded** (OD-8): one eligible placement fills; two eligible with none named is refused |
| #79 | One resolution site, before the run row exists | **Kept** (#117) |
| #79 | A new column, never a reinterpreted runner target | **Kept** (#116) |
| #79 | The ceiling is a deny list; a profile denying every placement is refused at write | **Kept** (#114), plus the P3 term ([§8.3](#83-p3-components-times-placement)) |
| #79 | Capabilities intersect while unresolved; real placement once resolved; union for display only | **Kept** (#108), with runner-asserted capabilities marked |
| #79 | A placement-eligible drive has external enforcement; a label, not a refusal | **Kept** (#113); `host_path` drives also bind to a `runner_id` |
| #79 | Drive-as-source widens one drive row with a second backend | **Kept** (#112), positioned as the shortcut |
| #79 | An unresolvable placement capability answers `503` | **Kept**; an offline runner is a refusal, never a substitution |
| #79 open | Drive-as-source positioned as "half, named" | **Superseded** by OD-9 |

## 2. Architecture

### 2.1 Components

| Component | Where | Role |
|---|---|---|
| org `wardynd` | Cluster | The only scheduler, ceiling resolver, credential broker and audit writer |
| `remoteSubstrate` | In org `wardynd` | Implements `substrate.Substrate` for one runner over its stream |
| runner hub | In org `wardynd` | Accepts runner streams, tracks online state, holds pending actions |
| internal relay listener | In org `wardynd` | Hands relayed TLS connections to the internal server, tagged with `runner_id` |
| `via_org` egress | In org `wardynd` | Re-runs the proxy's checks and injects org-held credentials for local runs |
| `wardyn-runnerd` | Laptop (new `cmd/wardyn-runnerd`) | Holds the runner key, keeps one stream open, drives the local Docker substrate |
| local relay listener | In `wardyn-runnerd` | Bound to the Docker host-gateway address; forwards TLS bytes to the org verbatim |
| `wardyn-proxy` | Laptop, sibling container | Same image; the per-run sidecar shape is unchanged. Three local-mode additions: the durable spool ([§5.3](#53-spool-bound)), `via_org` forwarding and the no-hold rule for `via_org` destinations ([§7.3](#73-the-via_org-egress-contract-h12)) |
| `wardyn` CLI | Laptop | Talks to the org's public API; `wardyn runner claim`, `wardyn sync` |

- `cmd/wardyn-runner`, the dev-only harness, is untouched.
- k8s-through-remote is out of scope; a runner drives Docker only.

**Packages** (C-H owns the types and package boundaries):

| Package | Holds |
|---|---|
| `internal/runnerwire` | Frames, the `Conn` interface, the WebSocket transport, the loopback, and `runnerwire/runnertest` |
| `internal/runner/remote` | `remoteSubstrate` |
| `internal/placement` | The classification table: pure functions over `SandboxSpec`, `ProxyConfig` and grant rows. A leaf package; store-backed facts (secret ownership, site config) are passed in |
| `cmd/wardyn-runnerd` | The runner daemon |

### 2.2 Diagram

```text
 ORG CONTROL PLANE (cluster)                    DEVELOPER LAPTOP
+------------------------------------+         +------------------------------------+
| wardynd                            |         | agent sandbox                      |
|   orchestrator                     |         |   |  L0: the only route            |
|     +-- cluster substrate          |         |   v                                |
|     +-- remoteSubstrate --+        |         | wardyn-proxy ----> other dests     |
|                           v        |         |   |  TLS to the internal host      |
|   runner hub <=====================|=========|   v                                |
|     +-- internal relay listener    |  one    | wardyn-runnerd (local relay)       |
|     |      +--> wardynd internal   | stream  |                                    |
|     +-- via_org egress --> upstream|         |                                    |
+------------------------------------+         +------------------------------------+
   the stream is opened outbound by wardyn-runnerd
```

| From | To | Carries | Opened by |
|---|---|---|---|
| `wardyn-runnerd` | runner hub | The one stream: calls, events, byte streams | The runner, outbound |
| `remoteSubstrate` | runner hub | Calls for one runner | Org dispatch |
| agent sandbox | `wardyn-proxy` | All agent egress (L0) | The agent |
| `wardyn-proxy` | `wardyn-runnerd` | TLS to the org's internal hostname | The proxy |
| runner hub | internal relay listener | Relayed TLS, tagged `runner_id` | The hub |
| runner hub | `via_org` egress | Requests for `via_org` destinations | The hub |
| `via_org` egress | upstream | The credentialed leg | The org |
| `wardyn-proxy` | the internet | Every other destination | The proxy |

### 2.3 Trust boundaries

| Boundary | Protected by | Who is trusted on each side |
|---|---|---|
| Runner ↔ org stream | TLS to the public URL; per-connection proof of possession of the runner key | Org: itself. Runner: the developer's host |
| Laptop host | Nothing Wardyn controls: the developer is root | Everything on the laptop is developer-readable ([pilot reply, the objection](../../adoption/hybrid-09-pilot-reply.md#21-the-objection-on-a-developer-rooted-laptop-every-credential-is-readable)) |
| Sandbox ↔ laptop proxy | L0 gatewayless Docker network, unchanged | Binds the **agent**, not the developer |
| Laptop proxy ↔ org internal listener | End-to-end TLS (`ControlPlaneCAPEM`) through an opaque relay; run token; `runner_id` binding | The runner sees ciphertext only |
| `via_org` egress ↔ upstream | Org-side TLS, org-side checks | The credentialed leg never touches the laptop |

### 2.4 Reused unchanged

| Piece | Why it transfers |
|---|---|
| The `wardyn-proxy` image and the Docker driver's sibling-proxy shape | One engine, two containers, which is one laptop ([pilot reply, engineering inventory](../../adoption/hybrid-09-pilot-reply.md#5-engineering-inventory-what-we-verified-transfers-and-what-does-not)) |
| `wardyn-rec` upload through the local proxy | Already assumes no inbound reachability |
| `runner.Capabilities` / `substrate.ClassSupport` | Already the scheduler's vocabulary and fails closed |
| The redirect-refusing, outbound-only client shape of `federation.Client` | Right plumbing; the payload changes |
| `Server.createRun` as the one door to the run row | The runner-online and placement gates join it there |
| The SSH gateway, web terminal and UI gateway | They call `Runner.Attach` / `Runner.ExecStream`; the relay sits beneath |
| `RefStore` | Persists `ref → substrate name`; remote substrates are named `runner:<id>` |
| `test/conformance.Run` | Runs unchanged against `orchestrator.New(remoteSubstrate)` |

## 3. Phase 0 protocol

### 3.1 Registration, claim and credential

| Step | Actor | Call | Result |
|---|---|---|---|
| 1a | Owner, in *My runner* or the CLI | `POST /api/v1/me/runners/tokens` | Single-use `wdr_` token (32 random bytes) for the caller, shown once, `1h` TTL |
| 1b | Admin, for a named person | `POST /api/v1/runners/tokens {owner}` | Same token, `minted_for` the person, `72h` TTL (as `deviceEnrolmentTokenTTL`) so MDM can deliver it; audited |
| 2 | `wardyn-runnerd register --org <url> --token-file <f>` | Generates an Ed25519 key pair locally | Private key written `0600`; never leaves the host; the runner prints its key fingerprint |
| 3 | `wardyn-runnerd` | `POST /api/v1/runners/register {token, public_key, name}` (anonymous, per-peer limiter) | Runner row, state `unclaimed`, for every token |
| 4 | Owner, under their own session | `wardyn runner claim` (reads the local fingerprint), or *My runner* showing the fingerprint the runner printed → `POST /api/v1/me/runners/{id}/claim {fingerprint}` | `unclaimed` → `claimed` when the caller is the token's owner and the fingerprint matches |

- One state machine, one door: a self-minted token also yields `unclaimed`. The token is a short-lived bearer; the fingerprint claim proves the claimant holds the key.
- An admin may mint for a person but can never complete a claim. The claim route admits only the token's owner.
- An `unclaimed` runner may connect, but it is offered no run and relays nothing. It expires after `24h`.
- *My runner* lists a waiting `unclaimed` runner with its fingerprint, so a stolen token shows up as a runner the owner did not register.
- A runner accepts only runs owned by its owner. The org filters at placement; the runner re-checks at `CreateSandbox`, protecting the developer.
- The redemption shape is #1508's owner-redeemed invitation: only the signed-in target redeems; an admin sees metadata only.
- Token consume and runner create are two statements, as in `Server.handleDeviceEnrol`; a failed create means a re-mint.

**Credential shape.**

| Property | Value |
|---|---|
| Key | Ed25519, generated by `wardyn-runnerd` |
| At rest | `runner.key` `0600` in the runnerd state directory; `runner.json` holds `runner_id`, `org_url_sha256`, key fingerprint |
| On the wire | No bearer exists. Each connection signs a fresh server nonce |
| Org URL binding | `org_url_sha256` uses `federation.OrgURLSHA256`; a different URL is never contacted with this key |
| Blast radius of theft | The owner's own locally placeable runs on that runner, until revoked. Replaces residual #51's device shape |

**Revocation.**

| Trigger | Org effect | Runner effect |
|---|---|---|
| `DELETE /api/v1/runners/{id}` (security operator) or `DELETE /api/v1/me/runners/{id}` (owner) | Row `revoked`; live stream gets `REVOKED`; every run on it marked lost `runner_revoked` with the kill cascade (token deny, broker revoke) | Durable revoked mark; stops every governed proxy locally; refuses everything after |
| Revoked while offline | Same org effect, immediately | Learns it at the next `AUTH`, which is answered `REVOKED`, then acts as above |
| Re-registration | A new token makes a new runner identity | Old key is discarded |

**Routes (H10).** Mounted unconditionally behind an optional store capability that fails closed, as `Server.mountDeviceRoutes` is, so the authorization matrix walks them.

| Route | Auth | Console (H11) |
|---|---|---|
| `POST /api/v1/runners/register` | Anonymous, the token in the body, per-peer limiter | — |
| `GET /api/v1/runners/{id}/stream` | Proof of possession ([§3.2](#32-the-stream)) | — |
| `GET /api/v1/runners`, `DELETE /api/v1/runners/{id}` | `requireSecurityOperator` | Admin → Runners: owner, state, last seen, revoke |
| `POST /api/v1/runners/tokens` | `requireOperator` | Admin → Runners: mint for a person |
| `GET /api/v1/me/runners`, `DELETE /api/v1/me/runners/{id}` | The owner | *My runner*: status, allowed roots, revoke; empty state "Register a runner" |
| `POST /api/v1/me/runners/tokens`, `POST /api/v1/me/runners/{id}/claim` | The owner | *My runner*: register this laptop, claim |

The runner routes never publish a human identity, the rule `TestDevices_DeviceRoutesNeverTouchHumanIdentity` pins for device routes.

### 3.2 The stream

One outbound WebSocket (RFC 6455) to `GET /api/v1/runners/{id}/stream` on the public URL. Binary messages, one frame each. It honours `HTTPS_PROXY` and survives TLS-terminating ingresses.

**Handshake.**

1. Org sends `HELLO {versions, nonce (32 bytes), server_time, org_url_sha256}`.
2. Runner sends `AUTH {runner_id, version, sig, resume?}`, `sig = Ed25519(key, "wardyn-runner-v1" ‖ nonce ‖ runner_id ‖ org_url_sha256)`.
3. Org answers `AUTH_OK {session_id, resumed, ping_interval}`, or `REVOKED`, or closes with a reason.
4. A fresh session (`resumed=false`) runs the full sequence: org sends `PENDING` ([§3.7](#37-pending-actions)); the runner applies each, sends `STATE`, replays its spools ([§5.3](#53-spool-bound)), then sends `READY`.
5. A resumed session (`resumed=true`) replays the un-`ACK`ed seq'd frames and skips `PENDING`, `STATE` and `READY`.

**Frame header** (20 bytes, big-endian): `type u8 · flags u8 · reserved u16 · stream u32 · seq u64 · length u32`.

| Type | Dir | Stream | Seq'd | Payload |
|---|---|---|---|---|
| `HELLO` `AUTH` `AUTH_OK` | handshake | 0 | no | as above |
| `REVOKED` `GOAWAY` | org → runner | 0 | no | reason |
| `PENDING` | org → runner | 0 | yes | pending actions, oldest first |
| `STATE` | runner → org | 0 | yes | per-run local state ([§5.4](#54-reconnect-and-reconciliation)) |
| `READY` | runner → org | 0 | yes | none |
| `CALL` / `REPLY` | either | call's own id | yes | method, JSON args / result or typed error |
| `EVENT` | runner → org | 0 | yes | `caps`, `posture`, `waiting`, `agent_exit`, `action_result`, `spool`, `resident_erase` |
| `LEASE` | org → runner | 0 | yes | `{run_id, token_renewed_at, ends_at}` after each renew or extend |
| `OPEN` `OPEN_OK` | either | new id | no | kind (`pty`, `exec`, `exec_stderr`, `relay`, `output`), target |
| `DATA` `WINDOW` `CLOSE` `RESET` | either | byte stream | no | bytes / credit / half-close / abort code |
| `ACK` | either | 0 | no | cumulative seq received |
| `PING` / `PONG` | either | 0 | no | timestamp |

| Rule | Value |
|---|---|
| Stream ids | One id space shared by calls and byte streams: `0` control; even ids org-initiated; odd ids runner-initiated |
| Max frame | `1 MiB` for control frames; `64 KiB` for `DATA` |
| Backpressure | Initial credit window `256 KiB` per stream, `4 MiB` per connection; `WINDOW` carries increments. A writer blocks on credit; nothing is dropped |
| `GOAWAY` | The runner reconnects with a fresh session, never a resume |
| Resume | Seq'd frames sit in a `1 MiB` replay buffer per side until `ACK`ed. A reconnect inside `120s` sends `resume {session_id, last_seq}` and both sides replay |
| Not resumed | Byte streams (`pty`, `exec`, `relay`) are `RESET` on any reconnect. A PTY's tmux session survives in the sandbox, so attach reopens |
| Output | The `output` stream resumes by byte offset from a runner-side buffer ([§3.4](#34-rpc-set)) |
| Keepalive | `PING` every `15s`; three missed (`45s`) means the link is down |
| Online state | `online` from `AUTH_OK` until the link is down; create refuses the instant it is down |
| Reconnect | Backoff `1s` doubling to `60s`, with jitter |
| Org restart | A fresh session; pending actions are durable; in-flight calls fail and callers retry |

`runner.disconnect` is written only when a session is not resumed within `120s`, so a blip writes no row.

### 3.3 In-memory loopback transport

| Item | Design |
|---|---|
| Interface | `runnerwire.Conn` with `ReadFrame` / `WriteFrame` / `Close`; the WebSocket transport and the loopback both implement it |
| Loopback | `runnerwire.Loopback() (org, runner Conn)` with fault injection: delay, drop the link, sever mid-frame |
| Fake runner | `runnerwire/runnertest` serves the runner side over a supplied `substrate.Substrate` (Docker or a fake) |
| Use | C-H ships it final; H3–H6 and H12 build and test against it in parallel; H2 swaps in WebSocket |
| Parity | `test/conformance.Run` runs over both, so a lane cannot depend on transport behaviour |

### 3.4 RPC set

The org's `remoteSubstrate` mirrors `substrate.Substrate`. `Name()` is local and returns `runner:<runner_id>`.

| Method | Wire | Notes |
|---|---|---|
| `Classes` | No call: served from the last `caps` event | Offline answers `runner.ErrRunnerOffline` |
| `CreateSandbox` | `CALL` with the spec minus `OnWaiting` / `ExecOutput` | Idempotent by `RunID`. `waiting` events call `spec.NotifyWaiting` on the caller's goroutine, in order, before `REPLY` returns. Never carries a `runner_resident` value: a placeholder names the grant |
| `Exec` | `CALL` | Returns the agent exec id |
| `Wait` | Long `CALL` | A link drop answers `ErrRunnerOffline` (transient). The runner also sends a durable `agent_exit {ref, code, observed_at}` |
| `Attach` | `CALL` returns a `pty` stream id | `Resize` is a `CALL` naming the stream. `Close` resets the stream only |
| `ExecStream` | `CALL` returns `exec` and `exec_stderr` stream ids | Stdin half-close is `CLOSE`; stderr is drained concurrently, as `runner.ExecSession` requires |
| `Status` / `AgentStatus` | `CALL` | Offline answers `ErrRunnerOffline`, never a guessed state |
| `StopSandbox` | `CALL`; offline → pending `end` | |
| `KillSandbox` | `CALL`; offline → pending `kill` | Returns `runner.ErrPendingOnRunner` while queued |
| `ExecOutput` | Runner opens `output` with offset `0` | Runner buffers up to `8 MiB` per run on disk; org copies into `spec.ExecOutput` with `BeginOutputDrain` semantics; reconnect resumes at the last acked offset |

**Optional capability interfaces in 0.9** (closed list; C-H pins it in a test):

| Interface | 0.9 | Why |
|---|---|---|
| `runner.SandboxEnder` | **implemented** | Run Ends keep files (OD-7) |
| `runner.ProxyStopper` | **implemented** | Lost-run and lapse containment |
| `runner.ProxyReviver` | **implemented** | Revive on reconnect (OD-7). The revive config is the org's stored copy, never read back from the runner; it passes the same strip as dispatch ([§6.3](#63-dispatch-plan-classification)), and the runner re-applies resident values from its keystore |
| `runner.SandboxStarter` | **implemented** | Revive after a laptop reboot |
| `runner.SubstrateProber` | **implemented, local** | Answers link state; no call |
| `runner.DriveProber` | **implemented** | `host_path` drives bound to the runner |
| `runner.OutputRecoverer` | **implemented** | Reads the runner-side output buffer |
| `SweepOrphanedSandboxes` | **implemented** | Org sends the live run-id set (a func cannot cross the wire); skipped while offline |
| `runner.Freezer` | not supported | `Capabilities.Freeze` false, so `run_pause.go` never pauses a local run |
| `runner.ActivitySampler` | not supported | "No reading" is neither idle nor gone; idle pause never fires locally |
| `runner.ImageChecker` | not supported | Callers already treat absence as "trust the cache" |
| `runner.ImageRemover` | not supported | Laptop image hygiene is the developer's |
| `runner.DriveReclaimer` | not supported | The one irreversible verb never crosses a relay |
| `runner.FitChecker` | not supported | Kubernetes-only; capacity comes from the advertisement |

**Runner-only calls**: `DeliverResident` and `EraseResident` ([§7.4](#74-runner_resident-and-device-posture)).

**New sentinels in `internal/runner`**: `ErrRunnerOffline` (transient; the caller retries or refuses) and `ErrPendingOnRunner` (queued as a pending action; not a failure).

**`REPLY` errors** are `{code, message}`. The code list is closed, and `remoteSubstrate` maps each code to its sentinel so `errors.Is` works org-side:

| Code | Sentinel |
|---|---|
| `runner_offline` | `runner.ErrRunnerOffline` |
| `pending_on_runner` | `runner.ErrPendingOnRunner` |
| `sandbox_gone` | `runner.ErrSandboxGone` |
| `exec_unsupported` | `runner.ErrExecStreamUnsupported` |
| `end_unsupported` | `runner.ErrEndUnsupported` |
| `revive_unsupported` | `runner.ErrReviveUnsupported` |
| `never_started` | `runner.ErrExecNeverStarted` |
| `refused` | None: a runner-side refusal (owner, roots, capacity), carried with its reason |
| `internal` | None: any other failure |

### 3.5 Capacity advertisement

The `caps` event carries the runner's Docker `ClassSupport` plus:

| Field | Meaning | Consumer |
|---|---|---|
| `cpu_millis_max` | Most CPU one local run may get | Placement-aware `boundResources` (A-S3); preview's per-placement caps |
| `memory_mib_max` | Most memory one local run may get | Same |
| `allowed_roots` | Owner-set local path roots | Local-path eligibility ([§6.7](#67-local-paths)) |
| `version` | `wardyn-runnerd` version | Inventory; refusal of an unsupported protocol version |

All of it is `runner_asserted`.

### 3.6 Job claim

**The routing seam.** `Orchestrator.New(substrates...)` is fixed at boot and `substrateFor` routes by class, so the carrier is new:

| Element | Design |
|---|---|
| Carrier | `SandboxSpec.RunnerID string`; empty means the cluster |
| Registration | The runner hub calls `Orchestrator.Add(substrate)` on claim and `Orchestrator.Remove(substrate)` on revoke. At boot it adds every claimed runner, so `RefStore` rehydration by name finds `runner:<id>` after a restart. An offline substrate answers `ErrRunnerOffline` and is skipped by `Capabilities` |
| Routing | `CreateSandbox` with `RunnerID` set routes to the substrate named `runner:<id>`, or fails `runner.ErrRunnerOffline`; with it empty, class routing is unchanged |
| Iteration | `Capabilities` and `SweepOrphanedSandboxes` iterate a snapshot of the set and skip remote substrates |
| Refs | Remote refs are prefixed `runner:<id>/`, so `Orchestrator.subForRef` never takes its single-substrate fallback for one |

| Step | Who | What |
|---|---|---|
| 1 | `Server.dispatchRun` | Runs the dispatch strip from `internal/placement` on the built spec, before the orchestrator; refuses before sending anything |
| 2 | Orchestrator | Routes by `SandboxSpec.RunnerID` to that runner's `remoteSubstrate` |
| 3 | Runner | Checks owner, `allowed_roots`, capacity, that every `runner_resident` grant has arrived, and that no pending action fences the run |
| 4 | Runner | Creates the sandbox and replies with the prefixed ref |

A dispatch whose runner drops before step 4 waits up to the dispatch timeout, then fails the run `runner_offline` with no sandbox.

### 3.7 Pending actions

| Kind | Queued by | Applied as |
|---|---|---|
| `kill` | `killTeardownTail` while offline | `KillSandbox` |
| `end` | Lease end or `StopSandbox` while offline | `EndSandbox` |
| `stop_proxy` | `stopLostSandbox` while offline | `StopProxy` (usually already done by the lapse rule; idempotent) |

- Stored in `runner_pending_actions`, folded into migration `0146`, which C-H already lands ([Open questions](#13-open-questions), Q5).
- Revocation is not a pending action: a revoked runner cannot authenticate, so `AUTH` answers `REVOKED`.
- **Order on a fresh session**: `AUTH_OK` → `PENDING` → runner applies each, emitting `action_result` → `STATE` → spool replay → `READY`.
- The runner fences each run with an unapplied action: no relay, no call. The org fences too and refuses a relay for that run with `runner_action_pending`.
- `killTeardownTail` treats `runner.ErrPendingOnRunner` as queued, not failed. The kill answers `202` with `teardown: "pending_runner"`; token deny and broker revoke run at once.

## 4. Relays

### 4.1 Streams (H4)

| Surface | Calls | Over the stream |
|---|---|---|
| Web terminal, attach holder | `Runner.Attach` | `pty` stream; the runner bridges its local Docker exec hijack |
| SSH gateway shell, exec, sftp, `-L` | `Runner.ExecStream` | `exec` + `exec_stderr` streams |
| UI gateway, run files, output snapshot, run resources | `Runner.ExecStream` | Same |
| Sandbox-not-ready status | `SandboxSpec.OnWaiting` | `waiting` events |

Every surface keeps its own audit rows and principal checks; the relay adds `data.relay` ([§4.4](#44-audit)).

### 4.2 Internal hops (H5)

| Element | Design |
|---|---|
| Proxy config | Keeps `ProxyConfig.ControlPlaneURL` and `ControlPlaneCAPEM` exactly as dispatched |
| Name resolution | `wardyn-runnerd` passes the Docker driver an `ExtraHosts` entry mapping the internal hostname to `host-gateway` |
| Local listener | Bound to the host-gateway address only, port of the internal URL; forwards bytes only to its own stream, never elsewhere |
| Org side | Each `relay` stream becomes a `net.Conn` from the internal relay listener; the internal `http.Server`'s `ConnContext` tags it with `runner_id` |
| TLS | Terminates at the org's internal listener; no CA key on the laptop; `:8443` is never published |
| Precondition | `runners.enabled` refuses to turn on while `ControlPlaneURL` is plain HTTP |

### 4.3 `runner_id` binding in `internalAuth`

Two layers, in this order:

1. The internal relay listener's route filter enforces the closed list of relayed routes below, before either middleware runs. That covers `/internal/groundtruth`, which sits behind `internalAuthGroundtruth`, not `internalAuth`.
2. After token verification and `refuseTerminalRun`, `internalAuth` reads the relay tag and applies the binding.

| Request arrives | Run placement | Outcome | Reason |
|---|---|---|---|
| Relayed by runner R | `local` on R | Continue to the route's own rule below | — |
| Relayed by runner R | `local` on another runner, or `remote` | `403` | `relay_runner_mismatch` |
| Not relayed | `local` | `403` | `relay_required` |
| Not relayed | `remote` | Unchanged | — |

**Relayed routes** (the closed list, enforced at the relay listener; a new `/internal/*` route fails a test until classified):

| Route | Relayed rule | Refusal reason |
|---|---|---|
| `POST /internal/decisions` | Allow | — |
| `POST /internal/activity` | Allow | — |
| `POST /internal/approvals`, `GET /internal/approvals/{id}`, `POST …/expire` | Allow | — |
| `POST /internal/token/renew` | Allow; the org then sends `LEASE` | — |
| `GET /internal/injection/{grantID}` | Only a grant with `delivery = own` | `delivery_via_org`, `delivery_runner_resident` |
| `POST /internal/credentials/mint` | Only a grant with `delivery = own` | Same |
| `POST /internal/via-org/{grantID}/…` (new) | Only `delivery = via_org` on this run | `delivery_not_via_org` |
| `PUT /internal/recordings/{runID}[/parts/{part}]` | Allow; stored with `source: runner_asserted` | — |
| `PUT /internal/scan-results/{runID}` | Refuse: trusted output is cluster-only | `relay_route_refused` |
| `PUT /internal/sso-token/{runID}` | Refuse: container-login is cluster-only | `relay_route_refused` |
| `POST /internal/groundtruth` | Refuse at the listener: no host sensor on a laptop | `relay_route_refused` |
| Any route not listed | Refuse at the listener | `relay_route_refused` |

`internalAuth`'s refusals write `authz.denied` through the existing `auditInternalDenied` shape, with `runner_id`. A listener refusal writes the same action with `runner_id` and the path, since no run claims exist yet.

### 4.4 Audit

- Every row a relayed request writes carries `data.relay = "runner:<id>"`.
- **Deviation**: the release plan says `data.via`. That key is taken twice already: `egress.DecisionLog.Via` (`"direct"` or `"upstream-proxy"`) and `types.DelegationVia` (portal delegation, an object). A third shape breaks SIEM rules.
- Rows from org-side `via_org` decisions carry `data.relay` too, and keep the proxy's own `via`.

## 5. Offline semantics (OD-7)

### 5.1 Rules

| Situation | Behaviour |
|---|---|
| Create with the runner's link down | Refused `runner_offline` before the run row exists; preflight mirrors it |
| Link drops mid-run | Nothing is torn down; the agent and proxy keep running |
| Proxy while offline | Enforces its cached policy; raises nothing (a hold cannot be raised, so a first-use host is denied); spools every decision |
| Link down past the lapse | The runner stops the proxy itself, keeps agent and files; no local decision is made |
| Kill, Ends, stop issued while offline | Durable pending actions, applied first on reconnect ([§3.7](#37-pending-actions)) |
| Revocation | The runner stops every governed proxy; the org marks runs lost `runner_revoked` and runs the kill cascade |

### 5.2 The lapse rule on the runner

| Item | Design |
|---|---|
| Input | `LEASE {run_id, token_renewed_at, ends_at}` from the org after each renew |
| Deadline | `token_renewed_at + runTokenLapseAfter` (`1h5m`), the same constant `Server.sweepLapsedRunTokens` uses |
| Clock | Computed as a duration from the `LEASE` receipt on the runner's monotonic clock, corrected by `HELLO.server_time`, so clock skew cannot stretch it |
| Action | Link down and deadline passed → `StopProxy` through the local Docker `runner.ProxyStopper`; recorded locally for `STATE` |
| Never | The runner never grants, approves, mints or widens anything offline |

### 5.3 Spool bound

- The proxy's `decisionSink` today drops and counts on a failed post.
- For a local run, a decision that cannot be posted is written to a durable spool on a runner-owned per-run volume.
- Replay never goes through the relay or `/internal/decisions`. After a lapse the run token is dead (`Identity.Verify` refuses it) and a kept run is refused `run_kept`, so that path cannot replay the case OD-7 exists for.

| Replay step | Design |
|---|---|
| Reader | The runner reads the per-run spool volume |
| Channel | `spool` events on the runner's own proof-of-possession stream, oldest first |
| Cursor | Acknowledged per run with the shared cursor package extracted from `federation.Forwarder` (X-cursor) |
| Org ingest | New `decisionIngestFromRunner`, for a running, lost or kept run alike; actor `runner:<id>`, `data.spooled: true`, `data.observed_at` |
| Order | Replay completes before `READY` on a fresh session |

| Bound | Value |
|---|---|
| Time | At most the lapse window, since the proxy is stopped after it |
| Records | `50,000` per run |
| Bytes | `32 MiB` per run |
| On overflow | Fail closed (Q3): the proxy refuses new egress until the spool drains. The first replayed row is a synthetic `egress.deny` with `rule_source` `builtin:spool-full` carrying `count` |

### 5.4 Reconnect and reconciliation

`STATE` reports per run: sandbox present, agent state and exit code, `proxy_stopped_at`, spool depth and drops, resident grants held. All of it is `runner_asserted`.

| Org-side state | Runner reports | Reconciliation |
|---|---|---|
| Running, token not yet lapsed | Proxy up | Nothing; `LEASE` resumes |
| `Server.sweepLapsedRunTokens` marked it lost `outage` while offline | `proxy_stopped_at` set | The pending `stop_proxy` acks; `run.lost` already written; revivable by the owner |
| Lost `outage`, and the proxy did **not** stop | Proxy up | Apply `stop_proxy` first; record `containment: "late"` |
| Headless run, kept as lost `outage` | Agent exited at `observed_at` | The `agent_exit` event finalizes it with the reported code, marked `runner_asserted`. The completion watcher skips kept runs, so nothing else would |
| Killed while offline | Anything | Pending `kill` applies before any relay |

`Server.loseRun` changes for local runs:

- `Server.stopLostSandbox` through an offline `remoteSubstrate` returns `runner.ErrPendingOnRunner`. `loseRun` records `containment: "pending_runner"`, outcome `success`, `kept: true`.
- `Server.lostRunKeepable` keeps only interactive runs today. For `placement = local` it also keeps headless runs, because OD-7 forbids teardown by a link drop (Q2).

### 5.5 `observed_at`

- Spooled rows arrive as `spool` events ([§5.3](#53-spool-bound)) and are chained at the org in arrival order, never back-dated.
- `data.observed_at` carries `egress.Request.Time`, and `data.spooled` is `true`.
- The chain timestamp stays the org's, so #1826's clock-jump logic is untouched.

## 6. Placement (OD-8)

### 6.1 The request field

| Field | Where | Values |
|---|---|---|
| `placement` | `client.CreateRunRequest` (#116) | `local`, `remote`, or empty |
| `runner_id` | `client.CreateRunRequest` | Optional UUID; replaces #116's `device_id` |
| `placement`, `placement_filled`, `runner_id` | `AgentRun`, migration `0138` | Recorded once |
| `confinement_source` | `AgentRun`, migration `0138` | `substrate` or `runner_asserted` |
| `--placement`, `--runner` | `wardyn run` (#109) | Map to `placement` and `runner_id` |

### 6.2 `placementFor`

`placementFor(requested, runnerID, eligible, denied)` is pure (#117). It runs once inside A-L1a's fold, which preview, preflight and create share.

| Request | Eligible set | Outcome | Reason |
|---|---|---|---|
| empty | only `remote` | `remote`, `placement_filled=true` | — |
| empty | only `local` | `local`, `placement_filled=true` | — |
| empty | both | `422` | `placement_required` |
| empty | none | `422` naming each placement's refusal | first refusal's reason |
| `remote` | `remote` not eligible | `422` | `placement_unavailable` |
| `local` | `runners.enabled=false`, or no claimed runner | `422` | `placement_unavailable` |
| `local` | runner offline | `422` | `runner_offline` |
| `local` | ceiling `deny_placements` has `local` | `403` + `authz.denied` | `placement_denied` |
| `local` | the plan carries a field local placement refuses | `422` naming the field | `placement_credential` |
| `local` | trusted-output run | `422` | `placement_trusted_output` |
| `local` | self-defined component and P3 term off | `403` naming the component | `placement_component_self_defined` |
| `local` | demanded class or capability not offered | `422` | `placement_capability` |
| `local` | local path not bound to this runner, or outside its roots | `422` naming the path | `placement_local_path` |
| `local`, no `runner_id` | more than one claimed runner online | `422` | `runner_ambiguous` |
| `local`, no `runner_id` | exactly one online | that runner, `runner_id` filled | — |
| `runner_id` set | runner not the caller's, or unclaimed | `404` | `runner_not_found` |

- Server-originated launchers (`newStepRun`, `newProbeRun`, scans) never name a placement; trusted-output makes them `remote`.
- `deny_placements` is a deny list: absent, including for a member with no assigned profile, means local is permitted. P3's default deny still applies (Q6).
- Placement-related refusals register their preflight mirror in the same change (`TestPreflightMirrorsLaunchGates`).

### 6.3 Dispatch-plan classification

`localPlaceable(plan)` reads the **dispatch plan**: the resolved spec, its grants and the site config dispatch will use, not just `EligibleGrants`.

| Class | Meaning | Local rule |
|---|---|---|
| `own` | The launching principal's own material | Allowed; stored grants persist `OwnerOnly=true` |
| `operator` | The operator namespace, a `shared` component secret, or org network configuration | OD-12 mode, else refused naming the field |
| `brokered` | Minted by the org's broker from an org-held identity | `via_org` or refused, never `runner_resident` |
| `platform` | Minted for this run by the platform | Allowed |

**`runner.SandboxSpec`**

| Field | Class | Local rule |
|---|---|---|
| `SecretEnv` (env_secret grant) | `own` if `OwnerOnly`, else `operator` | `own` allowed; `operator` per OD-12 (`runner_resident` or refuse) |
| `SecretEnv` (resident Bedrock role credentials) | `own` for the person's own `bedrock_sso`; `operator` otherwise | Per OD-12 (`runner_resident` or refuse) |
| `ManagedFiles` (managed settings) | `platform` | Allowed |
| `ManagedFiles` (`AgentOwned` file_secret) | `own` if `OwnerOnly`, else `operator` | `own` allowed; `operator` per OD-12 |
| `Mounts` (operator-authored, incl. host-mode `~/.aws`) | `operator` | Refused `placement_capability`: a cluster host path means nothing on a laptop |
| `Mounts` (`MemberAuthored` `local_dir`) | `own` | Allowed only when bound to this `runner_id` and inside `allowed_roots` |
| `Drive` (`host_path`) | `own` | Allowed only when bound to this `runner_id` |
| `Drive` (`k8s_pvc`, `k8s_pvc_static`, `docker_volume` of the org) | `operator` | Refused, unless #112's alternate arm resolves for local |
| `Image` (incl. a built workspace image in the org registry) | `own` | Pulled with the runner's own registry credentials, or refused `placement_capability`; never the org's pull secrets |
| `Env`, `Labels`, `UserMountRoots` | exempt (not credential-bearing) | Sent as dispatched; `UserMountRoots` is re-checked against `allowed_roots` ([§6.7](#67-local-paths)) |

**`runner.ProxyConfig`**

| Field | Class | Local rule |
|---|---|---|
| `RunToken` | `platform` | Allowed; bounded by the `runner_id` binding |
| `ControlPlaneCAPEM` | `platform` (public) | Allowed |
| `MITMCACertPEM` / `MITMCAKeyPEM` | `platform` (per run) | Allowed; reading it gains MITM of one's own sandbox only |
| `Injection` (stored api_key, own namespace) | `own` | Allowed; `OwnerOnly=true` |
| `Injection` (operator namespace or `shared` component) | `operator` | Per OD-12 (`via_org`, `runner_resident` or refuse) |
| `Injection` (person's model-provider key, own Claude sign-in) | `own` | Allowed |
| `Injection` (captured AWS SSO, `credential_source` `per_user`) | `own` | Allowed: the person's own sign-in |
| `Injection` (captured AWS SSO `shared`, Bedrock bearer) | `brokered` | Per OD-12 ([§7.2](#72-per-kind-bounds)) |
| `GitGrants` (GitHub App installation token) | `brokered` | `via_org` or refuse |
| `PATGrants` (`per_user` stored PAT) | `own` | Allowed |
| `PATGrants` (`shared` `CredentialSource`) | `operator` | `via_org` (broker-route shape) or refuse |
| `BrokeredPATGrantIDs` | follows `PATGrants` | Same as its grant |
| `ADOGrant` (minted PAT from a captured Entra sign-in) | `brokered` | `via_org` or refuse |
| `ADOGrant` (`own_pat` token mode) | `own` | Allowed |
| `AzureGates` (Azure Foundry, the person's own Entra sign-in) | `own` | Allowed |
| `AzureGates` (Azure Foundry, any shared capture) | `brokered` | Per OD-12 ([§7.2](#72-per-kind-bounds)) |
| `UpstreamProxyURL` (from `upstream_proxy_secret_ref`) | `operator` | Never sent; the runner's own upstream-proxy config applies |
| `TrustedCAPEM`, `InternalHosts`, `UpstreamProxyNoProxy`, `LLMUpstreams` | `operator` (org network) | Not sent; the runner's own config applies. Destinations needing them take `via_org` |
| `MITMHosts` | `operator` (org network) | Not sent as dispatched; the laptop copy holds the hosts of `own` and `runner_resident` injections and of every `via_org` destination (the laptop proxy must terminate that TLS to forward plaintext). Operator-only hosts are dropped |
| `Policy.EligibleGrants` | `operator` for any grant not delivered `own` / `runner_resident` | The laptop-bound `Policy` copy drops those grants; a `via_org` grant keeps only its `grant_id` and host. Operator secret names never reach the laptop |
| `Policy.LLMInspection.WorkspaceSecretNames` (resolved values) | `operator` | Stripped; the org-side `via_org` scan may still use them |
| `ControlPlaneURL`, `MITMLLM`, `LLMChannelHosts`, `LLMUnavailableDetail`, `Unattended`, `Attribution`, the rest of `Policy` | exempt (not credential-bearing) | Sent as dispatched |

**Grant kinds with no `SandboxSpec` field** (minted through the proxy's local mint route):

| Kind | Class | Local rule |
|---|---|---|
| `ssh_key` (own key) | `own` | Allowed |
| `ssh_key` (operator key) | `operator` | Per OD-12 (`runner_resident` or refuse) |
| `git_pat` helper shape (own) | `own` | Allowed |
| `git_pat` helper shape (operator) | `operator` | `runner_resident` or refuse |
| `cloud_sts` | `brokered` | Refused, not configurable |

- **Closed switch**: `TestDispatchPlanFieldsClassified` walks `runner.SandboxSpec` and `runner.ProxyConfig` by reflection. An unclassified field fails. The exempt rows above are the named exempt set.
- **One table, three chokepoints**, all in `internal/placement`:

| Chokepoint | Where | What it does |
|---|---|---|
| Create | `localPlaceable` inside A-L1a's fold | Refuses local placement, naming the field |
| Dispatch | The dispatch strip in `Server.dispatchRun`, before the orchestrator | Strips or refuses on the built spec |
| Revive | Before `ReplaceProxy(cfgJSON)` | `Server.refreshDeploymentConfig` re-reads the upstream proxy, trusted CA and internal hosts from current config, so the same strip runs again; the runner re-applies resident values from its keystore |

- Every `own` stored-secret grant of a local run is persisted `OwnerOnly=true` in `Server.persistRunGrants`, as its Azure DevOps arm already forces (#1429).
- Create refuses any `own` stored-secret grant, of every kind, when `ownsSecretMemoized(owner, name)` is false.

### 6.4 Trusted-output runs

| Run | Why cluster-only |
|---|---|
| `source scan` | Its scan facts drive the workspace profile the org trusts |
| `workspace record`, `workspace verify` | Their capture becomes policy |
| `harness login` (container-login, AWS SSO capture) | It writes a credential into the org's store |
| Site-config probe runs | The deployment's own diagnostic |

The predicate is `reservedRunTasks` plus `source scan` plus probe runs. Refusal reason `placement_trusted_output`.

### 6.5 Capability intersection (#108)

| Phase | Rule |
|---|---|
| Placement unresolved (preview, profile editor advisory) | `intersectCapabilities` over every eligible placement; ephemeral-disk enforcement keeps the weakest-wins rank |
| Placement resolved | `placementCapabilities(ctx, placement, runnerID)`: real answer, structural refusal, or unknown |
| Unknown (runner offline, stale caps) | Refusal (`runner_offline`), never a substituted answer |
| No runner wired at all | `resolveEnforcedConfinement` no longer skips its membership check behind `if s.cfg.Runner != nil` |
| Display | `/healthz` and the setup runner card keep the union; remote substrates are excluded from the deployment `Capabilities` |
| Unsupported interfaces | A demand that needs one ([§3.4](#34-rpc-set)) is refused, not dropped |

### 6.6 `confinement_source: runner_asserted`

- A local run records `confinement_source = runner_asserted`. A remote run records `substrate`.
- Self-reported for a local run: proxy decisions, recordings, `ExecOutput`, exit codes, advertised `Capabilities`, posture.
- The CC3 blast-radius floor still applies (Q7):
  - For `own` and `via_org` credentials it is evaluated against the runner-asserted classes.
  - For a `runner_resident` credential a runner-asserted class never satisfies it: the floor is unmet, and placement is refused `runner_posture_unmet`, naming the posture. The floor protects an org-held value on a host whose class nobody can verify.
- Residuals #50–#52 are re-scoped by D-TM.

### 6.7 Local paths

| Item | Rule |
|---|---|
| `local_dir` workspace sources | Carry the `runner_id` they were registered for (migration `0139`); placeable only there |
| `host_path` user drives | Carry a `runner_id` (migration `0143`, #113); placeable only there |
| Device-side check | The runner intersects `SandboxSpec.UserMountRoots` with its owner-set `allowed_roots` at bind |
| Residual | The writable bind lets agent-authored content reach files the developer later executes, as sync does ([§12](#12-threat-model-deltas-audit-actions-refusal-reasons)) |

### 6.8 Console states (for #111 and D-UI-a)

The five Placement control states each come from one server answer, so the card and the refusal never disagree.

| State | Server answer |
|---|---|
| No runner registered | `placement_unavailable` |
| Runner offline | `runner_offline` |
| Ceiling denies | `placement_denied`, naming the profile |
| Grant set refuses | `placement_credential`, naming the field; reveals Access and focuses the owning entry |
| Eligible | No refusal; several runners list by name, and none chosen with several online is `runner_ambiguous` |

Run detail and the header show `placement`, the runner's name, `placement_filled` and `confinement_source`.

## 7. Credential delivery (OD-12)

### 7.1 Modes

| Mode | Where the value lives | Who can read it | Available when |
|---|---|---|---|
| `own` | Wherever the person's own credential already lives | The person | Always |
| `via_org` | Only in the org | Nobody on the laptop; the developer can *use* it for the run's life | The stream is online and the destination fits the `via_org` shape |
| `runner_resident` | The runner's OS keystore, then the local proxy or sandbox | The developer (root) | The runner's reported posture meets the policy's requirement |
| `refuse` | Nowhere | — | Default for every org-held class |

### 7.2 Per-kind bounds

| Class key | Credential | Allowed modes | Why |
|---|---|---|---|
| `oauth_subscription` | OAuth subscription | `own` only, not configurable | It is a person's sign-in |
| `api_key` | Header-injected `api_key` (operator or `shared`) | `via_org`, `runner_resident`, `refuse` | Header injection works at either end |
| — | Captured AWS SSO or Azure Foundry token from the person's own sign-in (`credential_source` `per_user`) | `own`, not configurable | It is the person's sign-in |
| `bedrock_bearer`, `aws_sso_bearer` | Bedrock bearer; captured AWS SSO bearer from a `shared` capture | `via_org`, `runner_resident`, `refuse` | Header-shaped; `via_org`-eligible |
| `azure_foundry_token` | Azure Foundry token from any shared capture | `via_org`, `runner_resident`, `refuse` | Header-shaped; `via_org`-eligible |
| `bedrock_role_credentials` | SigV4 / Bedrock role credentials | `runner_resident`, `refuse` | They sign in-process |
| `env_secret`, `file_secret`, `ssh_key` | The three resident kinds | `runner_resident`, `refuse` | They live in the sandbox |
| `git_pat_broker` | `git_pat` broker route (`/wardyn/git/<host>/`) | `via_org`, `runner_resident`, `refuse` | The route shape can be relayed |
| `git_pat_helper` | `git_pat` credential-helper shape | `runner_resident`, `refuse` | The helper hands it to git |
| `github_token` | Brokered GitHub App installation token | `via_org`, `refuse` | Resident would bypass branch namespace and push-content review |
| `ado_minted_pat` | Azure DevOps minted PAT | `via_org`, `refuse` | Resident would bypass the ADO REST gate and capability hold |
| `cloud_sts` | `cloud_sts` | `refuse`, not configurable | Needs SPIRE attestation no laptop sandbox has |
| — | `upstream_proxy_secret_ref` | not a class | The runner's own upstream config applies (OD-8) |

- OD-12 bars `runner_resident` only for `github_token`, `ado_minted_pat` and `cloud_sts`. The header-shaped classes keep it.
- A resident value is static for the run, so a short-lived token stops working at its expiry. Re-delivery is a follow-on.
- H7 rule: a class is eligible when its best allowed mode is available for this runner. Otherwise local placement is refused, naming the credential and the missing mode or posture.
- #475 (per-person Azure DevOps), rewritten: the human creates runs at the org; ADO capture stays org-side; brokered ADO is `via_org` or `refuse`.

### 7.3 The `via_org` egress contract (H12)

| Aspect | Rule |
|---|---|
| Scope | The org relays only destinations with a `via_org` injection rule or broker route for this run. Everything else is dialed from the laptop |
| Path | Laptop proxy → `POST /internal/via-org/{grantID}/…` through the relay; one HTTP request per sandbox request, bodies streamed both ways |
| Engine | An in-process `internal/egress/proxy` instance built from the run's stored config, holding only the `via_org` rules |
| Destination | The upstream host and scheme are read from the grant's injection rule or broker route, never from the request. A request whose `Host` or authority differs is refused `via_org_destination_refused` |
| TLS | The org always dials TLS itself; cleartext is refused |
| Holds and spool | The laptop proxy raises no hold and writes no spool row for a `via_org` destination; the org owns both |
| Per request | Run token (`wardyn-internal` audience), `refuseTerminalRun`, relay `runner_id` == the run's runner |
| Per run | `16` concurrent streams, `8 GiB` total, `10m` idle timeout (constants, not knobs) |
| Closes on | Run end, lease lapse, runner offline |
| Decisions | Written org-side with `data.relay`; the laptop proxy's own rows are `runner_asserted` |
| Laptop TLS | The laptop proxy terminates sandbox TLS with its per-run CA, as the cluster proxy does |
| Credentialed leg | Its TLS terminates at the org, so the laptop cannot redirect or downgrade it |
| Plaintext | The sandbox request's plaintext crosses the stream inside org TLS; the laptop proxy saw it |

**Every check re-run org-side** (authoritative; the laptop's are `runner_asserted`):

| Check | Implementation reused |
|---|---|
| Allowlist and deny lists | The run's `RunPolicySpec` |
| Method, path and query pins | `egress.InjectionRule.AllowsInjection` |
| Sandbox-supplied credentials stripped | `stripSandboxCredentials` |
| Internal-host exclusion and IP guard | The unconditional IP guard; the org's `InternalHosts` never lift for a `via_org` request unless the rule names that host |
| ADO REST gate and capability hold | `ADOGrantConfig` gate |
| Git broker repo allowlist | `GitGrants` / `PATGrants` |
| Branch namespace | `BranchNSEnforced`, `PATBranchNSEnforced` |
| `require_review_paths` push-content approval, pkt-line caps | The push-rules path, `maxReceivePackCmds` |
| LLM inspection and scan | May use `workspace_secret_names` values, which never reach the laptop |
| Blind caps, byte accounting, approval holds | As on the cluster proxy |

### 7.4 `runner_resident` and device posture

| Item | Design |
|---|---|
| Posture fields | `mdm_managed`, `disk_encrypted`, `os`, `os_version`, from the `posture` event |
| Source | `posture_source: runner_asserted`; console label "Reported by this runner" |
| What it does | Separates a lost or compromised laptop from a healthy one; it does not constrain a root developer |
| Platform attestation | Follow-on (Entra device compliance, Apple Managed Device Attestation, Windows DHA at claim); Linux has no standard path |
| Delivery | `DeliverResident {run_id, grant_id, kind, value, not_after}` after claim, before `CreateSandbox`; never `/internal/*`, never inside `CreateSandbox` (a placeholder names the grant there) |
| Keystore | macOS Keychain, Windows Credential Manager, Linux Secret Service. Write failure refuses; no file fallback |
| Use | The runner writes the value into the local proxy's startup config (api_key, broker routes, local mint route) or `SecretEnv` / `ManagedFiles`, replacing the placeholder |
| Revive | `ReplaceProxy` gets the org's stripped stored config; the runner re-applies the value from its keystore |
| Re-resolve | None: the value is static for the run. Pair it with short-lived or rotated secrets |
| Erase | `EraseResident` at run end, best-effort; the runner reports `resident_erase` |
| Audit | `secret.read` with `runner_id`, `purpose: runner-resident`, `delivery`, `posture` |

### 7.5 Governance policy shape

New governance target `local_credential_delivery`, written through the governance-change path.

```json
{
  "classes": {
    "api_key": {"mode": "via_org"},
    "github_token": {"mode": "via_org"},
    "env_secret": {"mode": "runner_resident", "require_posture": {"mdm_managed": true, "disk_encrypted": true}}
  }
}
```

| Rule | Value |
|---|---|
| Writer | `isSecurityOperator`; four-eyes where enabled |
| Default | Every class absent means `refuse` |
| Class keys | The closed set in [§7.2](#72-per-kind-bounds); an unknown key is `400` |
| `require_posture` | Only on `runner_resident`; fields `mdm_managed`, `disk_encrypted` (booleans) and `os_min` (per-OS minimum version) |
| Validation | A mode outside its class's bounds is `400`; `cloud_sts` and `oauth_subscription` are not writable |
| Audit | `credential.delivery.write` with `before` / `after` |
| Per run | `run.create` records `delivery: [{grant_id, class, mode, posture?}]`; grants persist `credential_grants.delivery` |
| Console | "Stays in your org" (`via_org`), "Stored on your runner" (`runner_resident`), "Not available on your runner" (`refuse`) |

Migration `0146` (landed by C-H): the policy row, `credential_grants.delivery TEXT NOT NULL DEFAULT '' CHECK (delivery IN ('', 'own', 'via_org', 'runner_resident'))`, and `runners.posture`, `posture_reported_at`, `posture_source`.

## 8. P1 baseline hosts, P2 locked L2, P3 ceiling term

### 8.1 P1 baseline hosts

| Item | Exact shape |
|---|---|
| Document | Governance target `egress_baseline`, body `{"baseline_hosts": ["models.internal.example"]}`, surfaced as `egress.baseline_hosts` |
| Writer | `isSecurityOperator`; four-eyes where enabled; audited `egress.baseline.write` with `before` / `after` |
| Values | Exact hostnames only: no wildcard, no suffix, no port |
| `internal_hosts` | `types.InternalHost` gains `Baseline bool` (`baseline,omitempty`); only those entries count, and their `host_suffix` counts as one exact host |
| Upgrade | Existing `internal_hosts` default `baseline:false`, so no run's grade changes |
| Composer | `safeBaselineDomains` becomes the built-in half; the deployment half is passed in, keeping `composer` pure |
| Followers | `beyondBaseline`, `autonomyEgress` and `apiKeyToNonBaselineHost` all read the merged set |

### 8.2 P2 locked L2

| Item | Exact shape |
|---|---|
| Term | `AutonomyRubric.AgentGuardrailLocks bool`, JSON `agent_guardrail_locks,omitempty` |
| Effect | At L2, `ForAgent` returns a frozen `claudeL2Locked`: L2 plus exactly `allowManagedHooksOnly` and `allowManagedPermissionRulesOnly` |
| Not added | `disableAutoMode` is already in L2; `disableBypassPermissionsMode` would refuse L2's own `--dangerously-skip-permissions` |
| Hold lane | `HoldTakesOver` returns false for locked L2, since its document protects the gate |
| Other rungs | L0 and L1 already lock; L3 gets no document; the term changes neither |
| Evidence | Golden `testdata` bytes plus pinned-CLI evidence, as the existing documents have |

### 8.3 P3 components times placement

| Item | Exact shape |
|---|---|
| Term | `GovernanceLimits.LocalSelfDefinedComponents bool`, JSON `local_self_defined_components,omitempty` |
| Default | `false`: a self-defined component on a local run is refused |
| Why the zero value denies | Local placement is new, so no existing profile's meaning changes |
| Refusal | `403` + `authz.denied`, reason `placement_component_self_defined`, naming the component |
| Scope | Self-defined only (`snapshot.SelfDefined`); org components follow the normal classification |
| Justification | Agent reach: a prompt-injected agent widening its own egress through a component on a host the developer administers |

On a laptop the developer is root. Egress enforcement, managed settings, ADO capability narrowing, push and method rules bind the **agent**, not the developer.

## 9. Disk link (OD-9)

### 9.1 Paths by placement

| Placement | Disk link |
|---|---|
| `local` | `local_dir` bind through the runner ([§6.7](#67-local-paths)); no sync |
| `remote` | `wardyn sync <run> <dir>` over the SSH gateway's sftp channel |
| Either, with shared storage | Drive-as-source (#112/#113), the shortcut |

### 9.2 `wardyn sync`

| Item | Design |
|---|---|
| Transport | One `session` channel with subsystem `wardyn-sync`: one new `case` in the subsystem switch, backed by the sandbox's own `sftpServerPath` |
| Directory | Env `WARDYN_SYNC_DIR` before the subsystem request, honoured only on a `wardyn-sync` channel. Validated server-side: absolute, under `/home/agent/`, no `..`; passed as `sftp-server -d` (Q9) |
| Honesty | The directory is a start point and a label, not a boundary: sftp-server can reach what the agent uid can |
| Engine | Pure-Go sftp client in the CLI; `rsync` over `ssh exec` also supported (P4) |
| Editors | Mutagen and VS Code server via the Remote-SSH pattern, **to be proven by the pilot** |
| New protocol surface | None; D4 holds |

### 9.3 Safety defaults

| Rule | Default | Overridable |
|---|---|---|
| Direction | Laptop → sandbox | Pull needs `--pull`, per run, recorded |
| Exec bit | Pulled files never get one | No |
| Symlinks | Refused both ways | No |
| Denylist | `.git/` (covers `.git/hooks`), `.envrc`, `.direnv/`, `.vscode/tasks.json`, `.vscode/launch.json`, `.idea/runConfigurations/`, `.idea/workspace.xml`; both directions | No |
| Conflicts | Laptop wins: a file changed on both sides since the last sync is not pulled; it is reported | No |
| State | `~/.local/state/wardyn/sync/<run>.json`, never inside the synced tree | — |

All of these run in the CLI on the laptop. The gateway relays bytes and cannot parse sftp without new protocol surface. The rules protect the developer from the agent.

### 9.4 Budget and audit

| Item | Design |
|---|---|
| Channel budget | `wardyn-sync` draws from its own per-run cap of `2`, separate from `WARDYN_SSH_MAX_SESSIONS_PER_RUN` (P5) |
| Action | `ssh.sync.transfer`: `dir`, `direction`, `bytes_in`, `bytes_out`, `error` |
| sftp | `ssh.sftp.transfer` gains additive `bytes_in` / `bytes_out` and keeps `bytes` (P6) |
| Run detail *Sync* row | From `ssh.sync.*` and `ssh.sftp.transfer`: directory, last activity, bytes each way, active sessions; no file-level claims |
| Pilot | Runbook, then the pilot estate as a second conformance substrate (owner-gated) |

## 10. Migration, deprecation, MDM and tiers

### 10.1 `m′`-at-org

| Item | 0.9 | 1.0 |
|---|---|---|
| `WARDYN_ORG_URL` on a full daemon | Works; boot warns deprecated, naming client mode | Removed |
| Device routes, `wdd_` bearer, `device.*` actions | Kept, documented as deprecated | Removed |
| `scripts/hybrid-walk.sh` | Rewritten for client mode | — |
| Docs (H8) | DESKTOP.md tier table; `docs/operations/hybrid-laptops.md`; ROADMAP "remote drive read locally" = drive-as-source only; ARCHITECTURE drops `POST /authz/decide` and signed snapshots | — |
| Docs (#107) | Placement in the governance, members, desktop and audit docs; `runs.placement` backticked in the `governance_profile` row | — |
| This doc | Supersedes hybrid-0.8 and the #78/#79 sections; both get a status line pointing here | — |

### 10.2 MDM shape (H9)

| Delivered | Content |
|---|---|
| `wardyn` | The CLI |
| `wardyn-runnerd` | The runner, with a launchd plist or systemd unit |
| Registration token | One `wdr_` token minted for the named person, in a `0600` file; the person claims |
| Not delivered | `policy.json`, `site-config.json`, OIDC client secret, `age.key` |

The release asset for `wardyn-runnerd` joins `release.yml`, `verify-release.sh` and `docs/VERIFY.md` only at CP3.

### 10.3 Tier naming

| Tier id | Console and `wardyn setup status` label (D-UI-b owns copy) | Governed by |
|---|---|---|
| `local-only` | "Local only — not governed by an organisation" | The person at the keyboard |
| `runner` | "Runner for <org>" | The org |
| `org` | "Organisation" | The org |

"Offline" and "ungoverned" are never the same word: an offline runner is still governed, and it runs nothing new.

### 10.4 Checkpoint flags

| Checkpoint | Flag |
|---|---|
| CP2 | `placement: local` refuses `placement_unavailable` when no runner is registered |
| CP3 | Site-config `runners.enabled` (default `false`, admin-only, audited) gates every runner route |

### 10.5 Migrations

| Number | Lane | Content |
|---|---|---|
| `0138` | #116 | `agent_runs.placement`, `placement_filled`, `runner_id`, `confinement_source` |
| `0139` | H1 | `runners`, `runner_registration_tokens`, the `agent_runs.runner_id` foreign key, `runner_id` on `local_dir` workspace sources |
| `0143` | #113 | `runner_id` on `host_path` user drives, with the placement label |
| `0146` | C-H (H3 fills pending-action behaviour) | OD-12 policy row, `credential_grants.delivery`, `runners.posture*`, `runner_pending_actions` |
| `0147`–`0149` | — | Stay reserved |

## 11. Conformance

Pattern: a refusal asserts "no sandbox exists and nothing was minted", not just "an error came back".

| # | Case | Suite | Assertion |
|---|---|---|---|
| C1 | Placement honoured or refused | routing (#110, #687) | `remote` with the cluster down refuses; no local sandbox exists |
| C2 | Never substituted | routing | `local` with the runner offline refuses `runner_offline`; no cluster sandbox exists |
| C3 | Ceiling resolved once | api | `deny_placements:["local"]` refuses at create; a permitting profile records the placement it got |
| C4 | Capability intersection | api | A demanded class only one placement offers is refused before resolution |
| C5 | No default between two | api | Empty placement with both eligible refuses `placement_required` |
| C6 | Classification closed | unit | A new unclassified `SandboxSpec`/`ProxyConfig` field fails `TestDispatchPlanFieldsClassified` |
| C7 | Brokered token refused | api | A `github_token` local run with no `via_org` policy refuses naming the grant; nothing minted |
| C8 | Trusted output | api | A scan or record run never places locally |
| C9 | Offline > lapse | loopback + Docker | Proxy stopped by the runner; agent alive; files kept; no local decision made |
| C10 | Kill while offline | loopback + Docker | Kill answers `202` pending; token denied; on reconnect the kill applies before any relay |
| C11 | Revocation | loopback + Docker | Live: every proxy stops, runs lost `runner_revoked`. Offline: `AUTH` answers `REVOKED`, then the same |
| C12 | Relay binding | api | A relayed request for a run on another runner, or a remote run, refuses `relay_runner_mismatch` |
| C13 | Relay required | api | A non-relayed `/internal/*` call for a local run refuses `relay_required` |
| C14 | `via_org` scope | api + proxy | A relayed injection or mint for a `via_org` grant refuses `delivery_via_org`; a non-`via_org` destination is not forwarded; the laptop copy's `MITMHosts` holds every `via_org` destination and no operator-only host |
| C15 | `via_org` re-check | proxy | A method or path outside the rule's pins, or a push outside the branch namespace, is refused org-side. A request whose `Host` or authority differs from the grant's destination is refused `via_org_destination_refused`, and nothing is dialed |
| C16 | Docker-through-remote parity | conformance | `test/conformance.Run` passes against `orchestrator.New(remoteSubstrate)` over the loopback and over WebSocket |
| C17 | Resident posture | api | A `runner_resident` class with unmet posture refuses naming the posture; keystore failure refuses |
| C18 | Resume | loopback | A drop inside `120s` resumes calls; byte streams reset; no `runner.disconnect` row |
| C19 | Spool | loopback | Spooled rows arrive as `spool` events in order with `observed_at` and `spooled:true`, before `READY`. Replay succeeds after the token lapsed and the run is lost `outage`. Overflow refuses egress and replays `builtin:spool-full` first |
| C20 | Sync safety | CLI | Denylisted paths, symlinks and exec bits never land on the laptop |
| C21 | Owner binding | runner | A run of another owner offered to a runner is refused by the runner |
| C22 | Unclaimed runner | loopback | An `unclaimed` runner, from either token kind, is offered no run and relays nothing |
| C23 | Claim mismatch | api | A claim by anyone but the token's owner, or with another fingerprint, refuses `runner_claim_mismatch` |
| C24 | Local path | api | A `local_dir` source bound to another runner, or outside `allowed_roots`, refuses `placement_local_path` |
| C25 | P3 | api | A self-defined component on a local run under a profile without the term refuses `placement_component_self_defined` |
| C26 | Pending fence | loopback | A relay for a run with an unapplied pending action refuses `runner_action_pending` |
| C27 | Kill cascade, local run | loopback + Docker | Killing a local run revokes its broker credentials and denies its token, online or offline |
| C28 | Frames and credit | loopback | An oversize frame closes the session; a writer with no credit blocks and drops nothing |
| C29 | `GOAWAY` | loopback | After `GOAWAY` the runner opens a fresh session and runs the full `PENDING` → `READY` sequence |

## 12. Threat-model deltas, audit actions, refusal reasons

### 12.1 Threat-model deltas (for D-TM)

| Delta | Substance |
|---|---|
| Developer-rooted host | Anything a local run uses is developer-readable; agent-side controls bind the agent only |
| Runner credential | Theft reaches the owner's own locally placeable runs until revoked; replaces #51's shape |
| Relay | Opaque TLS; `runner_id` binding; the local listener is reachable by any process on the laptop, which still needs a run token |
| Self-reported evidence | Decisions, recordings, `ExecOutput`, exit codes, capabilities, posture; #50–#52 re-scoped |
| Runner-asserted CC3 | A developer can advertise a class the host does not enforce; the record says so; it never satisfies the floor for a `runner_resident` credential |
| OD-8 classification | Closed switch at three chokepoints (create, dispatch, revive); `own` material is readable by its owner by design |
| Spool replay | Spooled decisions are self-reported by the runner and carried on its own stream; their content is `runner_asserted` |
| `via_org` | The developer can use an org credential for the run's life, never read it; request plaintext is visible to the laptop proxy |
| `runner_resident` | Readable by a root developer; erase is best-effort; posture is self-reported |
| Offline window | Up to `1h5m` of egress on cached policy with a bounded spool; replaces #54 for local runs |
| Revoked while offline | The sandbox runs until reconnect; tokens are denied at once |
| Sync and bind write-back | Agent-authored content the developer later executes; mitigations in [§9.3](#93-safety-defaults); scripts such as `Makefile` remain |
| P1 | An extended baseline lowers grades; governance-gated and audited |
| P3 | Self-defined components on a developer-administered host |
| Cross-workspace co-residence | From A-S4, listed here so D-TM keeps one list |

### 12.2 New audit actions (for AUDIT-ACTIONS)

Names follow the closed-verb grammar. **Deviation**: the release plan's `runner.register|claim|online|offline` use verbs outside the list.

| Action | When | Key data |
|---|---|---|
| `runner.token.create` | A registration token is minted | `owner`, `minted_for`, `expires_at` |
| `runner.token.revoke` | A token is revoked before use | `owner` |
| `runner.enrol` | A runner registered (was `runner.register`) | `name`, `state`, `key_fingerprint` |
| `runner.claim` | The owner claimed a runner (needs verb `claim` added to the list) | `key_fingerprint` |
| `runner.revoke` | A runner was revoked | `by`, `runs_lost` |
| `runner.connect` | First authenticated session after an offline row (was `runner.online`) | `version`, `resumed` |
| `runner.disconnect` | A session ended and was not resumed in `120s` (was `runner.offline`) | `last_seen_at` |
| `runner.action.request` | A pending action was queued | `kind`, `run_id` |
| `runner.action.apply` | The runner applied it | `kind`, `run_id`, `outcome`, `observed_at` |
| `runner.posture.record` | Reported posture changed | `posture`, `posture_source` |
| `runner.resident.erase` | Resident value erased at run end | `grant_id`, `outcome` |
| `credential.delivery.write` | OD-12 policy written | `before`, `after` |
| `egress.baseline.write` | P1 baseline written | `before`, `after` |
| `ssh.sync.transfer` | A sync session ended | `dir`, `direction`, `bytes_in`, `bytes_out` |

Changed rows:

- `run.create` gains `placement`, `placement_filled`, `runner_id`, `confinement_source`, `delivery`.
- `run.lost` gains reason `runner_revoked` and containment `pending_runner`.
- `secret.read` gains `runner_id`, `delivery`, `posture`.
- `egress.*` gains `spooled` and `observed_at` on rows ingested from a runner (actor `runner:<id>`), and the `rule_source` `builtin:spool-full`.

### 12.3 New refusal reasons (constants owned by C-H)

| Reason | Status | Where |
|---|---|---|
| `placement_required` | `422` | `placementFor` |
| `placement_unavailable` | `422` | `placementFor` |
| `placement_denied` | `403` | `deny_placements` |
| `placement_credential` | `422` | `localPlaceable` |
| `placement_trusted_output` | `422` | `placementFor` |
| `placement_component_self_defined` | `403` | P3 |
| `placement_capability` | `422` | #108 |
| `placement_local_path` | `422` | local paths |
| `runner_offline` | `422` | create, dispatch |
| `runner_ambiguous` | `422` | `placementFor` |
| `runner_not_found` | `404` | `runner_id` |
| `runner_unclaimed` | `409` | stream, claim |
| `runner_revoked` | `401` on the stream | runner hub |
| `runner_token_invalid` | `401` | registration |
| `runner_claim_mismatch` | `403` | claim (not the token's owner, or fingerprint differs) |
| `runner_posture_unmet` | `422` | H13; the CC3 floor for a `runner_resident` credential |
| `runner_keystore_unavailable` | `422` | `DeliverResident` |
| `runner_action_pending` | `409` | relay fence |
| `relay_runner_mismatch` | `403` | `internalAuth` |
| `relay_required` | `403` | `internalAuth` |
| `relay_route_refused` | `403` | relayed-route list |
| `delivery_via_org` | `403` | injection, mint |
| `delivery_runner_resident` | `403` | injection, mint |
| `delivery_not_via_org` | `403` | `/internal/via-org` |
| `via_org_destination_refused` | `403` | `via_org` egress |
| `via_org_cap_exceeded` | `429` | `via_org` egress |

## 13. Open questions

The owner approved every answer below on 2026-10-09; the design is written to it.

| # | Question | Decision |
|---|---|---|
| Q1 | The relay audit key: the plan says `data.via`, which egress rows and portal delegation already use with two other shapes | `data.relay = "runner:<id>"` on every relayed row. Invariant 4 fixes `data.via` as portal delegation |
| Q2 | A headless local run whose token lapses offline: today `Server.lostRunKeepable` tears headless runs down | Keep it as lost `outage` until its end plus grace, like an interactive run. The completion watcher skips kept runs, so `agent_exit` finalizes it ([§5.4](#54-reconnect-and-reconciliation)) |
| Q3 | Spool overflow before the lapse: proceed unaudited (today's #54 shape) or fail closed | Fail closed: refuse new egress until the spool drains. The first replayed row is a synthetic `builtin:spool-full` denial carrying the count |
| Q4 | Audit names outside the closed verb list | `runner.enrol`, `runner.connect`, `runner.disconnect`; add one verb, `claim`. Do not alias `register` |
| Q5 | Migration for `runner_pending_actions`, which the pre-assigned list does not name | Fold it into `0146`, which C-H already lands; keep `0148`–`0149` reserved (`0147` went to the #1826 audit-partition fix) |
| Q6 | Do members with no assigned profile get local placement when `runners.enabled` is on | Yes: `deny_placements` is a deny list, and absent means permitted. P3's default deny still applies |
| Q7 | Does a runner-asserted CC3 satisfy the blast-radius floor | Yes for `own` and `via_org` credentials. No for `runner_resident`: evaluate the floor as unmet and refuse naming the posture |
| Q8 | #924 (`UserDrive*` → `Drive*` rename) before #112/#113, or defer | Defer to after 0.9: a pure rename that collides with #112, #113 and the drive placement work |
| Q9 | The sync directory travels as env `WARDYN_SYNC_DIR`, widening `sshEnvAllowed` by one name | Accept. Validate server-side exactly as [§9.2](#92-wardyn-sync) states, and scope the name to the `wardyn-sync` subsystem only |
