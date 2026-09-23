# Model access — the 0.7.6/0.7.7 console surfaces, frozen as shipped

Every earlier design round in this repo left a frozen artefact under `docs/design/` before the
next visual change touched its surface. The 0.7.6/0.7.7 model-access work — the AWS-sign-in strip,
the sign-in dialog, the New Run rail's credential rows, and a failed run's own sign-in door —
shipped with none. This document is not a mock round: nothing here is proposed, and nothing in
`ui/src/` changes to produce it. It records what the shipped code already does, so the next visual
change to these four surfaces has something to check itself against.

Every string cited below carries a `// DRAFT (M2 canon pending)` comment at its declaration site
in the shipped code (`workspace-providers-prompt.md` §7.6's own round notes name this pending
sitting as "M2"). This document **is** that sitting, for the four surfaces §3–§6 cover. Freezing a
row here does not change its text — the frozen value is the shipped value, byte for byte — it only
removes the "pending" from the comment the next time a maintainer edits that file.

Citations below name **symbols**, never line numbers (the convention `CONSOLE-RULES.md` and the
`cmd/wardynd` citation guards already enforce) — a line number is stale the moment anything above
it moves.

## 1. What shipped, in one paragraph

`lib/model-access.ts#modelAccessDoor` grades one `/setup/status` body into one `ModelAccessDoor` for
one viewer — is there something about *this person's* AWS credential they should be told about, and
can they fix it. `components/wardyn/model-access-context.tsx#ModelAccessProvider` holds the one
shared instance (one dialog, one open/closed state, ref-counted ownership) that every surface below
reads through `useModelAccessDoor()` / `useClaimModelAccessDoor()`, so four callers never mount four
independent sign-in dialogs. The four surfaces are: the shell strip (§3), the sign-in dialog itself
(§4), the New Run rail's credential rows (§5), and a failed run's own sign-in door (§6). §7 lists the
model-access strings this document deliberately does **not** cover, and why.

## 2. The shared predicate and door

`lib/model-access.ts#modelAccessDoor` reads `status.model_access` and returns:

| Field | What it means |
|---|---|
| `state` | The server's state name: `not_configured`, `expired_signin`, `expiring`, `shared_expired`, `live`, `not_applicable`, or `""` (nothing graded — a legacy install, an older daemon, or `/setup/status` not yet fetched). |
| `action` | The server's own sentence, verbatim; `""` when there is none. Never reworded client-side. |
| `deadline` | RFC3339 UTC for `action`; `""` when the state carries none. |
| `needsAttention` | `state !== "live" && state !== "not_applicable"`. |
| `actionable` | This *viewer's own* sign-in would repair it: `MODEL_ACCESS_ACTIONABLE.has(state)` (`not_configured`, `expired_signin`, `expiring`) or `shared_expired` for an operator. |
| `perUser` | The `claude-code` row is an enabled `per_user` + `bedrock_sso` lane today. |
| `bedrockSSO` | The `claude-code` row is an enabled `bedrock_sso` lane today, per-user or shared — the weaker test a surface bound to a *past* event (a failed run's declared lane) needs, since a sign-in repairs nothing for an agent the roster has since moved off `bedrock_sso`. |

`components/wardyn/model-access-context.tsx#useModelAccessDoor` grades nothing until the viewer's
identity resolves (`useOperatorResolved`) — every field above reads as "nothing to say" in that
window, deliberately, because the audience-dependent states below (`shared_expired`) would otherwise
answer with the wrong audience's sentence.

## 3. Surface A — the strip (`components/wardyn/model-access-banner.tsx`)

The console's per-person notification surface for model access: mounted once by the shell, last in
its banner stack, on every screen except where a page surface already claims the door.

### 3.1 States (`model-access-banner.tsx#modelAccessStripCopy`)

| `door.state` | Sentence | Action line | Tone | Dismissible |
|---|---|---|---|---|
| `not_configured` | `MODEL_ACCESS_BANNER.NOT_SIGNED_IN` | server's, if it differs from the button label | warning | yes (first-run state) |
| `expired_signin`, door claimed by a page | `MODEL_ACCESS_BANNER.EXPIRED_SHORT` | server's | warning | no |
| `expired_signin`, door unclaimed | `MODEL_ACCESS_BANNER.EXPIRED` | server's | warning | no |
| `expiring`, deadline known | `MODEL_ACCESS_BANNER.EXPIRING` (per-user) or `SHARED_ADMIN_EXPIRING` (shared), `{when}` filled | none (deadline is in the sentence) | info | no |
| `expiring`, no deadline (pre-0.7.6 daemon) | `""` | server's `door.action` | info | no |
| `shared_expired`, viewer is operator | `MODEL_ACCESS_BANNER.SHARED_ADMIN_EXPIRED` | none | warning | no |
| `shared_expired`, viewer is not operator | `""` | server's `door.action` | warning | yes (viewer cannot act at all) |
| `live`, `not_applicable`, `""`, or an unrecognised future state | nothing renders | — | — | — |

The server's action line renders only when it differs byte-for-byte from the sign-in button's own
label (`serverAction()`) — printing an identical line would render the button as prose. What
survives that test is the pin-contradicted pair, which names two account/role pairs no client
sentence could carry.

### 3.2 Frozen table — `components/wardyn/model-access-copy.ts#MODEL_ACCESS_BANNER`

| Key | String |
|---|---|
| `NOT_SIGNED_IN` | You are not signed in to AWS — Claude Code needs your AWS sign-in |
| `EXPIRED` | Your AWS sign-in no longer works for Claude Code — sign in again |
| `EXPIRED_SHORT` | Your AWS sign-in no longer works for Claude Code |
| `EXPIRING` | Your AWS sign-in lapses {when} |
| `SHARED_ADMIN_EXPIRING` | The shared AWS sign-in every Claude Code run uses lapses {when} |
| `SHARED_ADMIN_EXPIRED` | The shared AWS sign-in no longer works — every Claude Code run needs it; sign in again |
| `DIALOG_TITLE` | Sign in to AWS |
| `DIALOG_DESCRIPTION` | Signs you in to your organization's AWS access portal for your Claude Code runs. The sign-in runs in the terminal in this dialog. |
| `SIGNED_IN_TOAST` | Signed in to AWS — your runs can use your session now |
| `NOT_NOW` | Not now |

`{when}` is `relativeTime(door.deadline)` ("in 3h"); the absolute stamp rides the sentence's `title`
attribute. The sign-in control's visible label is `workspace-providers-copy.ts#AGENTS.SIGN_IN_AWS`
("Sign in to AWS"), reused rather than retyped, so the strip, the rail, the failure block and the
two card CTAs never drift into four spellings of one control — this document does not re-freeze it.

### 3.3 Behaviour notes (not copy, but load-bearing for the next visual change)

- Suppressed entirely on `/setup` (the page *is* the door); suppressed on `/settings` and
  `/providers` for an operator only, since those pages mount their own sign-in pane for the same
  states.
- The first-run (`not_configured`) and the non-operator dead-shared-credential states are the only
  two a viewer may dismiss for the session (`sessionStorage`, keyed per-principal so a sign-out in
  the same tab cannot pre-dismiss it for the next person).
- On a completed sign-in the toast (`SIGNED_IN_TOAST`) is the only confirmation — the strip itself
  disappears on the next status read, and a surface vanishing is not treated as confirmation on its
  own (`CONSOLE-RULES.md` §9's transient case).
- Focus management: a sign-in opened from the strip returns focus to the shell's
  `#main-content` skip target on completion (the strip's own button no longer exists once the
  state clears); a sign-in opened from a page control (the rail, the failure block) returns focus
  to that control.

## 4. Surface B — the AWS sign-in dialog (`components/screens/settings/harness-login-pane.tsx`, `provider="aws"`)

One instance, mounted by the strip's `ModelAccessSignInDialog` and reused by every caller that opens
the door (`components/wardyn/model-access-context.tsx`). This section covers only the `aws` flow —
the `anthropic` (Claude subscription) flow shares the same phase machine but is out of this
document's scope, since none of the four named surfaces open it.

### 4.1 Phase machine (`harness-login-pane.tsx`, `type Phase`)

`intro | prompt | launching | starting | attached | saving | done | error`

| Entry condition | Phase | What renders |
|---|---|---|
| `flow.needsStartUrl && !startURLManaged` (ordinary Settings sign-in — nothing stored) | `prompt` | The numbered "what happens next" list (`login-flows.tsx#LOGIN_FLOWS.aws.expects`) + the AWS access-portal start-URL field. |
| `startURLManaged` (a `per_user` roster row — the org's portal is already stored) | `intro` | The same expects list, plus `AGENTS.SSO_START_URL_MANAGED` in place of the field. |
| Start login clicked | `launching` | "Opening the login sandbox…" |
| `POST /setup/harness-login` resolved with a run id | `starting` | The graded wait — see §4.3. |
| Run reaches `RUNNING` | `attached` | The embedded terminal + the AWS device-code note — see §4.4. |
| The helper's done marker sighted | `saving` | The server-corroboration round trip — see §4.5. |
| Corroboration confirms | `done` | "Token captured — your AWS SSO session is connected." (`flow.doneLabel`) |
| Any of: launch refused/failed, wait ends unreadable/stuck, helper prints a fail marker, corroboration refuses | `error` | See §4.6. |

### 4.2 Frozen table — the AWS flow's own strings (`login-flows.tsx#LOGIN_FLOWS.aws`)

| Field | Value |
|---|---|
| `cmd` | `aws sso login --sso-session wardyn --no-browser --use-device-code && wardyn-aws-sso` |
| `title` | Connect an AWS SSO session via container login |
| `doneLabel` | your AWS SSO session is connected |
| `capture` | `"helper"` — the credential is uploaded by the in-sandbox helper; the pane never scrapes a token off the PTY for this flow |
| `doneMarker` | `wardyn: aws sso credential captured` |
| `failMarker` | `wardyn: aws sso credential rejected:` |
| `expects[0]` | A sandboxed login run starts and a terminal appears here, running `aws sso login` — with no credential to start from. |
| `expects[1]` | When the AWS verification page is ready, “Open AWS sign-in” opens it in a new tab: enter the code shown beside the button and approve with your IAM Identity Center login. (#628: the tab opens only from that button.) |
| `expects[2]` | The SSO session is uploaded from inside the sandbox and stored write-only; Bedrock runs exchange it for short-lived role credentials. |
| `blurb`, `startURLManaged=false` | Give Wardyn your organization’s AWS access portal URL and it opens a sandbox, writes a minimal `~/.aws/config` holding just that URL and the configured SSO region (no credential — the sandbox has none to start with), and runs `aws sso login` for you. It prints a verification URL and a short user code — open the link in any browser, enter the code, and approve. Wardyn then captures the SSO session automatically so later Bedrock runs can exchange it for short-lived role credentials — with no host `~/.aws` mount and no static keys. |
| `blurb`, `startURLManaged=true` | Same body, opening clause replaced with `login-pane-copy.ts#AWS_BLURB_MANAGED_OPENING` ("Your admin set your organization's AWS access portal; there is nothing to enter.") followed by "Wardyn". |

### 4.3 The `starting` wait — frozen table (`login-pane-copy.ts`, `login-start-wait.ts`)

Graded on the wall clock, not a tick count (`login-start-wait.ts#startWaitVerdict`): a terminal
substrate reason ends the wait immediately regardless of how long it has been running; otherwise
the clock and a failure-streak floor both have to agree before the wait calls itself unreadable.

| Verdict | Condition | Sentence |
|---|---|---|
| `starting` | Default / a healthy short wait / a read blip under the retrying floor | No sentence: the door's steps (`login-pane-copy.ts#SIGNIN_PROGRESS`, #628) say it — "Starting the sign-in sandbox", or "Downloading the sign-in image — first time only" with "Can take a few minutes the first time." while the substrate reports `Pulling`. |
| `slow` | Reads are healthy, ≥ `RUN_POLL_SLOW_START_MS` (60 s) elapsed, still not up | The run's own `status_detail` sentence if one exists, else `LOGIN_SANDBOX_SLOW_START` — "Still starting — Wardyn can read the sign-in sandbox, it just isn't up yet. A first start may need to pull the image, which can take a few minutes." |
| `retrying` | Reads have been failing for ≥ `RUN_POLL_RETRYING_AFTER_MS` (10 s) but under the unreadable floor | `LOGIN_SANDBOX_READ_RETRYING` — "Wardyn can't read the sign-in sandbox right now — still trying. It may be starting normally." |
| `unreadable` | Reads failing ≥ `RUN_POLL_UNREADABLE_AFTER_MS` (`LAUNCH_DEADLINE_MS`) **and** ≥ `RUN_POLL_MIN_FAILURES` (15) consecutive failures | `LOGIN_SANDBOX_UNREADABLE` — "Wardyn stopped being able to read the sign-in sandbox, so it can't say whether it came up. Try again." Ends the wait; phase → `error`. |
| `stuck` | The substrate reports a terminal reason (`ImagePullBackOff`, `CreateContainerError`, `CreateContainerConfigError`, `CrashLoopBackOff`, …) | An image-pull reason (`ImagePullBackOff`, `ErrImagePull`, `InvalidImageName`) is #628's state 7: "Downloading the sign-in image — failed", the run's `status_detail` as is, and Retry. Any other: `LOGIN_SANDBOX_STUCK_LEAD_IN` ("The sign-in sandbox cannot start — this needs your admin; trying again gets the same answer until they fix it.") followed by the substrate's own sentence. Ends the wait; phase → `error`. |

If the run itself goes terminal while `starting` (killed/stopped with no substrate reason):
`LOGIN_SANDBOX_ENDED` — "The sign-in sandbox stopped before it was ready — nothing was captured. Try
again." (`harness-login-pane.tsx`, local to the pane) — or `run.failure_hint` verbatim when the run
carries one.

### 4.4 `attached` — the AWS device-code note (`harness-login-pane.tsx`, inline JSX; `capture-confirm.ts`)

| Condition | Text |
|---|---|
| Auth URL known, tab blocked | `auth-tab-handle.ts#AUTH_TAB_BLOCKED_NOTE` — "Your browser blocked the automatic tab — use the link above to open the verification page." |
| Neither `signedIn` nor auto-captured yet | "In the tab that opened (or the link above), enter the user code shown in the terminal and approve. Wardyn captures the session automatically when the login completes." |
| CLI printed its own success line (`capture-confirm.ts#extractSignedIn`, a hint only — never a verdict) | `capture-confirm.ts#CAPTURE_HANDOFF` — "The sign-in tool reports you are signed in. Wardyn is waiting for the sandbox to hand over your session. If the terminal above lists accounts or roles, click or tab into it, type the number you want and press Enter — it may ask twice, account then role." |
| Auto-captured, still saving | "SSO session captured — connecting…" |

### 4.5 `saving` — the corroboration round trip (`capture-confirm.ts`)

The PTY's done marker is sandbox-forgeable by construction, so the pane never trusts it alone — it
re-reads `/setup/status` and only claims a capture when the server independently agrees
(`serverConfirmsCapture`).

| String | Text |
|---|---|
| `CAPTURE_VERIFYING` | Checking with Wardyn that the session was stored… |
| `CAPTURE_NOT_CORROBORATED` | The sandbox reported a capture the server does not have. If your last attempt was interrupted, sign in again. If it keeps happening, the sandbox's report and the server disagree — check the run's audit trail. |
| `CAPTURE_CHECK_UNREACHABLE` | Wardyn couldn't reach the server to verify this sign-in — try again. |

A mismatch here does not refuse outright: the phase stays on `CAPTURE_VERIFYING` and hands off to a
background watch (`watchForCapture`, bounded by `CAPTURE_WATCH_MAX_MS` = 45 min or
`CAPTURE_POST_RUN_GRACE_MS` = 5 min past the run going terminal, whichever is first) — the sentence
it eventually shows on giving up is whichever of the two rows above the short round trip would have
shown, never a third wording.

### 4.6 `error` (every source, one Cancel; Try again suppressed on a 409 or a stuck substrate reason)

| Cause | Sentence |
|---|---|
| Server refused the launch (409 — already running, or the no-credential member preview) | The thrown error's own message; Try again withheld (a retry earns the identical refusal). |
| Helper printed its fail marker | `login-pty-extract.ts#SANDBOX_REFUSAL_LEAD_IN` + the extracted sentence. |
| Corroboration watch gave up | Whichever of §4.5's two sentences the short round trip would have shown, or `CAPTURE_NOT_CORROBORATED` if none ran. |
| Wait graded `unreadable` / `stuck` | §4.3's rows. |
| Run ended with no substrate reason | `LOGIN_SANDBOX_ENDED`, or `run.failure_hint` verbatim if the run carries one. |

## 5. Surface C — the New Run rail's credential rows (`components/screens/new-run/new-run-rail.tsx`)

Two independent facts, both gated on the selected agent being the one `model_access` grades
(`claude-code`): where the model credential *lands* (`CredentialFacts`), and whether *this launcher*
has a working sign-in at all (`ModelAccessLine`). The rail claims the door for exactly as long as it
renders its own sign-in control, so the shell strip drops its button here and keeps its sentence.

### 5.1 `CredentialFacts` — frozen table (`components/wardyn/copy.ts#RAIL_CREDENTIAL`)

| Resolved state | Key(s) | String |
|---|---|---|
| Proxy-injected | `PROXY` | Model credential — injected by the proxy at launch; never written into the sandbox. |
| Proxy-injected, staged placeholder mounted | `PROXY_STAGED` | Model credential — this deployment injects it at the proxy; the sign-in mounted into the sandbox is staged as a placeholder. |
| Sandbox-resident, Bedrock family | `SANDBOX_BEDROCK` | Model credential — AWS credentials sign inside the sandbox, so this run holds them for its lifetime. |
| …chip, per-user row | `SANDBOX_BEDROCK_CHIP_PER_USER` | Per-person AWS sign-in |
| …chip, shared row | `SANDBOX_BEDROCK_CHIP_SHARED` | Admin's credential |
| Sandbox-resident, Claude subscription (`WARDYN_SUBSCRIPTION_INJECT=off`) | `SANDBOX_SUBSCRIPTION` | Model credential — this deployment mounts the Claude sign-in into the sandbox, so this run holds it for its lifetime. |
| Image-resident (a `none` roster row, BYOA) | `IMAGE` | Wardyn wires no model credential — the image brings its own, and Wardyn cannot say where it lives. |
| Not yet resolved (no Preflight run) | `RESOLVED_AT_LAUNCH` + `RUN_PREFLIGHT_HINT` | Resolved at launch. / Run Preflight to see where this run's model credential will live. |

### 5.2 `ModelAccessLine` — frozen table (`components/wardyn/model-access-copy.ts#RAIL_MODEL_ACCESS`)

| `door.state` | Sentence | Action |
|---|---|---|
| `not_configured` | `NOT_SIGNED_IN` — Sign in to AWS before you launch — Claude Code needs your AWS sign-in. | server's, if distinct from the button label |
| `expired_signin` | `EXPIRED` — Your AWS sign-in no longer works for Claude Code — sign in again before you launch. | server's |
| `expiring`, deadline known | `EXPIRING(when)` — Your AWS sign-in lapses {when} — sign in again soon. | none |
| `expiring`, no deadline | `""` | server's `door.action` |
| `shared_expired`, operator | `SHARED_ADMIN_EXPIRED` — The shared AWS sign-in no longer works — sign in again before you launch. | none |
| `shared_expired`, non-operator | `SHARED_EXPIRED` — Your admin's AWS credential has expired — Claude Code runs need it reconnected. | server's `door.action` |
| `live` / `not_applicable` / unrecognised | nothing renders | — |

The rail's own sign-in control carries `SIGN_IN_ARIA` ("Sign in to AWS — from the New Run rail") as
its accessible name — distinct from the strip's and the failure block's, so a page carrying more
than one "Sign in to AWS" control never collides on one accessible name. It is hidden whenever
`door.open` — never a live control pointing at a dialog already on screen.

### 5.3 The deployment-wide warning (only when `ModelAccessLine` renders nothing)

| Key | String |
|---|---|
| `NO_PROVIDER` | No model provider is connected. This run launches; its first model call fails. |
| `NO_PROVIDER_CTA` | Connect → |

Shown only when `showModelWarning && !showModelAccess` — the per-person fact supersedes the
deployment fact whenever both would otherwise render, since a never-signed-in member under a working
per-user lane is not the same problem as a deployment with no model path at all.

### 5.4 The credential-refused relaunch

A 422 refusal carrying `reason: model_credential` (`launch.credentialRefused`) opens the door
automatically — once per click — against the same `launchRef` neighbour the strip returns focus to,
and replays the same launch the moment the sign-in completes. No sentence of its own; it reuses
§5.2's rows and §2's door.

## 6. Surface D — the launch door (`components/screens/run-detail/failure-block.tsx`)

A failed run's own sign-in door — 0.7.6 Finding 3. `ENDING_COPY` deliberately carries no `credential`
row: the server's own refusal is already the complete "What happened" (rendered verbatim as
`run.failure_hint`), and a client sentence restating it would say the same fact in weaker words. What
the console adds is the sign-in control itself.

### 6.1 `showDoor` — every term load-bearing (`failure-block.tsx#RunFailureBlock`)

Renders only when **all** of: the ending is `credential`; the run's declared lane was `bedrock_sso`;
the roster's `claude-code` row is `bedrockSSO` **today** (not moved to another mechanism since); the
door is `actionable` for this viewer; and the viewer is the run's own `created_by` (an admin reading
a member's failed run is never offered a sign-in that repairs nothing for that run).

### 6.2 Frozen table — `components/wardyn/model-access-copy.ts#MODEL_ACCESS_RUN_DOOR`

| Key | String |
|---|---|
| `NOTE` | Sign in here. This run stays failed — relaunch it from the run header. |
| `SIGN_IN_ARIA` | Sign in to AWS — for this failed run |

The button's visible label stays `AGENTS.SIGN_IN_AWS` ("Sign in to AWS") — one spelling of the
control, reused, never retyped, across the strip, the rail and this block.

## 7. Out of scope — model-access strings this document does not freeze

Two further groups of shipped, `DRAFT (M2 canon pending)`-marked strings live beside the four
surfaces above but render on surfaces outside them. Recorded here as open items rather than silently
folded in, per this lane's own instruction not to widen into a redesign:

- **The mid-run re-auth row and its board/approvals rendering** —
  `components/wardyn/model-access-copy.ts#REAUTH_ROW`, `REAUTH_HEADING`, `REAUTH_TITLE`,
  `REAUTH_SIGNED_IN_TOAST`, `reauthAudience`, `reauthRowHint`, and
  `lib/reauth-waiting-copy.ts#waitingReauth`. These render on the run cockpit
  (`components/wardyn/live-approvals.tsx`), the `/approvals` card
  (`components/screens/approvals.tsx`), and the runs board card — none of which is one of the four
  surfaces this lane names. They remain `DRAFT (M2 canon pending)` in the shipped code; a future
  canon round should cover the cockpit/approvals/board surface as its own unit rather than as a
  subsection of this one.
- **`AGENTS_DRAFT` and `PROVIDERS_DRAFT`** (`lib/workspace-providers-copy.ts`) — new strings
  rendered on the Providers screen's Agents tab (`components/screens/providers/agents-tab.tsx`) and
  the Providers screen itself (`providers-screen.tsx`), including the per-user sign-in banner and
  the pinned-account/role fields. The source comments beside both exports say explicitly where their
  canon lands: `docs/design/workspace-providers-prompt.md`, the same document their already-frozen
  §7.2/§7.7 tables came from — not this document. Left untouched here for that reason.

## 8. Round notes

Every string in §3–§6 above was already shipped, unchanged by this lane — `git status --porcelain
ui/src` is empty for this change. Freezing them here is the M2 sitting the shipped comments have
been waiting on since 0.7.6; nothing was reworded, reordered, or normalised to remove a divergence
in the process. No open owner questions: this round records, it does not propose.
