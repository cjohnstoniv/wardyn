# Wardyn 0.7.6 — the 0.7.5 field report: SSO UX, "the facts exist, connect them to the person"

## Context

The operator who drove 0.7.4 and 0.7.5 ran **0.7.1 through 0.7.5** on the same private-endpoint Kubernetes
estate (Entra SSO, one enabled roster row `claude-code` / `bedrock_sso` / `credential_source: per_user`,
an admin and two colleagues) and photographed `HANDOFF-wardyn-076-sso-ux.md` (seven photos in
`attachments (3).zip` + `attachments (4).zip`; all 252 lines transcribed verbatim in Appendix A, no gaps).
It consolidates ONE journey — a person getting AWS SSO working and running a Claude Code agent — into one
question: *when a person's model access is missing or lapsed, does the product put them on the path to
fixing it, or does it let them find out by failing?* Today mostly the latter, and the operator's central
claim is that **the facts and the mechanisms already exist; they are not connected to the person**:
`model_access` grades the state, the CTA exists, the banner pattern exists, `ResolveWait` holds calls,
`waitingDetail` explains slowness.

The eight findings: (1) New Run's model warning is keyed on the deployment fact `llm_ready`, so it is
silent for exactly the members who need it — the same defect 0.7.5 fixed on Getting Started; (2) nothing is
proactive — `not_configured` / `expired_signin` / `expiring` reach a person only if they open Getting
Started; (3) the refusal names a destination ("sign in again under Settings → Model provider") instead of
being one; (4) a credential that lapses mid-run kills the run instead of pausing it; (5) one dropped packet
on the renewal path silently destroys a person's model access (`spent: false` then `invalid_grant` ninety
minutes later); (6) a starting run never says what it is waiting on although the kubelet's reason is in hand;
(7) the verification tab does not open itself and the pane does not close when the server confirms; (8) there
is no supported way to put the daemon's own AWS calls behind a corporate proxy. The changelog for 0.7.5
already admits two of these as known gaps (the rail chip painted from the roster row alone; the shared-row
admin offered the member-side CTA).

**Goal for 0.7.6:** handle ALL eight findings, verified end to end on the kind SSO walk with rebuilt images,
and release. Where the operator's proposed mechanism does not match the code (Finding 4: the proxy never
signs or injects the per-user Bedrock call; Finding 6: the chart deliberately grants no `events` verb;
Finding 7b: the corroboration narration already shipped in 0.7.5), the plan delivers the operator's
OUTCOME by the mechanism the code actually supports and says so in the "Contradictions" list.

**Execution model (owner's instruction, 2026-09-17):** Sonnet 5 and Opus 5 do the implementation and the
design-execution work; **Fable 5.1 reviews and verifies every lane on completion** (blind FIX-FIRST loop),
verifies the W5 walk evidence, runs the W6 blind lenses and the docs truth-check. Fable is probed with one
small sync agent first; on a 429 / stall the reviewer runs on Opus 5 and the ledger records the switch.
**Before execution** this plan itself gets two Fable review rounds — round 1 = one general blind reviewer
PLUS one separate UX-specialist reviewer (intuitive UX across the banner, rail, door, pane and starting
states); apply both; round 2 = one general blind reviewer re-reading the applied plan (UX re-check on any
UX section that changed); apply; then ExitPlanMode. When round 1 is fanned out, the full plan text is copied
to the owner's clipboard (`clip.exe`) for a third-party review agent.

## Summary — one screen

(Filled from the lane designs below; every row = one lane.)

| # | Finding (operator's words, shortened) | Lane | The fix in one line | Worker | Wave |
|---|---|---|---|---|---|
| 2 | Nothing proactive — three actionable states reach nobody | `ui-model-access-door` | ONE pure predicate (`lib/model-access.ts`) + a context fed by App.tsx's existing `/setup/status` (5-min visibility-aware poll) + a global strip (undismissable for a lapse; a per-viewer session "Not now" for the first-run and shared-dead states), rendered last in the shell stack, whose "Sign in to AWS" opens the pane in the pre-sized Settings Dialog; suppressed on `/setup` and `/settings` | Opus | W1 first |
| 1 | New Run warning keyed on a deployment fact, silent for a per-user member | `ui-new-run-model-access` | The rail reads the door predicate for the selected claude-code agent and states which per-person state the launcher is in ("refused at launch"); zero-line diff to the 995-line screen; the rail carries its own link-variant sign-in under a distinct accessible name (U-13's actual rule); Launch stays a server decision | Sonnet | W1 |
| 3 | The refusal names a destination instead of being one | `run-credential-door` | `reason: model_credential` on the existing `run.create` failure audit row → client ending kind `credential` → the failure block shows the server sentence + the door (only where a sign-in repairs it); no new column, no new action | Opus | W1 |
| 5 | One dropped packet silently kills model access | `sso-refresh-visibility` | The one retry already exists — make it legible (`attempts`); grading consults the spent set → `expiring` (actionable) while the token still works, instead of `live` until the registration lapses | Sonnet | W1 |
| 8 | No supported daemon-side proxy for AWS calls | `daemon-egress-proxy` | `WARDYN_DAEMON_PROXY_URL` + `WARDYN_DAEMON_NO_PROXY` set on `http.DefaultTransport.Proxy` beside `installTrustedCA`; auto-bypass of the k8s API host and the SSO test override; boot refuses a malformed value | Sonnet | W1 (first) |
| 7a | The verification tab never opens | `login-pane-tab` | Open `about:blank` on the Start-login click, sever `opener`, navigate the handle when the URL appears; blocked → 0.7.5's link + a note | Sonnet | W1 |
| 7b | The pane sits on a sign-in that worked | `login-pane-confirm` | The CLI's success line (a hint, never a verdict) swaps the sentence and starts a STRICT server watch bounded by the run's life + the 5-min upload grace; the pane closes on the server's word; the marker path hands back to the watch instead of refusing at 1.5 s | Sonnet (same agent, after 7a) | W1 |
| 4 | A mid-run lapse kills the run | `credential-reauth-hold` | Phase B (the code's own named next step): the proxy injects `x-amz-sso_bearer_token` on `portal.sso` through the existing grant re-resolve loop; a 423 from the control plane raises a `credential_reauth` approval (audited) and the sidecar HOLDS the request (`WARDYN_CREDENTIAL_REAUTH_TIMEOUT`, clamped); the capture resolves it (`credential.reauth.resolved`); the run resumes. **Security-sensitive: invariants I1–I8, state machine, adversarial round, kill switch `WARDYN_AWS_SSO_PROXY_INJECT`** | Opus (+ Phase 0 threat-model delta) | W2, gated |
| 6 | A starting run never says what it waits on | `starting-detail` | `SandboxSpec.OnWaiting` callback from the k8s poll loops (and docker's pull) → `agent_runs.status_detail` (migration 0063), read-blanked outside STARTING → header chip, board row, and the pane's verdict machine ends on a REASON | Opus | W2 |
| — | Live proof | `e2e-sso-path` | New live cases I (banner), A(rail)+, J (run door, when forceable), K/K(resume) (hold), E+/E2 (starting detail), tab-open (providers.spec) on the kind SSO walk | Opus | W3 build → W5 walk ×2 |
| — | Docs, release | `docs`, `release` | Assemble `canon/*`; copy 0.7.5's release tooling | Sonnet, Opus | W3, W7 |

## Facts established (2026-09-17, base = dfa89f60)

- **Lineage.** `origin/main` = `origin/release/0.7` = tag `v0.7.5` = **dfa89f60** = local `main` (clean). PR #71
  (`fix/nightly-harness`, test/CI only, 28/28 checks green) is OPEN awaiting the owner's merge; it does not
  touch product code. The campaign cuts from `origin/main` at cut time (dfa89f60 today; if #71 is merged
  first, the merge commit — no rebase of lanes is needed either way).
- **Worktrees.** `~/wt-v075` (feat/v0.7.5 = dfa89f60, holds `local/v075/` tooling: gate.sh, briefs/COMMON.md,
  RUNBOOK-W4-W7.md, release/ scripts) is the template; 135 worktrees are registered (0.7.4/0.7.5 lanes +
  verify-runs) — Step 0 lists, never deletes.
- **Laws carried in (0.7.5 COMMON.md, unchanged):** ≤3 gate-runners incl. reviewers (gate.sh semaphore, 2
  heavy slots box-wide); `nice -n 10 GOMAXPROCS=8 go test -p 4`; vitest `--maxWorkers=4`; golangci cache clean
  under lint.lock, last; ONE Playwright spec per call under e2e.lock on `DOCKER_HOST=unix:///run/wardyn-docker.sock`;
  file cap 1000 via `scripts/check-file-size.sh`, NO new allowlist entries — measured at dfa89f60:
  `new-run-screen.tsx` **995**, `attach-terminal.tsx` **995**, `internal/egress/proxy/proxy.go` **997**,
  `llm_routes.go` **988**, `app-shell.tsx` 912, `harness-login-pane.tsx` 910, `run-detail.tsx` 902,
  `internal/api/approvals.go` 940, `harnesscred.go` 898, `runs_bedrock.go` 856, `ui/src/app/components/wardyn/copy.ts` **961**, and `internal/api/setup.go`
  **1120 = its allowlisted cap** — new logic → new files; audit-citation window 0 checked PER COMMIT;
  `TestCommentsCiteSymbolsNotLineNumbers`; envdoc guard; claims-match-code; every new string `// DRAFT (M2
  canon pending)` + canon row (frozen tables in `docs/design/*-prompt.md` are byte-parity tested — never
  edited); FULL vitest for any ui/src change (`console-rules-citations.test.ts` pins CONSOLE-RULES.md to
  app-shell.tsx line numbers); `git commit -s`, author `cjohnstoniv <cjohnstoniv@users.noreply.github.com>`,
  NO AI trailers (repo law overrides the harness reminder); every UI fix re-runs its Playwright spec;
  `ui/e2e/live/**` has ONE owner (`e2e-sso-path`); a walk runs against images REBUILT from the merged tip;
  `agent-claude-code` is an unpublished local recipe (image-side fixes need the rebuild/push/re-point note);
  `cmd | grep -q` under pipefail = SIGPIPE false-fail (capture then match); clean-tree `make ci` needs
  `pnpm install --frozen-lockfile` first; never write estimated timestamps into a ledger.
- **Release authority:** the OWNER pushes/tags/releases unless a `/goal` grants it for 0.7.6 (0.7.5's grant
  does not carry over — the release lane STOPS at "release commit verified, commands delivered").
- **Iris KG** unreachable this session (`table 'topics' not found`) — noted, not blocking; queue writes in
  `local/v076/IRIS-PENDING.md`.
- **Already-promised 0.7.6 follow-ups NOT in the field report** (CHANGELOG 0.7.5 Known gaps): per-person
  advisory lock for the sign-in supersede race; a third k8s cache volume / caches under the workdir;
  admin-tier 5xx driver-text sweep. Out of this plan unless the owner adds them (open decision O-1).

## Step 0 — coordinator prep (sequential, ~20 min)

1. `git -C ~/containerized-agent-envs fetch origin --tags`; assert `origin/main` = dfa89f60 (or the #71 merge);
   `git worktree add ~/wt-v076 -b feat/v0.7.6 <base>`; copy `~/wt-v075/local/v075/` tooling →
   `~/wt-v076/local/v076/` (re-point `gate.sh`'s three hardcoded paths, COMMON.md's plan path + base, empty
   `canon/` and `evidence/`, fresh VERIFY-LEDGER.md header; RUNBOOK-W4-W7.md with 0.7.5→0.7.6 substitutions).
2. Gate PG: `docker run -d --name wardyn-v076-pg -p 127.0.0.1:55440:5432 -e POSTGRES_USER=wardyn -e
   POSTGRES_PASSWORD=wardyn postgres:17` + one DB per lane (`wardyn_v076_<lane>`).
3. **Probe Fable** with one tiny sync `independent-reviewer` agent (model `fable`) reading this plan's Step 0;
   on 429/unavailable every reviewer/verifier runs on `opus` (record the switch in the ledger).
4. Lane worktrees: `git worktree add ~/wt-v076-<lane> -b lane/v0.7.6-<lane> <base>` per lane, on launch.
5. Confirm the kind cluster `wardyn-quickstart` is UP on the wardyn daemon (`DOCKER_HOST=unix:///run/wardyn-docker.sock
   docker ps`) — the walk needs it; never `kind create/delete` from a lane.
6. **Artifact lineage** (third-party review): verify in one script that `origin/main` = `release/0.7` = tag
   `v0.7.5` = the README install line (`v0.7.5`, README.md:31), the published release's assets, the chart
   version and the five image digests all agree — a walk must never run against an image that is not the
   commit under test (the GitHub web view can lag; the in-repo README at dfa89f60 already names v0.7.5).

## Lanes

(Lane sections — Facts / Design / Fix / DRAFT strings / Tests red-first / Docs / Risks — follow. Every
file:line was verified at dfa89f60 by an explorer and a design agent and is re-cited from the lane's own
HEAD before editing.)

### Shared facts for the three model-access lanes (F1, F2, F3) — verified at dfa89f60

- **States and wire.** `internal/api/modelaccess.go:166-192` six states; `modelAccessExpiringWindow` 24 h (:197);
  action strings DRAFT block :205-230 (`Sign in again before %s` · `Sign in to AWS` · `Your admin's model
  credential expired — ask them to reconnect it` · the pin-contradicted action naming both account/role pairs);
  `SetupModelAccess` :264-288 — the wire is exactly `{state, mechanism?, action?}` (Deadline / PinMismatch /
  PerUser are `json:"-"`); `memberModelAccess` :310-327 folds every non-PerUser state a member sees to `live`
  or `shared_expired`; `setupModelAccess` :408-458 is keyed on `modelAccessAgent = "claude-code"` (:36) —
  **`model_access` grades the claude-code row only.** `SetupStatus.ModelAccess` (`setup.go:126`) and
  `SetupStatus.Harnesses` (`setup_integrations.go:485-522`: id, display, enabled, mechanism, credential_source,
  credential_residency) both survive `redactSetupStatusForMember` (`setup.go:779-846`).
- **The honest per-user predicate already exists:** `isPerUserSsoRow(h)` (`ui/src/app/lib/workspace-providers-copy.ts:321`)
  and its composition `status.harnesses?.some(h => h.id === "claude-code" && isPerUserSsoRow(h))`
  (`connection-cards.tsx:461`). `per_user` requires `bedrock_sso` (`agent_providers.go:308`) but NOT
  `claude-code`, so a `codex` per-user row is savable and `isPerUserSsoRow` alone would paint claude-code's
  state over a codex run. `MODEL_ACCESS_ACTIONABLE = {not_configured, expired_signin, expiring}`
  (`workspace-providers-copy.ts:306`); `AGENTS.SIGN_IN_AWS` = "Sign in to AWS" (:247).
- **The create gate.** `enforceCreateLLMMechanism` (`runs_dispatch_llm_mechanism.go:434-476`) answers **422**
  before a run row exists for every model-calling shape (`llmMechanismGateApplies` :338-347 → `isModelRun`
  `runs_dispatch_llm.go:78-80`) when the declared lane is not satisfied — a never-signed-in per_user member's
  interactive or ephemeral-workspace run is refused at the click, with the same sentence, rendered
  untruncated as `launch.error` (`new-run-rail.tsx:317-321`). A NON-interactive workspace-bound run is a
  **scan run** (`WARDYN_SCAN_ONLY=1`, `runs_dispatch_mounts.go:324-331`) that makes no model call and needs no
  credential; `task_mode=exec` likewise. So on New Run "find out by failing" = the 422 at the click; the
  FAILED-run shape (Finding 3) is the DISPATCH-time refusal (`enforceConfiguredLLMMechanism` :254-289 →
  `failAndRevoke` + audit `run.create`/`failure`/`{"error": msg}` :285-287) — a credential that lapsed or was
  spent between create and dispatch, or a pin contradiction.
- **No machine-readable failure kind exists on a run.** `AgentRun.FailureHint` (`internal/types/types.go:202-211`,
  "Display-only; never interpreted by the control plane"); TS mirror `ui/src/app/lib/types/runs.ts:127-132`;
  client `RunEndingKind` (`lib/types/audit.ts:149-153`) is derived by `runEndingFromAudit`
  (`lib/api/audit.ts:186-221`) from audit action names (`FAILED_CAUSE` :160-163: run.build→image,
  run.selftest→selftest) — a credential refusal grades `unknown`; `runEndingFromAudit` already reads
  `data.reason` into `detail` (:190).
- **Surfaces.** `new-run-screen.tsx` (995 lines) — `llmReady` :157 set from **`hasLlmPath(st)`**
  (`lib/readiness.ts:59-61`, a deployment-wide integration count — NOT `st.llm_ready`) at :199-207,
  `showModelWarning={isAgent && llmReady === false}` :955, `agentRow={…harnesses?.find(h => h.id === state.agent)}`
  :978 already reaches the rail. `new-run-rail.tsx` (399) — props :37-90, warning block :261-269 (inline JSX,
  `<Link to="/settings">Connect →</Link>`), `CredentialFacts` :104-140, gates `showCredentials` /
  `showCredentialFacts` :200-208, `RAIL_CREDENTIAL` in `wardyn/copy.ts:912`. Member Getting Started
  `member-getting-started.tsx` (561): `modelKeyProvider(status?.harnesses)` (`your-model-key.tsx:34-48`),
  `modelKeyState` (`model-key-state.ts:68-121`), action line :352-353, pane mount :358-372
  (`<HarnessLoginPane provider="aws" startURLManaged onDone onCancel/>`); the pane's props
  (`harness-login-pane.tsx:359-380`) need no agent id. **The pane is already mounted in a pre-sized Dialog**
  in Settings: `connection-cards.tsx:705-725` (`style={{width:"min(96vw, 72rem)", maxWidth:…, translate:"none",
  transform:"none"}}`, `DialogTitle` "Sign in with AWS SSO"; `LOGIN_PTY_COLS = 512`). Banner precedent
  `wardyn/member-mode-banner.tsx` (208; `MEMBER_MODE` DRAFT :23-95; `MemberModeBanner` :171-208, `role="status"`,
  class `relative z-50 flex shrink-0 items-center gap-2 border-b border-border bg-warning-subtle px-4 py-2 text-sm
  text-warning`, undismissable, underlined action button). `app-shell.tsx` (912) banner stack :559-623
  (member-mode :562 · unreachable :567-578 · identityUnknown :586-601 · session-expiry :608-623 with
  `SESSION_WARN_MS` 5 min / `SESSION_CHECK_MS` 15 s / link "Sign in again"); the shell's own props type sits at
  :486-500. `console-rules-citations.test.ts` pins `CONSOLE-RULES.md:35` → `app-shell.tsx:368–374`
  (`navLinkClass`) and `:37`,`:215` → `app-shell.tsx:570` (the unreachable banner's className line) —
  any shift of :570 must re-cite CONSOLE-RULES.md in the same commit. `App.tsx` holds `setupStatus` +
  `refreshSetupStatus` (:445-455; fetched ONCE at auth :495 — "the expensive endpoint": `handleSetupStatus`
  `setup.go:495-620` runs runner Capabilities, CLI sweep + subscription peek, secret listing, platform/SCM
  detect, site-config read, AWS SSO blob decrypt); `AttentionPublisherProvider` wraps `<Routes>` at :564;
  `usePoll` (`lib/use-poll.ts:46`) has an in-flight guard, skips hidden tabs and fires immediately on
  `visibilitychange` back to visible (:93-97); the shell already polls `HEALTH_POLL_MS` (App.tsx:56, 5 s) and
  `ATTENTION_POLL_MS` (:300, 5 s). `/me` (`me.go:97-101`) is "the console's most-polled route" whose author
  kept a site-config read off the member path — no `model_access` belongs there. Run failure surfaces:
  header chip `run-detail-summary-header.tsx:261-264` (danger Chip, `max-w-[160px]`, truncated, title=hint);
  full prose `run-detail/failure-block.tsx:147-165` (`!copy && run.failure_hint` — the only line for kind
  `unknown`; the block deliberately has no clone door since 0.7.3 F7, :194-198; the header's "Start a run like
  this one" covers relaunch, `run-detail-summary-header.tsx:83`). **The Runs board has NO failure chip**
  (`runs/run-card.tsx` never reads `failure_hint`; :255-259 is `cloneRun`). `authorizeHarnessLogin`
  (`harnesscred.go:708-710`) returns early `true` for ANY operator before the `!perUser` refusal, and
  `agents-tab.tsx:206-222` mounts the pane with `startURLManaged={false}` for a shared row — that IS the
  documented repair path for a shared credential. `docs/AUDIT-ACTIONS.md:56` documents the `run.create`
  failure family citing `runs_dispatch_llm_mechanism.go:290`; citation window 0
  (`cmd/wardynd/audit_actions_doc_guard_test.go:40`); the forward guard checks action NAMES only.

### Lane `ui-model-access-door` (W1, **Opus** worker + Fable review) — Finding 2 (+ the door F1/F3/F4 reuse)

**Root cause.** Every actionable model-access state is published on `/setup/status` for every caller and
exactly two screens read it; nothing in the shell does. The product has a per-person credential lifecycle
and no per-person notification surface.

**Design.**
1. **ONE predicate — NEW `ui/src/app/lib/model-access.ts`** (pure, ~35 lines, no React). Not an extension of
   `model-key-state.ts`, which answers a card-shaped question (`done`/`revealAllowed`/`band`) keyed on
   `modelKeyProvider`'s roster-order row — the wrong row for this.
   ```ts
   export const MODEL_ACCESS_AGENT = "claude-code"; // mirrors internal/api/modelaccess.go modelAccessAgent
   export interface ModelAccessDoor {
     state: string;            // "" when the server graded nothing
     action: string;           // the server's own sentence, verbatim; "" when none
     needsAttention: boolean;  // graded, and neither live nor not_applicable
     actionable: boolean;      // a sign-in THIS caller can complete repairs it (MODEL_ACCESS_ACTIONABLE)
     perUser: boolean;         // the claude-code row is enabled per_user + bedrock_sso
   }
   export function modelAccessDoor(status: SetupStatus | null): ModelAccessDoor;
   ```
   `perUser` = the `connection-cards.tsx:461` composition, lifted (imports `isPerUserSsoRow`; no third copy).
   `actionable` = `MODEL_ACCESS_ACTIONABLE.has(state)` with **no `perUser` gate**: the server admits an
   operator's sign-in unconditionally (`harnesscred.go:708-710`) and `memberModelAccess` already guarantees a
   member on a shared row never sees an actionable state; gating on `perUser` would break the shared-row
   admin's working repair path. `needsAttention` = `state !== "" && state !== "live" && state !== "not_applicable"`
   (the superset that includes `shared_expired`: text, no button).
2. **Data flow — the shell's existing `/setup/status`, through a context.** NEW
   `ui/src/app/components/wardyn/model-access-context.tsx`: `ModelAccessProvider({status, onRefresh, children})`
   + `useModelAccessDoor(): ModelAccessDoor & {refresh: () => void; claim(): () => void}` — `claim()` is the door-ownership mechanism (round-2 UX B1): an effect-registered ref count; a page surface that renders its own sign-in control (the rail on `/runs/new`, the failure block on a credential-FAILED run, the held-approval row while a run is held) calls it on mount and releases on unmount, and the strip renders its button only while the count is 0 (its sentence stays), fail-open default (`needsAttention:false`)
   with no provider above so any bare-mounted screen/test renders today's output. App.tsx wraps `<Routes>` in
   it beside `AttentionPublisherProvider` (:564) and adds `const MODEL_ACCESS_POLL_MS = 300_000` +
   `usePoll(refreshSetupStatus, MODEL_ACCESS_POLL_MS, auth !== "authed")` — **12 req/hr/tab**, 1/60th of one
   poller the shell already runs, visibility-aware (hidden tabs skip; return-to-visible fires at once, which
   closes the "tab open for hours" hole without a faster cadence). Rejected: a `model_access` field on `/me`
   (a site-config read + an AWS-SSO blob decrypt on the most-polled route, for every caller); a new endpoint
   (a third copy of `setupModelAccess`'s wiring). A context, not AppShell props: the shell diff is 2 lines, the
   failure block (F3) and the rail (F1) get the predicate with no prop drilling, and `new-run-screen.tsx`
   (995 lines) gets a **zero-line** diff.
3. **The door = ONE component, ONE Dialog, three callers.** NEW `ui/src/app/components/wardyn/model-access-banner.tsx`
   (~130 lines) mirrors `member-mode-banner.tsx` byte-for-byte in structure: the `MODEL_ACCESS_BANNER` DRAFT
   block, `ModelAccessSignInDialog` (copies `connection-cards.tsx:705-714`'s `style` override verbatim, with
   its comment's citation; `<HarnessLoginPane provider="aws" startURLManaged={door.perUser} onDone={() => {close();
   refresh();}} onCancel={close}/>` — the same `startURLManaged` rule `agents-tab.tsx:206-210` /
   `connection-cards.tsx:720` state), and `ModelAccessBanner`. Rendered **LAST** in the shell's banner stack
   (after the session-expiry strip, below `app-shell.tsx:623`) for the shipped reason at :580-585 — a dead
   control plane or an unknown identity is the better explanation and is read first; NOT hidden in focus mode,
   `z-50`, for the reason at :563-565 — and because Finding 4's mid-run re-auth needs exactly this surface on
   the cockpit. **NOT rendered on `/setup`; on `/settings` and `/providers` withheld ONLY when the viewer is an
   operator** (`useLocation()` + `useOperator()`; UX round B1/S12: for a MEMBER the Settings card's AWS button
   is `disabled={!operator}` — `connection-cards.tsx:477`, `:660`, "Requires the admin role." — so hiding the
   strip there would strand exactly the person the refusal sentence sends there; `/providers` is admin-only
   and mounts its own pane, `agents-tab.tsx:206-222`): for an operator those pages already mount the pane
   for the same states (`member-getting-started.tsx:358-372`, `connection-cards.tsx:705-725`); a second control
   with the accessible name "Sign in to AWS" on one page is the U-13 defect `member-getting-started.tsx` already
   fixed once, and those pages' own status copies would go stale after a strip sign-in.
   **Relevance and dismissability (third-party review; UX round rules — O-11).** `perUser` already is the
   deployment-level relevance test (the org enabled a claude-code per-user row = every member is expected to
   sign in). But `not_configured` is, in the server's own words, "NOT an error — the first-run state"
   (`modelaccess.go:181-183`), and a member who never launches an agent would otherwise read a warning on
   every page forever. Default: `not_configured` AND `shared_expired` carry a **"Not now"** control that hides the strip for
   THIS viewer in THIS browser session (`sessionStorage` key `wardyn.modelAccessDismissed.<me.sub>`, wrapped
   in try/catch — `sessionStorage` is per browsing context and survives a sign-out in the same tab, so an
   unkeyed flag would pre-dismiss the strip for the NEXT person on that tab; round-2 UX S3) while the rail
   line and Getting Started keep saying it; `shared_expired` is included because it is the one state where
   the viewer cannot act at all and an undismissable actionless nag on every screen forever is worse than
   the first-run case (round-2 UX S4); `expiring` and `expired_signin` stay undismissable — they are lapses
   of something the person already had. The `role="status"` region re-announces only on a state CHANGE (key the element on `state`, not on
   every poll). (Tone is ruled ONCE, below, under "Tone".) Principle: one PRIMARY recovery action per state per screen — a second occurrence of the same
   action for a DIFFERENT state (the run door for a failed run while the strip shows `expiring`) is not the
   defect; competing duplicates for the same state are.
4. **What each state renders.** The server's `action` renders beside the sentence ONLY where it carries what
   the sentence and the button cannot (round-2 UX S1): `modelAccessSignInAction` is byte-identical to the
   button label `AGENTS.SIGN_IN_AWS` (`modelaccess.go:207` / `workspace-providers-copy.ts:247`), so for
   `not_configured` and a plain `expired_signin` it would print the button's label as prose; for `expiring`
   the sentence already carries the deadline. Rule, strip and rail alike: render `door.action` when
   `door.action !== AGENTS.SIGN_IN_AWS && door.state !== "expiring"` — i.e. the pin-contradicted pair and
   `shared_expired`'s admin line. "**button**" below = the member-mode strip's underlined TEXT button
   (`member-mode-banner.tsx:195-203`, `className="font-medium underline underline-offset-2"`), never a
   `default` (teal) Button — CONSOLE-RULES §6 allows one teal control per surface and this strip is on every
   surface (round-2 UX S13).

   | Audience / row | `state` | Banner |
   |---|---|---|
   | member or admin, per_user | `not_configured` | `NOT_SIGNED_IN` + action + **button** |
   | member or admin, per_user | `expired_signin` (lapsed OR pin-contradicted — one sentence true of both) | `EXPIRED` + (the action only when pin-contradicted, where it carries the two account/role pairs) + **button** |
   | member or admin, per_user | `expiring` | `EXPIRING` + action ("Sign in again before {ts}") + **button** |
   | member or admin, per_user | `live` | nothing |
   | admin-token principal, per_user | `not_applicable` | nothing (`modelaccess.go:190`) |
   | member, shared | `shared_expired` | the server's action ALONE ("Your admin's model credential expired — ask them to reconnect it") + **no button** (S13: no second sentence saying the same fact) |
   | member, shared | `live` (incl. the admin's `expiring`, folded) | nothing |
   | admin, shared | `expiring` / `expired_signin` | `SHARED_ADMIN_EXPIRING` / `SHARED_ADMIN_EXPIRED` (S2: names the blast radius — every run rides it) + action + **button** (the working repair path; `startURLManaged=false`) |
   | legacy, no roster | `""` (zero `SetupModelAccess`, :421) | nothing |
   | any, on `/setup` | any | nothing — the page is the door |
   | operator, on `/settings` or `/providers` | any | nothing — those pages mount the pane for an operator |
   | member, on `/settings` | actionable | the strip (the member's only door on that page) |

**Fix steps.** (1) NEW `lib/model-access.ts`. (2) NEW `wardyn/model-access-context.tsx`. (3) NEW
`wardyn/model-access-banner.tsx`; lift `connection-cards.tsx:715`'s literal "Sign in with AWS SSO" into
`MODEL_ACCESS_BANNER.DIALOG_TITLE` in the same commit (one spelling). (4) `App.tsx`: the provider around
`<Routes>`, the poll constant, the `usePoll` line (~6 lines). (5) `app-shell.tsx`: one import + one
`<ModelAccessBanner />` after the session-expiry block — **2 lines**, no prop change. (6)
`docs/design/CONSOLE-RULES.md`: **re-cite ALL THREE app-shell citations by hand in the same commit** —
`app-shell.tsx:368–374` and `:413` (:35) and `:570` (:37, :215) — because the guard CANNOT see this drift:
`console-rules-citations.test.ts` checks only bounds (start ≤ end ≤ EOF) plus that `navLinkClass`'s
declaration lies inside its cited RANGE (:86-99, :118-164); one added import at the top shifts every
citation by +1 and the guard stays green while `:413` and `:570` point one line off. Read each cited line
after the edit and confirm it is still the line the doc describes. (7) `internal/api/modelaccess.go:268-274` —
`Deadline` goes on the wire (`json:"deadline,omitempty"`; the doc comment's "no second wire copy" reason is
replaced by the localisation reason) — SAFE for a member under a shared row only because `memberModelAccess`
(:311-327) builds a FRESH struct that drops it, which its own doc (:289-310) names as the leak it exists to
close — pin it: Go test "a member under a shared row gets no `deadline` on the wire" (round-2 general S7).
(8) `lib/types/setup.ts` — `deadline?: string` on `SetupModelAccess` (hand mirror). (9) the two existing
renders (`member-getting-started.tsx:353`, `agents-tab.tsx:179`) switch to
`AGENTS.MODEL_ACCESS_EXPIRING_ACTION(absoluteTime(deadline))` with the verbatim-`action` fallback.
**Strings module (round-2 general S8):** `wardyn/copy.ts` is **961 lines** (cap 1000, no allowlist) — every
new model-access / re-auth string (`MODEL_ACCESS_BANNER`, `RAIL_MODEL_ACCESS`, `REAUTH_ROW`, `REAUTH_HEADING`,
`waitingReauth`) lives in ONE NEW module `ui/src/app/components/wardyn/model-access-copy.ts`; `copy.ts` takes
only the one-line `WIRE_TO_COPY` / `APPROVAL_KIND_LABEL` additions.

**DRAFT strings** (NEW `wardyn/model-access-copy.ts` — `copy.ts` is at 961 lines; the button label is `AGENTS.SIGN_IN_AWS`, reused never retyped):
```ts
// DRAFT (M2 canon pending) — ruled by the UX round (S2, S3, S13, nits)
export const MODEL_ACCESS_BANNER = {
  NOT_SIGNED_IN: "You are not signed in to AWS — Claude Code runs you launch are refused",
  // "lapsed" would be FALSE for a pin-contradicted session, which is live and the wrong identity
  // (modelaccess.go:213-221) — one sentence true of both (round-2 UX B5); the server's action carries the pair.
  EXPIRED: "Your AWS sign-in no longer works for Claude Code — runs you launch are refused",
  EXPIRED_SHORT: "Your AWS sign-in no longer works for Claude Code",   // when the page has claimed the door (S7)
  EXPIRING: "Your AWS sign-in lapses {when}",            // {when} = relativeTime(deadline) ("in 3h"); absoluteTime as the title (B6)
  SHARED_ADMIN_EXPIRING: "The shared AWS sign-in every Claude Code run uses lapses {when}",
  SHARED_ADMIN_EXPIRED: "The shared AWS sign-in no longer works — every Claude Code run is refused until you sign in again",
  // shared_expired (member): the server's action line renders ALONE — no sentence of ours.
  DIALOG_TITLE: "Sign in to AWS",                       // one spelling with the button
  DIALOG_DESCRIPTION: "Signs you in to your organization's AWS access portal for your Claude Code runs. The sign-in runs in the terminal in this dialog.",   // <DialogDescription className="sr-only">
  SIGNED_IN_TOAST: "Signed in to AWS — your runs can use your session now",
  NOT_NOW: "Not now",                                   // not_configured only, for this session (O-11)
} as const;
```
(The sentences say "refused", not "or fail their first model call": every model-calling shape is refused at
create for these states — see the shared facts.)
   **Tone (S3, refined by round-2 S14):** `expiring` renders as a STATE, not an alarm and not chrome —
   `border-b border-info/25 bg-info-subtle text-info` with the `Clock` glyph (CONSOLE-RULES §2 reserves the
   state tones for exactly this and pairs colour with a word; `bg-muted/40` would read as a layout hairline;
   the console's existing "a state, not an alarm" idiom is `attach-terminal.tsx`'s read-only chip) with the
   button — full amber on every screen for 24 h in the same tint as "Control plane unreachable" trains
   members to ignore amber; amber (`bg-warning-subtle`) only for `not_configured`, `expired_signin`,
   `shared_expired`; the rail's expiring line uses the same muted tone so one screen never shows two tones
   for one fact. **Deadline (B4):** the server's `action` for `expiring` carries an RFC3339 UTC stamp
   ("Sign in again before 2026-09-19T14:03:22Z"); on every screen for 24 h a member in another timezone
   misreads it. `SetupModelAccess.Deadline` goes ON THE WIRE (`json:"deadline,omitempty"` — reversing the
   "no second wire copy" note at `modelaccess.go:270-274`, for this reason) and the strip/rail render
   `{when} = relativeTime(deadline)` ("in 20m", "in 3h" — `lib/format.ts`'s existing helper) in the strip's and
   the rail's EXPIRING sentences, with `absoluteTime(deadline)` as the element's `title` (round-2 UX B6:
   `absoluteTime` is `toLocaleString` with SECONDS — "Sep 19, 2026, 02:03:22 PM" — and a deadline 20 minutes
   out, reachable for a no-refresh-token blob whose ACCESS token lapses, needs urgency, not a calendar stamp);
   fall back to `action` verbatim when `deadline` is absent (an older daemon). The two existing card renders
   (`member-getting-started.tsx:353`, `agents-tab.tsx:179`) switch to
   `AGENTS.MODEL_ACCESS_EXPIRING_ACTION(absoluteTime(deadline))` in the same commit — the frozen "Sign in
   again before %s" cannot take a relative string.
   **Dialog close (S4):** Esc / overlay-click on the door dialog while a login run is live routes to the
   pane's `cancel` (kill the run, close the auth tab) — `closeLogin` in `connection-cards.tsx:421-424` only
   clears state and would orphan a "wardyn: sign-in running" run for up to 30 min on the member's board; the
   dialog exposes `onCancel` and the tab lane's unmount cleanup closes the tab. **Confirmation (S5):** on
   `onDone` the door emits `toast.success(MODEL_ACCESS_BANNER.SIGNED_IN_TOAST)` (CONSOLE-RULES §9's transient
   case) and moves focus to `#main-content` (`app-shell.tsx:636`, `tabIndex={-1}`) — Radix would return focus
   to a trigger that no longer exists once the strip vanishes (when the dialog was opened from a page
   control — the rail, the block, the row — focus returns to THAT control's neighbour instead: Launch on New
   Run). A11y note for the lane: `role="status"` announces changes, not mount content, and the skip-to-main
   link routes a keyboard/SR user past the strip — pre-existing with `member-mode-banner`; the strip's
   sentence is repeated by the rail and Getting Started, which is why that is acceptable. **Live region:** `role="status"` re-announces
   on a state CHANGE only (key on `state`).

**Tests, red first.** NEW `lib/model-access.test.ts`: a claude-code per_user bedrock_sso row makes the door
perUser · a **codex** per_user row does NOT · `shared_expired` needs attention and is not actionable ·
`not_applicable` needs no attention · absent `model_access` (legacy) needs no attention · `expiring` is
actionable for an admin under a shared row. NEW `wardyn/model-access-banner.test.tsx` (mirrors
`member-mode-banner.test.tsx`): nothing for live / absent / not_applicable · not_configured renders the
sentence, the server action verbatim and the button · shared_expired renders the admin line and NO button ·
the button opens the pane and `onDone` calls `onRefresh` · the strip is `role=status`; it renders `NOT_NOW` for `not_configured` and `shared_expired` (keyed on the viewer's subject) and NO dismiss control for `expiring` / `expired_signin`.
`app-shell.test.tsx`: the banner renders below the session-expiry banner (DOM order) · no banner on `/setup`
or `/settings`. `console-rules-citations.test.ts`: NO edit, and NOT a proof — it does not pin `:570`; the hand re-cite
is verified by reading the cited lines (the lane's REPORT quotes each). Playwright NEW `ui/e2e/model-access-banner.spec.ts` (pattern
`member-mode.spec.ts`): a never-signed-in member sees the banner on the Runs board and on New Run · the
strip's button opens the pane without leaving the page · no banner on Getting Started · an admin with a live
credential sees no banner. Live (handed to `e2e-sso-path`): **case I (banner)** — sign in as the member before
any capture, `/runs` shows `MODEL_ACCESS_BANNER.NOT_SIGNED_IN` with `AGENTS.SIGN_IN_AWS` as a button in the
strip; still there on `/workspaces`; sign in from the strip (the same fake device flow case B uses) → the strip
is gone without a reload. Full `pnpm test:coverage --maxWorkers=4`.

**Docs / CHANGELOG (canon).** Added: "**A global model-access banner.** An actionable model-access state —
not signed in, lapsed, or lapsing within 24 h — now rides a strip on every screen, in focus mode and on the
run cockpit, carrying the sign-in itself in a dialog; a lapse cannot be dismissed, the first-run state and a
dead shared credential can be set aside for the session.
Suppressed on Getting Started and Settings, which already mount the sign-in. Read once per session plus a
5-minute visibility-aware poll; no new endpoint and no new field." `CONSOLE-RULES.md`: the `:570` re-cite
(guard-forced). Retire the 0.7.5 Known gap "A member on a shared Bedrock row whose ADMIN credential is
expiring is still offered the member-side sign-in CTA" with the `harnesscred.go:708-710` citation: it
describes a working path (Contradictions #3).

**Risks / open.** (a) RESOLVED by the UX round: the Dialog is `z-50` (`dialog.tsx:41,66`) over the focus-mode
overlay's `z-40` (`focus-mode.tsx:107`); the CSP has no `sandbox` directive and admits inline styles
(`security_headers.go:54-58`) — verify once by hand anyway, in focus mode, before reporting. (a2) Known gap to
record in canon: a member under a dead SHARED credential is told and has nothing to do — Wardyn offers no way
to notify the admin (UX round, persona e). (b) `MODEL_ACCESS_POLL_MS` is a named module const — a field report moves one number. `RequireSetup` gates once
per load (`App.tsx:281-283`), so the 5-min status poll cannot re-bounce a member to the setup gate. (c) A member
who signs in from the banner while on a run cockpit gets a fresh status; the run does not retry itself —
that is `credential-reauth-hold`.

**Size.** 5 new files (~260 lines incl. the strings module), 6 edited (App.tsx, app-shell.tsx, modelaccess.go,
setup.ts, the two deadline renders; ~30 lines), 3 doc citations; ~8–10 worker-hours.

### Lane `ui-new-run-model-access` (W1, **Sonnet** worker + Fable review) — Finding 1

Depends on `ui-model-access-door` steps 1–2 (predicate + context) only.

**Root cause.** `llmReady` (from `hasLlmPath`) is a deployment fact — true the moment a per-user Bedrock row
exists — so the warning is structurally unreachable for the person it was written for, and its sentence
("No model provider is connected") is FALSE on that deployment: the deployment is connected, the person is not.

**Design.** The rail calls `useModelAccessDoor()` itself; **`new-run-screen.tsx` gets a zero-line diff**.
The existing `showModelWarning` prop keeps its exact meaning (a deployment with no LLM path at all) and its
sentence; the model-access line is a second, independent line in the same `Credentials` section, gated on
`agentRow?.id === MODEL_ACCESS_AGENT && door.needsAttention` (the selected agent is the one `model_access`
grades; a codex row renders nothing whatever `model_access` says; a shell command withholds `agentRow`).
**The rail carries its own sign-in (UX round S1):** the eye is on Launch, not on a strip above the header;
U-13's actual rule (`your-model-key.tsx:275-300`, `copy.ts:691-697`) is two controls with DISTINCT accessible
names, the duplicate hidden while the pane is open. So the rail line ends in a `link`-variant "Sign in to AWS"
(`aria-label="Sign in to AWS — from the New Run rail"`) that opens the SAME door dialog through the context
and is hidden while it is open; the rail calls `door.claim()` while it renders its control, so the strip on
`/runs/new` keeps its sentence and drops its button — the ONE rule the Principles state, no New Run exception
(round-2 UX S2). Focus returns to Launch when the dialog was opened from the rail (not to `#main-content`,
which would drop the person at the top of the form they were mid-way through). **Launch is NOT blocked client-side:** the server is the gate
(create 422s every model-calling shape and the rail renders that sentence untruncated at :317-321; scan/exec
shapes need no credential); a client-side block would be a second, driftable copy of `isModelRun`. `//
ponytail:` mark it. `showCredentials` gains `|| showModelAccess`; `showCredentialFacts` unchanged (a
`not_configured` per_user row still legitimately says where the sign-in WILL land).

| Row / audience | `state` | Rail `Credentials` section |
|---|---|---|
| per_user (member or admin) | `not_configured` | `RAIL_MODEL_ACCESS.NOT_SIGNED_IN` + `door.action` verbatim, warning tint, above `CredentialFacts` |
| per_user | `expired_signin` | `RAIL_MODEL_ACCESS.EXPIRED` + `door.action` (carries the pair on a pin mismatch) |
| per_user | `expiring` | `RAIL_MODEL_ACCESS.EXPIRING` + `door.action` — muted text, not the warning tint (the session still signs; `connection-cards.tsx:464` grades `expiring` as Connected) |
| per_user | `live` | nothing new (today's `SANDBOX_BEDROCK` + `AWSSignInChip perUser`) |
| shared, member | `shared_expired` | `RAIL_MODEL_ACCESS.SHARED_EXPIRED` + `door.action`, no CTA |
| shared, admin | `expiring` / `expired_signin` | as per_user — it is the admin's own credential |
| any | `not_applicable` / `""` (legacy) | nothing — today's rail byte for byte |
| agent ≠ claude-code, or `!isAgent` | any | nothing |

**Fix steps.** (1) `wardyn/model-access-copy.ts` (the door lane's new module; `copy.ts` is at 961 lines): `RAIL_MODEL_ACCESS`, ~14 lines. (2)
`new-run-rail.tsx`: `useModelAccessDoor()`; `showModelAccess`; a small local `ModelAccessLine` (sentence by
state + `door.action`); widen `showCredentials` (~35 lines, no prop change). (3) `new-run-screen.tsx`: **no
change.** (4) Lift the existing inline warning literals (:261-268) into `RAIL_MODEL_ACCESS.NO_PROVIDER` /
`NO_PROVIDER_CTA` in the same commit (strings law; the last inline strings in the file).

**DRAFT strings** (`wardyn/model-access-copy.ts`):
```ts
// DRAFT (M2 canon pending) — the rail's per-PERSON model-access lines. Distinct
// from RAIL_CREDENTIAL, which states where the credential LANDS: these state
// whether the person launching has one at all. "refused at launch" is the
// server's word: create answers 422 for every model-calling shape while the
// declared per-user lane is unsatisfied (enforceCreateLLMMechanism); a scan or
// exec run needs no model credential and shows no line.
export const RAIL_MODEL_ACCESS = {
  // UX round S1: "This run is refused at launch." was a verdict on a run that does not exist yet.
  NOT_SIGNED_IN: "Sign in to AWS before you launch — Launch is refused until you do.",
  EXPIRED: "Your AWS sign-in no longer works for Claude Code — Launch is refused until you sign in again.",   // true of a pin mismatch too (B5)
  EXPIRING: (when: string) => `Your AWS sign-in lapses ${when} — this run launches; sign in again soon.`,
  SHARED_EXPIRED: "Your admin's AWS credential has expired — Launch is refused until they reconnect it.",
  SIGN_IN_ARIA: "Sign in to AWS — from the New Run rail",
  // The existing inline warning, lifted (strings law) — a DEPLOYMENT with no
  // model path at all, a different fact from the four above.
  NO_PROVIDER: "No model provider is connected. This run launches; its first model call fails.",
  NO_PROVIDER_CTA: "Connect →",
} as const;
```

**Tests, red first.** `new-run-rail.test.tsx`: a claude-code per_user row with `model_access` not_configured
states the person is not signed in and the server's action verbatim — **RED today: the rail renders nothing**
· …and renders the rail's own link control under the DISTINCT accessible name `RAIL_MODEL_ACCESS.SIGN_IN_ARIA`,
hidden while the door dialog is open (the two names never collide — `getByRole("button",{name: AGENTS.SIGN_IN_AWS})`
must not match the rail's control) · expiring renders the deadline line and the run is not called refused · a member's shared_expired
states the admin's action and offers no CTA · a **codex** row with an actionable claude-code `model_access`
renders nothing · `model_access` live leaves today's rail byte-identical · with no `ModelAccessProvider`
above, the rail is today's rail (the fail-open contract that keeps the ~15 existing rail cases green) · the
existing `showModelWarning` cases stay green unchanged. Run `new-run-screen.test.tsx` (the rail renders
inside it). Playwright `ui/e2e/new-run.spec.ts`: extend "New run rail — credentials and recording are read,
not asserted" (:456) with a fixture whose `/setup/status` carries `model_access:{state:"not_configured",
action:"Sign in to AWS"}` and a per-user claude-code row → `RAIL_MODEL_ACCESS.NOT_SIGNED_IN` visible, and a
Launch click shows the 422 sentence in the rail (the existing `launch.error` path). Live (handed to
`e2e-sso-path`): extend **A(rail)** (:377) — `RAIL_MODEL_ACCESS.NOT_SIGNED_IN` visible and
`RAIL_MODEL_ACCESS.NO_PROVIDER` has count 0 (the finding-1 negative). Full vitest.

**Docs / CHANGELOG (canon).** Fixed: "**New Run's model warning is about the person, not the deployment.** It
was keyed on a deployment-wide integration count — true the moment an admin saves a `per_user` roster row —
so a member who had never signed in filled in the whole form and was refused at the click, and the one
sentence that would have warned them said *'No model provider is connected'*, which on that deployment is
false. The rail now reads `model_access` when the selected agent is the one it grades and states which of the
four per-person states the launcher is in, in the server's own words. The deployment-level warning is
unchanged." Retire the 0.7.5 Known gap "The New Run rail still paints its credential chip from the ROSTER
ROW alone" — move it out of Known gaps.

**Risks.** Launch stays enabled by design (server gate); do not claim the server's `llm_ready` field was
misread — the rail never read it. **Size.** ~47 lines + ~7 test cases; ~4–5 worker-hours.

### Lane `run-credential-door` (W1, **Opus** worker + Fable review; Go + UI) — Finding 3

Depends on `ui-model-access-door` steps 1–3.

**Root cause.** The dispatch refusal is a complete, correct sentence with a destination in it and no
machine-readable class, so the console can only render it as prose under kind `unknown`.

**Design.** (i) **The signal = one map key, not a column and not a new action:** add `"reason":
llmRefusalAuditReason` (`= "model_credential"`, a const beside the other copy) to the existing emit's map at
`runs_dispatch_llm_mechanism.go:285-287`. Rejected: a `failure_kind` column (migration, `runInsertCols`/
`runCols`, TS mirror, a second truth beside `FailureHint`); a distinct action name (a new AUDIT-ACTIONS row +
forward-guard entry that splits a family the doc documents as one). (ii) **The reason is deliberately NOT
narrowed to "a sign-in repairs it":** the server says the failure was about a model credential; the console
decides whether to offer a door with the shared predicate — correct for free in every case: refresh SPENT →
`expired_signin` → door; refresh UNAVAILABLE ("launch again in a moment", `awssso_refresh.go:152-154`) → the
blob grades `live` → **no door** (right); pin contradiction → force-graded `expired_signin` (:443-448) → door;
per_user never signed in → door; shared member → `shared_expired` → no door, sentence only; shared admin →
door. (iii) Client: `RunEndingKind` gains `"credential"`; `runEndingFromAudit` matches `run.create` +
`outcome==="failure"` + `data.reason===CREDENTIAL_REASON` AFTER the `FAILED_CAUSE` scan (an image/selftest
failure is the earlier cause), built with `detail: undefined` (`data.error` is byte-identical to
`failure_hint`, which the block already renders — never twice). (iv) **No `ENDING_COPY` row for
`credential`**: the server's sentence IS the reason; `!copy && run.failure_hint` (:163) renders it unchanged
and the only addition is the door under it. (v) **The server sentence's DESTINATION changes for the per-user lane (UX round B1):** "sign in again under
Settings → Model provider" sends a member to a page whose AWS button is admin-only (`connection-cards.tsx:477`),
and says "again" to a person who has never signed in. For a `per_user` row the clause becomes "— sign in to
AWS from Getting started in the console, or from the sign-in banner the console shows on every page." (round-2
UX S5: Getting started is the only destination true on all three surfaces the sentence reaches — the rail's
422, the failed run's block in focus mode where the block owns the door and the strip has no button, and the
CLI, whose reader has no banner; "Getting started" is sentence case, as the nav item and page title are,
`app-shell.tsx:818`, `copy.ts:653`) and the `not_configured` arm drops "again"; for a shared row the Settings destination stays (it is the admin's door). Both `llmMechanismDeadSentence`
/ `llmMechanismPinContradictedSentence` (`runs_dispatch_llm_mechanism.go:46-64`) and `awsSSORefreshSpentSentence`
(`awssso_refresh.go:137-139`) are `DRAFT (M2 canon pending)` constants — check first whether any is reproduced
in a FROZEN §7 table; if so the change goes through the canon row, never a direct edit. The CLI reads the same
sentence and the same destination is right for it. The console's answer to any remaining redundancy is the
button directly under the sentence. (vi) **Header chip and Runs board: no
change** — the board has no chip; the header chip is `max-w-[160px]` and a third "Sign in to AWS" control on a
page that has the banner and the block is the U-13 defect a third time; relaunch is the header's existing
"Start a run like this one".

| Audience | model_access | Failure block (run FAILED, kind `credential`) |
|---|---|---|
| member/admin per_user: not signed in / lapsed / pin-contradicted / expiring | actionable | server sentence + **Sign in to AWS** (opens the door's dialog) + `CREDENTIAL_DOOR_NOTE` |
| member, shared | `shared_expired` | sentence only, no door |
| any, admin's credential recovered since | `live` | sentence only |
| refresh-unavailable | `live` | sentence only — correct, no door |
| non-credential failure | — | unchanged |

**Fix steps.** (1) `runs_dispatch_llm_mechanism.go:285-287` — the const + the map key (2 lines). (2)
`docs/AUDIT-ACTIONS.md:56` — add `reason` to the `run.create` failure data-key cell and re-run `go test
./cmd/wardynd/ -run AuditActions` to re-point the `:290` citation (window 0; it moves by 1–2). (3) Go test: the
refusal's audit row carries `reason`; the create-time 422 twin does not audit (unchanged). (4)
`lib/types/audit.ts:149-153` — `| "credential"` with its doc line. (5) `lib/api/audit.ts` —
`CREDENTIAL_REASON` + the match after the `FAILED_CAUSE` scan (~10 lines). (6)
`run-detail/failure-block.tsx` — `useModelAccessDoor()`; when `ending.kind==="credential" && door.actionable
&& run.created_by === me` (the door is the VIEWER's credential — an admin reading a member's failed run must
not be offered a sign-in that repairs nothing for that run; the viewer's subject comes from the same `/me`
the shell holds) render the button + note under the hint, reusing `ModelAccessSignInDialog`; and call
`door.refresh()` ONCE on mount when the ending is `credential` (the context can be up to 5 min stale, and a
just-refused run is exactly when the spent mark flipped) (~18 lines). The RUN PAGE owns the button while it
has one: the strip on `/runs/:id` renders its sentence WITHOUT the button when the page has claimed the door
(`door.claim()` in an effect from this block, and from the held-approval row in `credential-reauth-hold`) —
the row while held, the block when FAILED, the strip's button otherwise. (7) No change to
`run-card.tsx`, `run-detail-summary-header.tsx`, `run-detail.tsx`, `internal/store/`.

**DRAFT strings** (`failure-block.tsx`, local by the rule at :37-40): `// DRAFT (M2 canon pending)` `const
CREDENTIAL_DOOR_NOTE = "Sign in here. This run stays failed — relaunch it from the run header.";` (round-2 UX B7: "Start a run like this one" lives in `run-detail-summary-header.tsx:83`, which focus mode does not render — `focus-mode.tsx:104-118` portals only `ctx.terminalPane`)
(UX round S7: the block also renders inside focus mode, `run-detail.tsx:572` → `focus-mode.tsx:118`, where
there is no header). Button label `AGENTS.SIGN_IN_AWS` with `aria-label="Sign in to AWS — for this failed run"`
(S8; precedent `SIGN_IN_AWS_ARIA_CARD`, `copy.ts:646`).

**Tests, red first.** `lib/api/audit.test.ts`: a FAILED run whose run.create failure carries reason
model_credential grades `credential` — **RED: kind is `unknown`** · a run.create failure with no reason still
grades `unknown` · run.build/failure still wins over a later credential run.create · the credential ending
carries no detail. `failure-block.test.tsx`: a credential ending renders the server sentence and a Sign in to
AWS button — RED · …and renders the sentence exactly once · with shared_expired model access: sentence and NO
button · with model access `live`: no button (refresh-unavailable) · existing "a FAILED run with failure_hint
shows the bare server text" (run-detail.test.tsx) and the `:245-250` "'Start a run like this one' is not in
this block" stay green. Go `runs_dispatch_llm_mechanism_test.go`: the declared-mechanism refusal audits
reason=model_credential (through the const). Guards: `go test ./cmd/wardynd/ -run AuditActions` both
directions. Playwright (`ui/e2e/runs.spec.ts` or `audit.spec.ts`): a fixture FAILED run whose trail carries
the reason shows the sentence and the button. Live (handed to `e2e-sso-path`): **case J (run door)** needs a
run that reaches dispatch with a dead credential — `credential-reauth-hold`'s test seam (a control-plane
override that marks the owner's blob spent / an awsssofake `invalid_grant` switch) is the way to force it;
J lands when that seam exists, else the Playwright fixture is the pin. Full vitest.

**Docs / CHANGELOG (canon).** Added: "**A run refused for a model credential now offers the sign-in, not
directions to it.** The dispatch refusal's audit row carries `reason: model_credential`, the console grades
that ending `credential`, and the run's failure block shows the server's own sentence with the AWS sign-in
beside it — in a dialog, on the run page. The button appears only where a sign-in this person can complete
repairs the state: a member under a shared credential gets the sentence and no button, and a refusal whose
renewal merely did not complete ('launch again in a moment') gets no button either. Relaunch is the header's
existing 'Start a run like this one'." `docs/AUDIT-ACTIONS.md:56` cell + citation (guard-forced). The server
sentence is unchanged — no `workspace-providers-prompt.md` §7 movement.

**Risks / open.** `data.reason` on a `run.create` failure row: `runEndingFromAudit` reads it only as a fallback
after `data.error`, which is always present on this emit — confirm with the existing audit tests; the CLI's
`runFailureReason` prints `data.error` — read it once to confirm it does not enumerate keys. **Size.** 2 Go
lines + 1 test, 3 UI files (~25 lines), ~8 test cases, 2 doc cells; ~5–6 worker-hours (mostly the citation
ratchet).

**Sequencing of the three:** `ui-model-access-door` lands (incl. the CONSOLE-RULES re-cite) → the other two
rebase and run in parallel; all three run the FULL vitest.

### Shared facts for the credential lanes (F4, F5, F8) — verified at dfa89f60

- **The premise of F4 does not match the code on the per-user lane.** `internal/egress/proxy/mitm.go:176-182`
  excludes Bedrock from MITM ("client-side SigV4 a terminate-and-reforward proxy would invalidate");
  `resolveBedrockAuth` (`runs_bedrock.go:471-701`) resolves the credential ONCE at dispatch (`refresh=true` only
  there, :445-449); the ssoInject arm (:587-635) writes a synthetic `~/.aws` into the sandbox via
  `WARDYN_AWS_SSO_CONFIG_B64` (:327; materialized by `agent-run-lib.sh`): config (:343-344) + the token cache
  `awsSSOCacheFileContents` (:390-410) carrying accessToken + expiresAt ALWAYS and WITHHOLDING
  refreshToken/clientId/clientSecret when the blob has a refresh token ("ONE REFRESHER PER TOKEN … CreateToken
  ROTATES the token", :369-386). The sandbox SDK exchanges the access token for short-lived role credentials
  itself at `portal.sso.<r>` GetRoleCredentials (header `x-amz-sso_bearer_token`, authtype:none) and refreshes
  nothing; "A run that outlives its access token therefore fails visibly at its first model call" (:385-386);
  `awssso_refresh.go:26-30`: "a mid-run renewal channel is a later change". **The code names its own next step
  at `runs_bedrock.go:568-574`: "PHASE B (not yet built): proxy-inject the token as the `x-amz-sso_bearer_token`
  header on portal.sso.<region> instead of writing it into the sandbox — that call is authtype:none (unsigned),
  so a MITM can set the header without the sandbox ever holding the token, mirroring the Bedrock bearer path".**
- **The machinery Phase B reuses is in production for the Bedrock bearer and subscription lanes.**
  `authorBedrockBearerInjection` (`runs_dispatch_llm.go:530`, called :722) authors an `api_key` grant + a
  port-qualified MITM entry; the per-run MITM CA is provisioned when any consumer needs it (:695-699) and
  `sandboxCATrustVars` (:390-397) sets `NODE_EXTRA_CA_CERTS` + `AWS_CA_BUNDLE`; `proxy.go:828`
  `isCorpMITMHost` → `mitmConnect`; `mitm.go:455` `p.inject.resolve(host)` — a resolve ERROR fails closed
  (502, :456-460); **`injector.resolve` (`inject.go:135`) is already a mid-run, per-host single-flighted
  (`reMu` :72-77,:150), fail-closed, expiry-driven re-resolve loop**: a dynamic entry (`expiresAt != 0`) is
  re-resolved within `injectRefreshMargin` = 5 min (:51) by `resolveInjection` (:457, `GET
  /api/v1/internal/injection/{grantID}`, run-token bearer) — it is what refreshes the subscription OAuth token
  today. `handleInternalInjection` (`injection.go:74`) = the sentinel-secret pattern (`oauthProviderForSentinel`
  :47), host pin (`subscriptionInjectionHost` :38, :118), forced header/format (:166-168), `ExpiresAt`
  advertised when the provider has one (:185-187); the grant is `RequiresApproval:false` so re-mints never hit
  `ErrAlreadyMinted`. The injector's HTTP client is the shared 130 s client (`cmd/wardyn-proxy/main.go:126`) —
  **a server-side block cannot be the hold; the hold is a poll loop in the sidecar, exactly as `ResolveWait`**
  (`approvals.go:289-363`: budget ctx :313, `holdSem` :347, ticker `holdPollInterval` 1 s :116; ceilings
  `defaultHoldTimeout` 30 s :98, `maxHoldTimeout` 600 s :108; `configureHold` :200). `WARDYN_GIT_APPROVAL_TIMEOUT`
  lives in the SIDECAR (`git_broker.go:89-129`, default 120 s, `os.Getenv`, **no upper clamp**).
- **Approvals are the vehicle for the visible re-auth request.** Kinds `credential` (the git-mint approval),
  `egress_domain`, `tool_call` (`internal/types/types.go:363-369`), pinned by a column CHECK
  (`migrations/0001_init.sql:46`, enum-parity guard) → a new kind is a migration + Go enum + TS mirror.
  `approval.RequestApproval` (`internal/approval/approval.go:57-99`) dedups on run+kind+`requested_scope` and
  **emits no audit** (the package doc's "every state change" claim is false for creation); `ExpireStale`
  (:201) sweeps PENDING at `WARDYN_APPROVAL_EXPIRY_AFTER` 24 h; `approval.decide` :167, `approval.expire` :248,
  `approval.cancelled` :321. `handleListApprovals` (`internal/api/approvals.go:79`) is kind-agnostic; the
  cockpit polls `?run_id=` (`run-detail.tsx:174`, `usePoll` 4 s), the header chip is a count
  (`run-detail-summary-header.tsx:327-333`, `passiveHold` :150); `live-approvals.tsx` `isHeld` :102, `rowLabel`
  :113, kind filter :234; `WIRE_TO_COPY` (`wardyn/copy.ts:557`). Writeback/reconcile are kind-guarded
  (`approvals_reconcile.go:186`, `approvals_writeback.go:240`). **`authorizeMemberDecision` (`approvals.go:523-536`)
  admits only `egress_domain` for a non-security-operator** → a member cannot approve/deny a new kind; the
  re-auth item must be a DOOR, not a decision. `maxApprovalsPerRun` (`internal.go:428`).
- **Residency does not change with Phase B.** `threatmodel/THREAT-MODEL.md`'s *Derived AWS role credentials*
  row already says "Phase B would end the SSO token's residency, not theirs"; `gradeModelCredential`
  (`credential_residency.go:100-108`) returns `sandbox` for per_user+bedrock_sso BECAUSE the SigV4 role
  credentials are resident — no `credential_residency` change, no `RAIL_CREDENTIAL.SANDBOX_BEDROCK` change;
  only the captured-SSO row's "not-yet-built gap" prose changes.
- **Capture path.** `PUT /api/v1/internal/sso-token/{runID}` (`ssotoken.go:44-256`; `storeAWSSSOBlob` :216;
  the `run_killed` arm inside the owner lock :203-210; per-run mask :243; audit `harness.credential.captured`
  :246-252). No notify on capture. `awsSSOScopeFor(sc, agent, subject)`; `readAWSSSOBlob` refuses an empty
  owner; `awsSSOScopeIsMechanism`.
- **F5 — the retry already exists and the grading is worse than reported.** `createAWSSSOTokenWithRetry`
  (`awssso_refresh.go:472-483`) retries ONCE after `awsSSORefreshRetryDelay` 400 ms (:85-89) on any non-spent
  error, transport errors included — the 01:09 `EOF` was TWO attempts; the failure audit (:396-399) records only
  the last error. `awsSSOErrorIsSpent` :194-200; `markAWSSSOTokenSpent` :309-316 → in-memory
  `s.ssoRefreshSpent` keyed by fingerprint :239-242 (never stored; `ponytail:` note `server.go:760-769`); a
  transport failure with a still-valid token serves the STALE blob silently (:405-413). **`awsSSOCredentialState`
  (`modelaccess.go:337-363`) tests `blob.renewable(now)` FIRST (:351) and the spent map is consulted by nothing
  in `modelaccess.go` — after an `invalid_grant` the console grades that session `live` until the client
  REGISTRATION lapses (days), while every dispatch refuses the person's runs.** `setup.go:948` is the one
  caller; multi-replica is refused (`claimSingleInstance`, `cmd/wardynd/main.go:177`).
- **F8.** `createAWSSSOToken` (:497-558) `&http.Client{Transport: http.DefaultTransport, Timeout: 10s}` with the
  comment :488-496 (the photo-cut words: "… honours the PROCESS proxy environment exactly as the GitHub broker's
  client does — wardynd's own egress is a separate channel from the sandbox proxy's, and a deployment behind a
  corporate proxy needs this hop to follow it"). Siblings on `http.DefaultTransport` — FIVE wardynd-side consumers: OIDC discovery/JWKS
  (`internal/auth/oidc/jwks.go:187`, `oidc.go:354` — incl. the split-horizon `InternalIssuerURL`, a boot flag
  `*f.oidcInternalIss` consumed at `boot_deps.go:359-362`, a CLUSTER-INTERNAL host that must be bypassed and
  IS known before `installDaemonProxy` runs), audit webhook (`sinks/webhook.go:121`), GitHub App
  (`broker/github.go:148`), Entra sync (`directory/entra.go:106`), AWS SSO `CreateToken`. NOT a daemon
  consumer (round-2 general B2 corrected round 1): the content-scan sidecar client
  (`internal/contentscan/sidecar.go:35-38`) is constructed in the PROXY SIDECAR process
  (`internal/egress/proxy/server.go:140-145`), and `DetectorSidecarURL` is a per-policy DB field
  (`internal/types/policy.go:375-382`) read at dispatch, after `connectAndMigrate` (`main.go:165`) — wardynd's
  transport never dials it and no URL exists at install time. Correctly NOT on it either:
  `uigateway.go:369-374` and `sidecar/sidecar.go:72-73` (`Proxy: nil`), client-go (own transport).
  `x/net/httpproxy` exempts only loopback by default. **`cmd/wardynd/main.go:156-158` already
  mutates `http.DefaultTransport` in place at boot (`installTrustedCA`, `trusted_ca.go:95`)** so every holder
  picks up the change. `docs/ENV.md:343` (HTTP_PROXY row: "forwarded from the operator's shell as build args …
  Never set as wardynd runtime env — see `WARDYN_HOST_PROXY_B64` above for why") and :381-389 (the photo-cut
  words: "It is deliberately not named `HTTP_PROXY`: Go's `net/http` honors the standard names process-wide,
  which would silently reroute wardynd's own OIDC discovery, audit webhooks, GitHub App minting and AWS
  credential chain through the corporate proxy"). **OIDC discovery runs at BOOT** (`boot_deps.go:359`, under
  the 30 s bootCtx) before any SiteConfig read. `SiteConfig.UpstreamProxyURL` (`site_config.go:46`) is
  admin-editable at runtime, validated http-only (`site_config.go:170`, `proxy.parseUpstreamProxy` `upstream.go:56-76`)
  because the sandbox hop is plaintext CONNECT chaining; consumers `resolveUpstreamProxyURL` (`runs_bedrock.go:46-141`)
  → `BuildProxyConfig` (`sandbox.go:200-233`, both substrates) → sidecar. client-go v0.36.4 builds its own
  transport with `Proxy: http.ProxyFromEnvironment` (`transport/cache.go:142`) except a TLS-less kubeconfig
  reuses `http.DefaultTransport` (:113). `golang.org/x/net v0.57.0` is already in go.mod (indirect) —
  `x/net/http/httpproxy` gives `NO_PROXY` semantics with no new module. `envdoc_guard_test.go:21`: any
  `WARDYN_*` read needs an ENV.md row. Test endpoint: `WARDYN_AWS_SSO_ENDPOINT_OVERRIDE` (`awssso_endpoint.go`;
  the kind fake's Service is `wardyn-awsssofake:8090`, plain http, lifted via `internal_hosts`).
- **Migrations.** Last is `0062_approval_cancelled.sql`; `TestEveryMigrationIsDocumented`
  (`internal/db/migrations_documented_test.go`): every migration ≥ 0038 must be named by its FULL filename stem
  in tracked markdown — CHANGELOG.md for anything operator-observable. **Numbers fixed by this plan (do not
  renumber at merge):** `0063_agent_runs_status_detail.sql` (lane `starting-detail`) and
  `0064_approval_credential_reauth.sql` (lane `credential-reauth-hold`).

### Lane `credential-reauth-hold` (W2, **Opus** worker + Fable review; Go proxy + control plane + UI) — Finding 4

**Decision: Phase B built out of the existing grant/injection machinery, plus ~150 lines of WAITING in the
sidecar and one new approval kind.** Rejected — Candidate B (keep the token resident; `Exec` a cache rewrite
into the running sandbox before expiry): needs a scheduler, a per-substrate exec path, a writable-sandbox
assumption BYOI images break, leaves the token resident, and still cannot hold anything without a second
mechanism (a control-plane flag the proxy consults on CONNECT) — i.e. Candidate A's proxy work anyway.

**Sequence.**
```
dispatch (per_user OR shared bedrock_sso, ssoInject)
  resolveBedrockAuth refreshes ONCE as today
  cache file gets PLACEHOLDER accessToken + far-future expiresAt (awsSSOCacheFileContents(b, proxyInjected=true))
  authorBedrockSSOInjection: grant{api_key, scope:{host: portal.sso.<r>[:port], header: x-amz-sso_bearer_token,
                              format: "%s", secret_name: aws-sso-access-token}}; mitmHosts += portal.sso.<r>:443 → MITM CA
  proxy start: buildInjector resolves once → dynamic entry, expiresAt = blob.ExpiresAt
run, later: SDK's role creds expire → GET portal.sso/federation/credentials
  CONNECT portal.sso:443 → allowlisted → isCorpMITMHost → mitmConnect → serveMITMRequest → inject.resolve(host)
      entry inside injectRefreshMargin → re-resolve → GET /internal/injection/{id}
  control plane (NEW resolveAWSSSOInjection):
      snapshot = the grant's dispatch-time scope (I3); scope = awsSSOScopeFor(siteCfg, run.Agent, claims.Sub)
      re-derived from the roster; snapshot != scope → 403 scope_changed, fail closed (never another blob)
      blob = readAWSSSOBlob(scope); blob, reason = refreshAWSSSOBlob(scope, blob)   ← the SAME single-flight
      live → 200 {header, value:<token>, expires_at} + MaskRegistry.Add(claims.RunID, token)  (per run; the
             refresh path's own AddGlobal at awssso_refresh.go:427-428 stays)
      dead → RequestApproval{kind: credential_reauth, run_id, scope:{mechanism, credential_source, owner}}
             + audit credential.reauth.requested → 423 {"state":"reauth_pending","approval_id":…}
  proxy sees 423 {approval_id} → errReauthPending → HOLD (credhold.go): poll the APPROVAL by id
      (GET /internal/approvals/{id}, the existing approvalClient.poll, every holdPollInterval 1 s) under ONE
      budget (WARDYN_CREDENTIAL_REAUTH_TIMEOUT), holding reMu → the GetRoleCredentials request is parked
      — NEVER re-polling the injection URL: each injection resolve re-MINTS (handleInternalInjection →
      Broker.MintForGrant, injection.go:92 → b.mint, broker.go:390-396) and commits a credential.mint audit
      row in the same tx (broker.go:575-597) — 300 rows per 10-min hold on a hash-chained log
  person: run header "1 waiting" (held glyph) + the global banner on every screen → "Sign in to AWS" → the
      existing device-code login run → capture PUT → storeAWSSSOBlob → NEW: every PENDING credential_reauth
      for this owner whose requested_at < login_run.created_at (I6) → APPROVED via ResolveReauth
      (audit credential.reauth.resolved, resolved_by = the person; never approval.decide)
  approval reads APPROVED → ONE re-call of resolveInjection → 200 with the NEW token → header injected →
      request forwards → the model call completes (DENIED/CANCELLED/EXPIRED → give up → the 401 body below)
  budget expires → the SDK gets 401 {"__type":"UnauthorizedException","message":<reauthTimedOutSentence>}
      + a decision row (rule_source credential:reauth-timeout); the approval row stays PENDING (the sign-in is
      still wanted) — the 24 h sweeper or the run's terminal cascade closes it
```

**Every state and its audit row** (TWO new actions — `credential.reauth.requested` and
`credential.reauth.resolved`; expire/cancel/timeout reuse rows that already exist):

| State | Where | Audit |
|---|---|---|
| resolve OK (incl. after a mid-run refresh) | `resolveAWSSSOInjection` | existing `secret.read` success (`purpose: proxy-injection-sso`) + existing `harness.credential.refresh` when the refresh fired |
| credential dead/unrenewable → hold opens | control plane, at the raise | **NEW `credential.reauth.requested`** (`owner`, `credential_source`, `provider`, `approval_id`, `reason` ∈ {spent, unavailable, not_found, pin_contradicted}) |
| hold dedup (second call, same run) | `RequestApproval` returns the PENDING row | none (dedup is silent by design) |
| person signs in → hold resolves | the capture handler decides the approval | **NEW `credential.reauth.resolved`** (`resolved_by` = the person, `capture_run_id`, `approval_id`, `owner`) + existing `harness.credential.captured` |
| hold budget expires | proxy | existing decision row `rule_source: credential:reauth-timeout`, `decision: deny` |
| approval ages out / run killed while held | 24 h sweeper / `cancelRunApprovals` | existing `approval.expire` / `approval.cancelled` |

**Failure modes.** Hold resolves in time → the agent sees one slow tool call; the banner clears. Hold expires →
`UnauthorizedException` from the portal; Claude Code reports a credential error and the turn fails (the
same class as today); the banner + PENDING row survive; signing in fixes the NEXT run. Control plane
unreachable during the hold → each poll errors; the budget still bounds; fail closed. Startup resolve returns
423 → **fail closed at boot exactly as today** (`buildInjector` runs under the proxy's 30 s `startupCtx`,
`cmd/wardyn-proxy/main.go:116`, seconds after dispatch refreshed synchronously — a boot-time death is a race
measured in seconds; holding there would fight the 3-min canary); the hold exists only on RE-resolve. SDK gives up before the
budget → the turn fails early; **the knob is for this** — lower it below the measured tolerance. **What a
"pause" looks like:** Claude Code (aws-sdk-js-v3) awaits the credential provider inside the signing
middleware — the normal spinner, no message; the bounding timeout is the SSO client's own request timeout
(Node default: none), NOT `API_TIMEOUT_MS` — **the one number not derivable from the tree: measure it in the
docker-gated test and set the default from the measurement.** botocore: `read_timeout` 60 s × 3 retries;
each retry re-enters the proxy and re-joins the same `reMu` hold.

**This lane is a SECURITY-SENSITIVE runtime change, not "lane #4 of eight" (third-party review, adopted).**
It changes how a captured credential reaches `portal.sso` (a new MITM host, a new injection path, a new
approval kind, a migration, a new internal resolve path, a sidecar hold state). It ships behind ONE switch,
with named invariants, an explicit state machine, adversarial tests and a rollback, and it cannot merge on
"tests green + Fable CLEAN" alone — it needs the dedicated F4 security round (below) CLEAN.

**Phase 0 (before any code; ~half a session, Opus):** write the THREAT-MODEL delta first — *before:* the SSO
access token is resident in the sandbox; *after:* the sandbox holds a placeholder, the proxy injects the token
on `portal.sso` only, the SigV4 role credentials stay resident, a parked model call is bounded by one knob —
and the invariants below, as the contract the code is then written against.

**Invariants (named in code comments and pinned by tests):**
- **I1 Run binding** — a grant resolves credentials only for the run that owns it (the run-token claims name
  the run; `minted.GrantID` must belong to `claims.RunID`).
- **I2 Principal binding** — the re-auth request is bound to the run's owner at DISPATCH; only a capture by
  that principal (per_user) or by an operator (shared lane) can resolve it.
- **I3 Credential-scope snapshot — the fix for the substitution hole found in review.** `awsSSOScopeFor`
  (`modelaccess.go:111-117`) derives the owner from the ROSTER ROW at call time: if an admin flips the row
  `per_user → shared` (or disables it) while a run is held, a resolve-time read hands that run the OPERATOR's
  blob. Therefore the grant's scope carries an IMMUTABLE snapshot taken at dispatch — `{owner_subject,
  credential_source, mechanism, sso_account_id, sso_role_name, region, host}` — and `resolveAWSSSOInjection`
  re-derives the scope from the roster AND requires equality with the snapshot; any drift → **403 fail closed**
  with `credentialReauthScopeChangedRefusal` (audited `secret.read` failure, `reason: scope_changed`), never a
  different credential. The equality check reads `claims.Sub` (`internal/identity/identity.go:33`, stamped from
  `runIdentitySubject(ctx, createdBy)` at `runs_policy.go:457-462` — the run's owner, identical to
  `created_by` except under the dev-only `X-Wardyn-Principal` override) against the snapshot owner on the
  per_user lane (a policy-authored grant cannot name another owner and pass). Recovery may refresh credential MATERIAL; it must never re-authorize
  the operation against a different principal, source, mechanism, account, role, region or host.
- **I4 Host binding** — the token is injected only to `ssoPortalHost(snapshot.region, override)`, exact host
  AND port (`hostEqual`, plus `require_tls` in production).
- **I5 No substitution** — `enforceConfiguredLLMMechanism`'s promise carries over: a dead lane is held or
  refused, never served by another provider or another owner's blob.
- **I6 Stale-capture protection (generation)** — a capture resolves a re-auth request only if its login run was
  CREATED AFTER the request was raised (`login_run.created_at > reauth.requested_at`); an older sign-in cannot
  resolve a newer wait. Combined with the 0.7.5 supersede (`run_killed` refusal inside the owner lock), the
  newest sandbox wins; the per-person advisory lock stays 0.7.7 (O-1).
- **I7 Audit chain** — no credential-bearing retry proceeds without an auditable predecessor: `secret.read`
  (resolve) → `credential.reauth.requested` → `harness.credential.captured` → `credential.reauth.resolved` →
  `secret.read` (the retry). Expiry/cancel/timeout each leave a row.
- **I8 Secret non-observability** — the token never appears in proxy logs, audit detail, HTTP error bodies,
  the sandbox cache file, test artefacts or screenshots (a dedicated test greps every sink).

**State machine of one re-auth request (row `kind: credential_reauth`, PENDING at raise):**
| from PENDING | event | to | audit | proxy sees |
|---|---|---|---|---|
| — | capture lands (I2 + I6 pass) | APPROVED | `credential.reauth.resolved` (`resolved_by`, `capture_run_id`) | next poll → 200 + token → request forwards |
| — | person cancels the sign-in pane | (unchanged) | `run.kill` of the login run only | keeps holding until the budget ends (the person may sign in again) |
| — | the held RUN is killed / ends | CANCELLED | `approval.cancelled` (existing) | next poll → **403 terminal** → 401 to the SDK |
| — | 24 h sweeper | EXPIRED | `approval.expire` (existing) | next poll → 403 terminal |
| — | roster scope drifts (I3) | (unchanged) | `secret.read` failure `scope_changed` | 403 terminal, fail closed |
| — | hold budget ends | (unchanged: the sign-in is still wanted) | decision row `credential:reauth-timeout` | 401 `UnauthorizedException` to the SDK; the banner + row survive |
| — | control plane unreachable | (unchanged) | — | each poll errors; the budget bounds; fail closed at its end |
The control-plane resolve answers **423 only while the row is PENDING**; any terminal row state answers 403, so
a re-resolve never holds on a dead request. Residual, accepted and bounded by the budget: the APPROVAL poll
itself answers `decided=false` on a 404 or a transport error (`proxy/approvals.go:703-712`), so an unreadable
row holds until the budget ends — exactly `ResolveWait`'s own behaviour.

**Concurrency contract:** N concurrent GetRoleCredentials for one run share ONE re-auth (the per-host
`reMu`, `inject.go:72-77`, single-flights the re-resolve; `RequestApproval` dedups on run+kind+scope) → N
waiters → ONE human action → ONE refresh → all waiters proceed on the new token. `maxReauthHolds` counts
RE-AUTH WORKFLOWS per run (sequential lifecycles), not requests; waiters queue on `reMu` and are bounded by
the SDK's own concurrency. Tests: 1 request → 1 row; 64 concurrent resolvers → 1 row, 1 hold, all 64 wake;
a request arriving during the hold joins it; one arriving right after resolution reads the fresh entry; a
duplicate capture is a no-op; the ninth WORKFLOW on a run is refused with the timeout sentence.

**Rollback / kill switch:** `WARDYN_AWS_SSO_PROXY_INJECT` (daemon env; `on` | `off`; ENV.md row; envdoc
guard). `off` = 0.7.5 byte for byte FOR NEW DISPATCHES (the placeholder cache, the grant and the MITM host are all
authored at dispatch, so a run already dispatched keeps its authored lane until it ends; resident token, no
`portal.sso` MITM, no hold, no re-auth rows for everything launched after the flip) — the rollback for an SDK
or corporate-MITM surprise without a downgrade. Default is **O-10** (recommendation:
`on` only once the docker-gated measurement AND the adversarial suite are green on the release tip; else
ship `off` with the flag documented as the way in). Migration `0064` is additive (widens a CHECK); a
downgrade to 0.7.5 with `credential_reauth` rows present is UNSUPPORTED — the upgrade runbook's pg_dump is the
rollback, said in OPERATIONS.

**Dedicated F4 security round (Fable, blind, on the lane's diff; then again in W6 S):** each attack below is
a scenario the reviewer must attempt on the code, not a checklist item — S1 run A receives run B's credential
· S2 member A's re-auth resolves from member B's (or the admin's) blob · S3 the injected token reaches a
different host or port (endpoint override, port confusion) · S4 an older sign-in satisfies a newer request ·
S5 concurrent calls create two workflows or wake with stale material · S6 a grant is reused after its
lifecycle · S7 a cancelled/expired hold later resumes · S8 timeout and capture race into an allow · S9 a
daemon restart loses state into an unintended allow · S10 a request resumes after the run was killed · S11 a
member triggers or satisfies an admin's credential flow · S12 a resume occurs without its audit predecessor ·
S13 the token appears in any sink. Plus a deterministic state-machine test that replays sequences (live,
expired, capture, timeout, capture, restart, kill, 423, 423, 200, kill, capture …) against the invariants.

**Metrics** (the existing hand-rolled `/metrics`, `internal/api/metrics.go`; same Fprintf shape):
`wardyn_credential_reauth_total{outcome=requested|resolved|expired|timeout|cancelled}` and
`wardyn_credential_reauth_wait_seconds` (a summary of resolve latency). Success criterion: a mid-run expired
SSO credential does not terminate the run when the SDK tolerates the hold (measured), and the SAME run
resumes with no new run, no new grant, no policy relaxation, no workspace remount (asserted in K).

**Non-goals — 0.7.6 must NOT change:** shared-credential semantics beyond the hold; `awsMount` / `bedrock_env` /
static keys / `bedrock_bearer` behaviour; non-Bedrock providers; scan and exec runs; existing approval
DECISION semantics (`egress_domain`, `credential`, `tool_call`); model substitution (never); sandbox policy;
`credential_residency` classification (only the SSO token's own row prose); existing audit action meanings;
CLI behaviour beyond documented messages; run IDs on resume.

**Scope (state it in the code).** In: `ssoInject` only — per_user AND the shared/admin `owner: ""` lane
(both read through `awsSSOScope`). Out, explicitly: `awsMount` (the operator's host `~/.aws`, SDK-refreshed),
`bedrock_env`/static keys, `bedrock_bearer` (nothing Wardyn can hold on). **Create still refuses** (422) — a
hold is for a run already doing work; F3 gives create's refusal a door. **Supersede race (0.7.5 known gap):**
the reauth door makes concurrent sign-ins likelier; the `run_killed` arm inside the owner lock already makes a
late capture lose — note, do not solve (per-person advisory lock stays open, O-1).

**Fix steps.**
1. `internal/types/types.go` — `ApprovalCredentialReauth ApprovalKind = "credential_reauth"`; update
   `TestApprovalKindValues` (`types_test.go:166`); ADD a `types.ApprovalKinds` slice (there is none today —
   `types.go:366-370`) + `TestApprovalKindsCoversEveryConstant`, because `closedEnumChecks()`
   (`internal/db/migrations_check_test.go:392-440`) covers `approvals.state` and `approvals.decision_scope` but
   NOT `approvals.kind` — a Go constant the DB CHECK rejects would leave every gate green (round-2 general S6).
2. NEW `internal/db/migrations/0064_approval_credential_reauth.sql` — drop-then-add the `approvals_kind_check`
   on the 0062 template admitting the four kinds; header notes the 0022 partial unique index (`kind <>
   'credential'`) now also covers this kind (one PENDING reauth per run — wanted). Name the stem in the
   CHANGELOG bullet (migration doc guard).
3. `internal/api/runs_bedrock.go` (856 lines; small edits): `ssoPortalHost(region, override string) string`
   beside `ssoEgressHosts` (:300) — returns the BARE host (`portal.sso.<r>.amazonaws.com`, or the override's
   `Hostname()`): **the injection grant's `scope.host` must stay bare** — `buildInjector` keys `byHost` on the
   rule host verbatim (`inject.go:99-116`) and both lanes resolve with the bare host (`plain_lane.go:183` →
   `apply(req, host, port)` `inject.go:424-434`; `mitm.go:455`), as `runs_dispatch_llm.go:527-528` already
   states for the bearer lane. The PORT goes where the transport reads it — and TODAY it goes nowhere: `ssoEgressHosts`
   (`runs_bedrock.go:300-303`) returns ONE bare entry (`gatewayHost` = `u.Hostname()`, `llm_gateway.go:176-182`),
   so `Policy.AuthoredPortFor(host, 8090)` (`internal/egress/proxy/policy.go:476-494`) answers false and
   `injectableTransport` (`inject.go:387`) withholds the credential on cleartext :8090 (round-2 general B1).
   **Explicit edit:** `ssoEgressHosts`' override arm appends `net.JoinHostPort(host, port)` BESIDE the bare
   host whenever the override URL carries a port; then `AuthoredPortFor` answers true and the cleartext fake
   lane is credentialed (`exact_host_binding.go:46-49`: a port-qualified entry still satisfies the bare
   host). Pin in `runs_bedrock_test.go`: with override `http://wardyn-awsssofake:8090` the egress list holds
   BOTH `wardyn-awsssofake` and `wardyn-awsssofake:8090`, and the grant host is `wardyn-awsssofake`;
   `awsSSOCacheFileContents(b, proxyInjected bool)` — when true, `accessToken: awsSSOPlaceholderToken`
   (`"wardyn-proxy-injected"`, the existing sentinel spelling :545, NEVER mask-registered) and `expiresAt: now +
   awsSSOPlaceholderCacheTTL` (30 days: aws-sdk-js-v3 validates expiry locally and attempts a refresh inside
   5 min of it; botocore raises only once expired; the file must outlive any run); the ssoInject arm also sets
   `ssoRegion: blob.Region` on `bedrockAuth`.
4. NEW `internal/api/runs_dispatch_sso_inject.go` (~120 lines) — `authorBedrockSSOInjection`, a copy of
   `authorBedrockBearerInjection`'s shape: grant scope `{"host": ssoPortalHost(...), "header":
   "x-amz-sso_bearer_token", "format": "%s", "secret_name": types.AWSSSOAccessTokenSecret}` (a NEW constant in `internal/types`, kept out of `sinkReservedSecret`); `RequireTLS:
   s.cfg.AWSSSOEndpointOverride == ""` (production TLS-only; the plain-http fake is the exception — and the
   reason a silently non-injecting proxy is a REFUSAL with Wardyn's sentence, not a bare AWS 401); returns
   `mitmHosts = []string{net.JoinHostPort(portalHost, "443")}` (port-qualified); the grant carries `TTLSeconds: 3600` like the bearer precedent (`runs_dispatch_llm.go:541`); `run.llm.bedrock`'s audit
   `detail`/`mode` for ssoInject names proxy injection (`runs_dispatch_llm.go:325`).
5. `internal/api/runs_dispatch_llm.go` (748) — `injectBedrockSSO` on `llmTransport`; set when
   `bedrock.ssoInject`; add to the MITM-CA condition (:695) and the authoring block beside :722 (~12 lines).
6. NEW `internal/api/injection_awssso.go` (~150 lines) — `resolveAWSSSOInjection(w, r, claims, minted) (handled
   bool)`, called from `handleInternalInjection` after the sentinel arm returns (`injection.go:105-188`; a new file
   because that handler is near `funlen` 150): scope = the grant's DISPATCH-TIME SNAPSHOT (I3), re-derived from the roster and required EQUAL (drift → 403 `credentialReauthScopeChangedRefusal`, never another blob); the row state gates the answer (PENDING → 423; APPROVED → 200 on the new token; CANCELLED/EXPIRED/DENIED → 403 terminal); **host pin**
   `hostEqual(minted.Injection.Host, ssoPortalHost(blob.Region, override))` else 403 + `secret.read` failure
   (the `subscriptionInjectionHost` pin, :118); `refreshAWSSSOBlob` (reuses the single-flight, skew, spent map
   and the `harness.credential.refresh` audit); live → `MaskRegistry.Add(claims.RunID, token)` — a PER-RUN registration, ADDITIVE to the global one: `refreshAWSSSOBlob` already does `AddGlobal(next.AccessToken)` + `AddGlobal(next.RefreshToken)` on every successful rotation (`awssso_refresh.go:427-428`) and this resolve calls it — **do NOT remove that `AddGlobal`** (removing it would unmask the token from every other run's streams); the per-run set is evicted for terminal runs past `RunSecretGrace` (`SweepRunSecrets` → `Evict`, `runs_lifecycle.go:373-399`), the global set has no expiry (`secretmask.go:120`) — the existing ceiling, noted, TTL-aware masking is a follow-up (round-2 general S5) (**required**: a token
   captured mid-run is only per-run-masked by the login run's handler, `ssotoken.go:243`), `secret.read`
   success, `200 {Header, Value, ExpiresAt: blob.ExpiresAt.UnixMilli(), JTI}`; dead → `raiseCredentialReauth` +
   `423`. `raiseCredentialReauth`: `RequestApproval{RunID, Kind: credential_reauth, RequestedScope:
   {"mechanism":"bedrock_sso","credential_source":<label>,"owner":<subject>}}` — **`reason` is NOT in the
   scope** (it is in the dedup hash; a spent→unavailable flip would raise a second row); audit
   `credential.reauth.requested` with the reason. **Server-side decisions on this kind are refused:** `decide()`
   (`internal/api/approvals.go:249`) gains a kind rule answering **409** "resolved by signing in, not by a
   decision" for `credential_reauth` (a security operator could otherwise Deny it, and the next poll would
   raise a fresh PENDING row — `findPendingDup` matches PENDING only); and the server-side raise honours a
   per-run cap (the internal route's `maxApprovalsPerRun`, `internal.go:428`, does not guard this path — mirror
   `maxReauthHolds` = 8 workflows per run here, refusing the ninth raise with 403 and the timeout sentence).
7. `internal/api/ssotoken.go` (392) — after `storeAWSSSOBlob` succeeds (:216): resolve every PENDING
   `credential_reauth` whose scope `owner` equals this capture's owner (query = the `findPendingDup` pattern, `approval.go:104-116`: `ListApprovals(ctx, PENDING)` filtered on kind + `requested_scope.owner`) to APPROVED through a NEW service method `ResolveReauth` that emits **`credential.reauth.resolved`** (`resolved_by` = the capturing subject, `capture_run_id`, `approval_id`, `owner`) — NOT `approval.decide` (a decision nobody clicked; third-party review, O-6 resolved) — and only for rows whose `requested_at` precedes the login run's `created_at` (I6)
   (`DecidedBy: claims.Sub`, `Reason: "signed in again"`) — best-effort + logged, never failing the capture;
   `resolvePendingReauth` lives in `injection_awssso.go` beside the raise if `funlen` bites.
8. `internal/egress/proxy/inject.go` — `resolveInjection` returns `errReauthPending` on `http.StatusLocked`
   before the generic status-error branch (:474); `injector.resolve`'s re-resolve (:157) calls the holding
   wrapper — and derives the hold's budget ctx from the MITM REQUEST's ctx rather than the
   `context.Background()` the re-resolve uses today (:157), so a disconnected SDK releases `reMu` instead of
   pinning it for the whole budget; **`buildInjector` (:90-124) keeps calling `resolveInjection` unchanged —
   no hold at boot** (round-2 general B3; ruling S3).
9. NEW `internal/egress/proxy/credhold.go` (~120 lines) — `resolveInjectionHolding(ctx, base, token, grantID,
   client)`: call `resolveInjection`; unless the error is `errReauthPending` (carrying `approval_id`) return it
   unchanged (**every other grant is byte-identical to today**); otherwise one budget ctx from
   `credentialReauthBudget()` and poll the APPROVAL with the existing `approvalClient.poll(ctx, id)`
   (`proxy/approvals.go:694-696`) every `holdPollInterval` (1 s, the existing const); on APPROVED re-call
   `resolveInjection` exactly once (200 → proceed; anything else → `errReauthTimedOut`'s body); on
   DENIED/CANCELLED/EXPIRED give up at once (same 401 body); on budget expiry `errReauthTimedOut`. This is
   literally the second caller of the `ResolveWait` shape the field report asked for. Per-run
   bound: `reauthHolds atomic.Int32` on the injector, refuse past `maxReauthHolds = 8`; one hold per host at a
   time is already `reMu`. Knob read with `os.Getenv` here (the `git_broker.go:129` precedent), **clamped**.
10. `inject.go` + `mitm.go:455-460` — on `errors.Is(ierr, errReauthTimedOut)`: emit the decision
    (`rule_source: credential:reauth-timeout`) and write **401** + `{"__type":"UnauthorizedException",
    "message":…}` instead of the 502 (`UnauthorizedException` is a modeled, non-retryable GetRoleCredentials
    error, so both SDKs map it to a credential failure rather than retrying a 502) — `writeSSOUnauthorized` in
    `credhold.go`.
11. UI (edits only): `wardyn/copy.ts:557` `WIRE_TO_COPY += credential_reauth: "reauth"` (a NEW `ApprovalKind "reauth"` with `APPROVAL_KIND_LABEL.reauth = "AWS sign-in"` — UX round B3: mapping it to "credential" made `/approvals` title the row "Mint a scoped credential" with a blast-radius banner); NEW `REAUTH_ROW`
    block; `canDecideApproval` → false for this kind for EVERY tier; `live-approvals.tsx` `isHeld` → true for
    the kind (:102), `rowLabel` (:113) → the reauth sentence, the filter (:234) admits it; the row renders **one
    button "Sign in to AWS" that opens the SAME door dialog `ui-model-access-door` ships** (no navigation, no
    Approve/Deny); the header chip needs no change (it counts PENDING rows and `isHeld` feeds the held glyph);
    `ApprovalKindChip` picks the label up from `WIRE_TO_COPY`; `WireApprovalKind` gains the member (hand
    mirror). For the shared lane a member's run can raise a reauth only an admin can satisfy — the row's
    sentence for a non-owner reuses `shared_expired`'s "ask your admin" copy (no dead door).
    **UX round rulings on the cockpit and the Approvals page (B2, B3, S5, S6, S8, nits):** (a) the three
    existing sentences that fire around a pending row are FALSE for this kind and are excluded from it:
    `viewerBlocked` (`run-detail.tsx:550-551` → `VIEWER_APPROVAL_BLOCKS_NOTE` "blocked until an admin
    decides…", `copy.ts:138`), the strip heading "Sandbox is waiting — approve to let it through"
    (`live-approvals.tsx:315-319`) and `SECURITY_ONLY_REASON` (:335) — predicates become
    `pending.some(a => a.kind !== "credential_reauth" && !canDecideApproval(...))`, and when every pending row
    is a reauth the heading arm is `REAUTH_HEADING = "Your AWS sign-in lapsed — this run is paused until you
    sign in again"`; the Approve/Deny pair is REMOVED for the kind, not disabled; (b) `/approvals`
    (`approvals.tsx` `deriveTitle`) gains the arm "Sign in to AWS again — this run is paused", no
    blast-radius banner, the door button in place of the decision pair; the sidebar Approvals badge keeps
    counting it (it is a request); (c) the board card and the cockpit header say `waitingReauth: (n: number) => n > 1 ?
    \`Waiting for your AWS sign-in · ${n - 1} more waiting\` : "Waiting for your AWS sign-in"` (round-2 UX S8: a
    count-free string would hide a co-pending egress approval — the person signs in and the run still sits) instead of `RUN_COCKPIT.waitingHeld` "1 waiting · sandbox held" (`copy.ts:453`,
    `run-card.tsx:209`, `run-detail-summary-header.tsx:327-333`) — carry `reauth: true` in `approvalSignals`
    (`board-groups.ts`); (d) the row's button carries `aria-label="Sign in to AWS — to resume this run"`; (e)
    when `LiveApprovals` sees a reauth row leave PENDING as APPROVED, `toast.success("Signed in — the run is
    continuing")` — the person otherwise gets no "it worked" moment as the row vanishes on the next 4 s poll;
    (f) the row (while held) is the cockpit's door owner: it calls `door.claim()` so the strip drops its
    button on this page.

**Knob.** `WARDYN_CREDENTIAL_REAUTH_TIMEOUT` — duration, default `600s` (= `maxHoldTimeout`'s ceiling and the
order of the device-code window; a hold that outlives the sign-in it waits for waits for nothing), clamp
`[10s, 1800s]`, unparseable/non-positive → default; read in the sidecar (`credhold.go`). ENV.md row (proxy
sidecar table beside `WARDYN_GIT_APPROVAL_TIMEOUT`, `docs/ENV.md:226`): "how long the proxy HOLDS a sandbox's
AWS SSO `GetRoleCredentials` call while the credential's owner signs in again, instead of failing the run's
model call (`credhold.go`). Applies only to the captured-AWS-SSO Bedrock lane, whose token the proxy injects;
every other credential is unaffected. Clamped to `[10s, 1800s]`; unparseable or non-positive values keep the
default. Lower it if your agent's SDK gives up before the hold does — on expiry the call fails exactly as it
did before this existed." **Do not set the default before the docker-gated measurement runs.**

**DRAFT strings** (all `// DRAFT (M2 canon pending)`, asserted through constants):
```go
// internal/api/injection_awssso.go
const credentialReauthRaisedSentence = "this run's model access is configured as Amazon Bedrock " +
    "(captured AWS SSO session), and that session can no longer be renewed — the run is HELD while " +
    "you sign in again. It resumes by itself when the sign-in lands. Wardyn does not substitute a " +
    "different model provider."
const credentialReauthHostPinRefusal = "the AWS SSO access token may only be injected to this " +
    "credential's own sso portal host"
// internal/egress/proxy/credhold.go — the 401 body on expiry
const reauthTimedOutSentence = "wardyn held this AWS SSO credential request while its owner was " +
    "asked to sign in again, and nobody signed in before the hold expired; nothing was substituted"
```
```ts
// ui/.../wardyn/model-access-copy.ts  (copy.ts is at 961/1000; only WIRE_TO_COPY / APPROVAL_KIND_LABEL touch it)
export const REAUTH_ROW = {
  label: "AWS sign-in needed — this run is paused",   // "paused" everywhere a person reads; "held" is the wire/audit word (round-2 UX S6)
  hint: "The run is paused until you sign in again — not failed. Sign in and it continues where it was.",
  action: "Sign in to AWS",   // = AGENTS.SIGN_IN_AWS, reused
} as const;
```

**Tests, red first.** Go control plane — NEW `injection_awssso_test.go`: live blob → 200 with
`x-amz-sso_bearer_token`, `%s` format, `expires_at == blob.ExpiresAt` · wrong host on the grant → 403 +
`secret.read` failure · a grant naming the sentinel on a run whose roster scope is `shared` resolves the
operator blob, never another member's · dead blob → **423**, exactly one PENDING `credential_reauth` for the
run, `credential.reauth.requested` with `owner` · a second resolve while pending → still one row, still 423 ·
capture PUT → the row is APPROVED and the next resolve answers 200 with the NEW token, which is in the
`MaskRegistry`'s per-run set for `claims.RunID` (and in the global set via the refresh path's existing
`AddGlobal`). `runs_bedrock_test.go`: `awsSSOCacheFileContents(blob, true)` holds the placeholder,
an `expiresAt` > 7 days out and NO real token; `(blob, false)` byte-identical (golden); `ssoPortalHost` port
pin. NEW `runs_dispatch_sso_inject_test.go`: an ssoInject dispatch authors exactly one api_key grant on the
portal host, a `host:443` MITM entry and the CA; awsMount/bearer/static lanes author none.
`closedEnumChecks()` gains `{"approvals", "kind", enumSet(types.ApprovalKinds)}` (the parity test does not exist for `kind` today) + a store round-trip of the kind. Go proxy — NEW
`credhold_test.go`: a fake control plane answering 423 three times then 200 → the header, elapsed ≥ 2 polls ·
423 forever → `errReauthTimedOut` within the budget (+10 %) · a NON-423 error returns immediately (no hold —
the regression that keeps every other grant untouched) · `maxReauthHolds` refuses the ninth. `mitm_test.go`:
a reauth-timeout resolve writes 401 with an `UnauthorizedException` body and emits `credential:reauth-timeout`;
the body is masked. `inject_test.go`: `buildInjector` FAILS CLOSED on a first-resolve 423 exactly as on any other error (no hold at boot).
**awsssofake** (`test/awsssofake/server.go`: `bearerHeader` :41, `checkBearer` :532; CreateToken hard-codes
`expiresIn: 3600` (:378,:406) and role credentials expire in 1 h (:150); flags are only `-addr`/`-accounts`,
`cmd/main.go:34-41`): THREE additions — (a) `AWSSSOFAKE_TOKEN_TTL` and (b) `AWSSSOFAKE_ROLE_CRED_TTL` env knobs — (b) must stamp the expiry PER CALL inside `handleGetRoleCredentials`
(`server.go:446-468`): `New()` fixes one absolute `Expiration` at construction (:146-151) and echoes it on
every call, so a constructor-only TTL would make every later answer already-expired (a refresh loop, not a
T+3/6/9 cadence); and the docker-gated measurement reports the SDK's OBSERVED re-call cadence before the walk
budgets the 12-minute case — whether a 3-minute role-credential TTL survives aws-sdk-js-v3's own expiry
window is unverified in-tree (round-2 general S9; O-9)
(the hold is reachable only when the injector RE-resolves, i.e. inside `injectRefreshMargin` 5 min of
`blob.ExpiresAt` and after dispatch's 10-min skew — with 1 h TTLs a live case would take ~55 min; with token
12 min / role creds 3 min the SDK re-calls `portal.sso` at ≈T+3/6/9 and the T+9 call re-resolves → the hold
fires at ≈T+9), and (c) a control
(`-reauth-after <n>` or an HTTP control endpoint) that makes `CreateToken` answer `invalid_grant` on demand;
`GetRoleCredentials` already 401s a wrong bearer (the assertion that the proxy substituted the token).
**Docker-gated** (`WARDYN_TEST_DOCKER`, the SSO pair): a real claude-code image run against the fake flipped to
`invalid_grant` mid-run — (a) the SDK call is PARKED, not failed, for ≥ 180 s (**the measurement that sets the
default**), (b) after a capture lands the same run's GetRoleCredentials succeeds and `roleCredsSeen` records
the pinned account/role, (c) the sandbox cache file never contains the real token. **Live** (handed to
`e2e-sso-path`): **case K** "a member's session dies mid-run and the run is HELD, not killed" (timeline: the walk runs the fake with token 12 min / role creds 3 min; capture at T; run launched at T+1; the fake flipped to `invalid_grant` at T+4; the SDK's T+9 role-credential refresh re-resolves → hold; ~12-minute case, budgeted as such in the walk) — flip the fake,
drive a run to a model call, the run header shows `1 waiting` with the held glyph, the row reads
`REAUTH_ROW.label`, the run is still RUNNING, `credential.reauth.requested` is in the audit list with the
member's owner; **case K(resume)** — click `REAUTH_ROW.action`, complete the fake device flow, the approval row
goes APPROVED, the waiting chip clears, the run completes — SAME run id. `scripts/kind-sso-walk.sh` sets the
fake's reauth control and nothing else. `go test ./cmd/wardynd/...` + `make cover-check`.

**Docs (canon).** THREAT-MODEL: the captured-AWS-SSO row's "Why it can't be proxy-injected" cell → SHIPPED (the
sandbox holds a placeholder; the SSO access token lives only in proxy memory); the *Derived AWS role
credentials* row unchanged and named as the reason residency stays `sandbox` (so nobody "fixes"
`credential_residency.go`); the hold added to residuals (a parked model call bounded by one knob).
`docs/AUDIT-ACTIONS.md`: TWO rows — `credential.reauth.requested` (fields `approval_id, credential_source,
owner, provider, reason`; citation `internal/api/injection_awssso.go:<line>`; tier internal) and
`credential.reauth.resolved` (fields `approval_id, owner, resolved_by, capture_run_id`; citation the
`ResolveReauth` emit) — the forward guard reds on any emitted action without a row
(`audit_actions_forward_guard_test.go:412`; round-2 general S3); PLUS the window-0 re-cites this lane's
insertions shift: `AUDIT-ACTIONS.md:181` → `internal/api/injection.go:118` (the resolve call lands above it) and
`:192` → `internal/api/ssotoken.go:246` (the reauth resolution lands after `storeAWSSSOBlob`) — re-point both
in the same commit (S4). Also update `isMITMHost`'s trust-boundary comment (`mitm.go:175-200`), which
enumerates two permitted MITM sources: this dispatch-authored `portal.sso` entry is the third, and W6's S
lens reads that comment as the boundary statement (S10). OPERATIONS.md: "a run
is holding for a sign-in" — what the operator sees, the knob, the PENDING row outliving the hold is deliberate.
ENV.md: the knob row. CHANGELOG: "A captured AWS SSO session that lapses mid-run now HOLDS the agent's next
model call while its owner signs in again, instead of failing the run. The SSO access token is no longer
written into the sandbox on that lane (Phase B); the short-lived role credentials the SDK mints from it still
are. Migration `0064_approval_credential_reauth` adds the approval kind." `docs/adoption/corp-network-onboarding-findings.md`:
a Finding-4 resolution note.

**Risks / open.** (a) The SDK tolerance is the only unmeasured number — if Claude Code's SSO client carries a
short request timeout the hold degrades to "one retry's worth of pause"; the copy must not promise minutes
until measured. (b) MITM'ing `portal.sso` widens the MITM set by one exact host+port with paired injection;
the proxy sees only GetRoleCredentials/ListAccounts, never the model call. The timeout's 401 body lands on the
TLS lane only: the plain (fake) lane swallows injection errors (`inject.go:430-434`) and the SDK sees the
fake's own 401 — acceptable on the walk, said in the lane. (b2) the CLI's `runFailureReason` prints `error`
before `reason` (`cmd/wardyn/commands.go:507`), so the F3 key is harmless there. (c) RESOLVED: the capture emits
`credential.reauth.resolved`, never `approval.decide` (O-6). (d) Shared lane: a member's run
can raise a reauth only an admin can satisfy — the row must say "ask your admin". (e) The supersede race is
noted, not solved.

**Size.** ~900 lines Go (≈350 logic), ~120 TS, 1 migration, 1 fake control, 2 live cases. Opus, 2–3 sessions;
the docker-gated measurement gates the knob default and runs first.

### Lane `sso-refresh-visibility` (W1, **Sonnet** worker + Fable review) — Finding 5

**Design — three changes, none touching the spent bookkeeping.** (i) **No new retry**; make the existing one
LEGIBLE: `createAWSSSOTokenWithRetry` returns `attempts int`, and the failure audit carries `"attempts": 2`
plus `errors.Join`ed text so BOTH errors are in the row (two EOFs = a network story; EOF then `invalid_grant`
already says `spent: true`, now with `attempts: 2` beside it). (ii) **Grading consults the spent set and
grades `expiring`, not `expired_signin`, while the access token still works:** in `awsSSOCredentialState` a
spent blob skips the `renewable` arm — `spent && expired → dead (expired_signin | shared_expired)` as today;
`spent && !expired → expiring, deadline = blob.ExpiresAt − awsSSORefreshSkew` NEW (dispatch refuses a spent token from `ExpiresAt − 10 min`, `awssso_refresh.go:381-386` — the deadline names the moment runs actually start failing, not the token's nominal expiry); `!spent → unchanged`. `expiring` is already in
`MODEL_ACCESS_ACTIONABLE`, so "Sign in to AWS" appears while the person can still work — exactly F5's ask —
and it is more honest than `expired_signin` on a credential still signing requests; the deadline comes from
`modelAccessDeadline`'s default arm for free. (ii-b) **Keep the facts distinct in-process (third-party review):** a spent refresh token and an expiring
access token are different facts that happen to want the same door. `SetupModelAccess` gains an in-process
`Cause` (json:"-", values `renewal_spent` | `registration_lapsing` | `token_expiring` | "") so grading and the
admin checklist row can say WHICH ("your AWS session was retired by AWS — sign in again" on the operator's
checklist row vs "Sign in again before <ts>"); the six WIRE states are unchanged (the operator calls the
vocabulary right). Metrics on the existing `/metrics`:
`wardyn_sso_refresh_total{outcome=success|transport_error|spent|unavailable}`.
(iii) Thread `spent` as a parameter, keeping both functions pure:
`awsSSOCredentialState(blob, found, perUser, spent, now)`, `setupModelAccess(sc, blob, found, spent, scope,
oidcConfigured, now)`; the one caller (`setup.go:948`) computes `s.awsSSOTokenSpent(awsSSOTokenFingerprint(
blob.RefreshToken))` (guard `RefreshToken != ""`). **Deliberately NOT changed:** the spent map stays in-memory
and unpersisted (a restart re-grades `live` until the next dispatch proves otherwise — extend the `ponytail:`
note at `server.go:760-769` by one sentence: grading reads it too); the dispatch refusal sentences; the
transient-failure-with-valid-token path stays silent to the run (the banner is where it belongs).

**Fix steps.** (1) `awssso_refresh.go:472-483` — `(awsSSOTokenResponse, int, error)`; on the second attempt's
failure `errors.Join(first, second)`; the spent short-circuit stays un-retried. (2) :389-399 — `"attempts"` in
the failure data map. (3) `modelaccess.go:337` — the `spent` parameter and the two arms; :408 threads it. (4)
`setup.go:948` — compute and pass (setup.go is AT its cap: this must be a net-zero-line edit or the computation
moves to a helper in `modelaccess.go`). (5) `server.go:760-769` — the doc sentence.

**Tests, red first.** `modelaccess_test.go` table: `{spent, !expired}` → `expiring` with the expiring action
and `Deadline == blob.ExpiresAt` — RED · `{spent, expired, perUser}` → `expired_signin` · `{spent, expired,
!perUser}` → `shared_expired` · `{!spent, …}` every existing row byte-identical. `awssso_refresh_test.go`:
transport error twice → failure row with `attempts: 2` and both errors · transport error then `invalid_grant`
→ `spent: true`, `attempts: 2` · first-attempt success → pin whether `attempts` is 1 or absent.
`setup_test.go`: after a dispatch marks a token spent, `/setup/status` for that principal flips `live` →
`expiring` WITHOUT the token having expired — **the F5 regression test, named for it.** UI: no change (the
states already render; the banner from `ui-model-access-door` shows it within one poll).

**Docs (canon).** `docs/AUDIT-ACTIONS.md:196`: `attempts` added to `harness.credential.refresh`'s fields, and its citation `awssso_refresh.go:469` re-pointed in the same commit (this lane edits :389-399 above it; window 0 — round-2 general S4) + one
clause ("`attempts` is 1 or 2 — a transient failure gets exactly one immediate retry, so `attempts: 2` with a
transport error on both is a network story, not a credential one"). CHANGELOG: "A captured AWS SSO session
whose refresh token AWS has already retired now grades `expiring` with a sign-in action the moment Wardyn
learns it, instead of reading `live` until the client registration lapsed. Refresh failures record how many
attempts were made." adoption note: the single-dropped-packet framing was two attempts.

**Risks.** In-memory spent marks are per-process (single instance enforced; a restart loses them —
re-derived at the next dispatch; O-8). A false `spent` (only the four AWS codes set it) grades a working
credential `expiring` — the worst case is a redundant sign-in prompt. **Size.** ~120 lines Go + ~100 test
lines; half a Sonnet session; land BEFORE `credential-reauth-hold` (both touch `awssso_refresh.go`).

### Lane `daemon-egress-proxy` (W1, **Sonnet** worker + Fable review) — Finding 8

**Decision: a dedicated env knob applied in the one place that already owns this transport; do NOT reuse
`SiteConfig.upstream_proxy_url`.** (1) Boot ordering: OIDC discovery is a boot-time call, SiteConfig a runtime
DB read — a SiteConfig proxy would cover AWS SSO and the webhook but not OIDC, the shape that produces a
nine-minute outage. (2) Trust tier: `upstream_proxy_url` is admin-editable from the console; where wardynd's
own `CreateToken` POST (client secret + refresh token) is dialed belongs at the `WARDYN_TRUSTED_CA_FILE` tier
(deployer/MDM, boot log, refused if malformed — "Control-plane-authored only: never a SiteConfig field",
`server.go:220`). (3) The two hops have different rules (the sidecar validator is http-only CONNECT chaining
because that is what `dialThroughUpstream` implements; the daemon's hop is Go's `Transport.Proxy`).
**Implementation = a few lines beside `installTrustedCA`:** set `http.DefaultTransport.Proxy` at boot from
`httpproxy.Config{HTTPProxy: v, HTTPSProxy: v, NoProxy: nop}.ProxyFunc()` — scoped by construction to the
daemon HTTP paths that dial via `DefaultTransport`; the docker client (unix socket) and client-go (its own
transport, its own env read) are untouched, which is the whole difference from `HTTPS_PROXY`. **The bypass
list self-defends:** `WARDYN_DAEMON_NO_PROXY` in `NO_PROXY` spelling, and wardynd AUTO-APPENDS
`$KUBERNETES_SERVICE_HOST` (the exact address whose mis-bypass took their control plane down) and the
`WARDYN_AWS_SSO_ENDPOINT_OVERRIDE` host (a kind Service must never go through a corp proxy). One boot log line
naming the proxy host (never userinfo) and the effective bypass list. Rejected: a `daemonTransport()` helper
cloned per call site (five packages, drifts on the sixth caller — the argument `installTrustedCA` already
settled).

**Fix steps.** (1) `cmd/wardynd/boot_flags.go` — `daemonProxyURL` (`WARDYN_DAEMON_PROXY_URL`) and `daemonNoProxy`
(`WARDYN_DAEMON_NO_PROXY`) beside the flag block (:308). (2) NEW `cmd/wardynd/daemon_proxy.go` (~90 lines) —
`installDaemonProxy(tr *http.Transport, rawURL, noProxy string) (effective string, err error)`: validate (scheme
`http`/`https`, non-empty host, **no userinfo**), append `KUBERNETES_SERVICE_HOST`, the SSO override host and the OIDC `InternalIssuerURL` host (every cluster-internal destination wardynd itself dials; the content-scan sidecar is the PROXY's client, not wardynd's) to the bypass list,
set `tr.Proxy`; **refuse boot** on a malformed value (the `ValidateAWSSSOEndpointOverride` posture). (3)
`cmd/wardynd/main.go:156-158` — call it inside the same `if tr, ok := http.DefaultTransport.(*http.Transport)`
block after `installTrustedCA`; log. (4) `awssso_refresh.go:488-496` — the comment now says the call follows
the DAEMON proxy knob, not the process `HTTP_PROXY` family, and the four siblings follow it too. (5) `go.mod` —
promote `golang.org/x/net` to a direct require (no new module).

**Knobs.** `WARDYN_DAEMON_PROXY_URL` — unset = direct (byte-identical to today); `http://`/`https://`, non-empty
host, no `user:pass@`; malformed ⇒ boot refused. `WARDYN_DAEMON_NO_PROXY` — `NO_PROXY` spelling (host,
`.suffix`, CIDR, `*`); `KUBERNETES_SERVICE_HOST` and the SSO override host appended automatically; ignored
when the URL is unset. ENV.md rows (daemon table): "`WARDYN_DAEMON_PROXY_URL` | string (URL) | (unset =
direct) | the forward proxy **wardynd's own** outbound HTTP calls traverse: OIDC discovery/JWKS, audit
webhooks, GitHub App token minting, AWS SSO `CreateToken` renewal, and Entra directory sync. The supported
replacement for setting `HTTPS_PROXY` on wardynd, which is still refused: the standard names are also read by
the Kubernetes client and by every library that calls `http.ProxyFromEnvironment`, and getting `NO_PROXY`
wrong there takes the control plane's own API access with it. Applied to `http.DefaultTransport` only — the
Kubernetes client builds its own transport and the Docker client speaks a unix socket. Deliberately not
`SiteConfig.upstream_proxy_url`, which is the sandbox's egress hop and is admin-editable at runtime. Must not
embed `user:pass@`. A malformed value refuses boot. Separate from `WARDYN_HOST_PROXY_B64` (diagnostics only)."
and "`WARDYN_DAEMON_NO_PROXY` | string (CSV) | (unset) | destinations `WARDYN_DAEMON_PROXY_URL` must not be
used for, in `NO_PROXY` spelling. Wardynd appends `KUBERNETES_SERVICE_HOST` and any
`WARDYN_AWS_SSO_ENDPOINT_OVERRIDE` host itself. Ignored when the proxy URL is unset."

**DRAFT strings** (`cmd/wardynd/daemon_proxy.go`):
```go
const daemonProxyInvalidRefusal = "refusing to start: WARDYN_DAEMON_PROXY_URL is %s — " +
    "it decides where wardynd's OWN outbound calls go, including the AWS SSO token renewal that " +
    "carries your client secret and refresh token; fix it or unset it"
const daemonProxyUserinfoRefusal = "refusing to start: WARDYN_DAEMON_PROXY_URL must not embed a " +
    "credential (user:pass@) — put the proxy's credential in the proxy, or open an issue for a secret-ref form"
```

**Semantics (third-party review, adopted):** `NO_PROXY` matching is `x/net/http/httpproxy`'s, never a
hand-rolled matcher; `url.Parse(raw).User != nil` is REFUSED at parse (a credential in the process environment
is visible to diagnostics and logs), not merely documented; the test matrix covers: no proxy → direct · proxy
→ the AWS SSO CreateToken dials it · malformed / unsupported scheme → boot refused · proxy unreachable → the
CreateToken failure names the proxy host in its error (no silent hang: the 10 s client timeout stands) ·
`NO_PROXY` host / `.suffix` / CIDR / `*` / IPv6 literal → defined by `httpproxy` and pinned · k8s API host and
the SSO override host auto-bypassed · credentials in the URL → refused · proxy + `WARDYN_TRUSTED_CA_FILE` →
both apply (the CA to the TLS to the proxy or the origin, as Go does). Metric:
`wardyn_daemon_egress_total{path=sso_refresh|oidc|webhook|github|entra, via=proxy|direct}` is OPTIONAL — only
if the existing counters' shape makes it a few lines; otherwise the boot log line is the observability.
**Tests, red first.** NEW `cmd/wardynd/daemon_proxy_test.go`: unset ⇒ `tr.Proxy` untouched (identity against a
fresh clone) · a valid URL ⇒ `Proxy(req)` returns it for an external host and nil for a bypassed host ·
`KUBERNETES_SERVICE_HOST` set ⇒ auto-bypassed · the SSO override host auto-bypassed · userinfo ⇒
`daemonProxyUserinfoRefusal` · garbage ⇒ boot refusal. `envdoc_guard_test.go` passes only with both ENV.md
rows. `awssso_refresh_test.go`: existing tests unchanged (httptest loopback is `NO_PROXY`-exempt by default —
pin it in a test, not prose).

**Docs (canon).** ENV.md: the two rows + edits to the `HTTP_PROXY` row (:343) and the `WARDYN_HOST_PROXY_B64`
explainer (:381-389) pointing at the knob. OPERATIONS.md "wardynd behind a corporate proxy": the knob, the
bypass list, the boot log line to grep, the k8s API and Docker socket are unaffected. adoption
`corp-network-onboarding-findings.md`: the Finding-8 resolution beside A2 (the SANDBOX hop) — two different
hops, said plainly. CHANGELOG: "wardynd's own outbound calls can now follow a corporate proxy through
`WARDYN_DAEMON_PROXY_URL`, without the process-wide blast radius of `HTTPS_PROXY`."

**Risks / open.** A credentialed corp proxy is unsupported by this knob (O-7; a `WARDYN_DAEMON_PROXY_SECRET`
secret-ref follow-up mirrors `UpstreamProxySecretRef`). A TLS-less hand-rolled kubeconfig reuses
`http.DefaultTransport` (`cache.go:113`) and would inherit the proxy — the `KUBERNETES_SERVICE_HOST` auto-bypass
covers in-cluster; document the rest. `https://` proxy URLs accepted here, refused for the sandbox hop —
deliberate; say why in both places. **Size.** ~150 lines Go + ~120 test lines + docs; half a Sonnet session;
fully independent — **land it first, it unblocks the operator today.**

### Shared facts for the starting-detail and login-pane lanes (F6, F7) — verified at dfa89f60

- **F6 — the answer exists and is thrown away.** `internal/runner/k8s/lifecycle.go:38-60` `statusFromPod`
  (PodPending / default → `st.State = RunStarting; st.Message = waitingDetail(pod)`); `waitingDetail` :83-97
  returns `"<container>: <Reason>[: <message>]"` (main container `agent`, `naming.go:49`); `podStuckReason`
  (`k8s/sandbox.go:393-427`) reads `PodScheduled=False`. `runner.Status{State, ExitCode, Message}`
  (`runner.go:474-479`). The ONLY production caller of `Runner.Status` is `SweepTerminalSandboxes`
  (`runs_lifecycle.go:339-341`), which discards `Message`. `handleGetRun` / `handleListRuns`
  (`runs_policy.go:181-207`, :132-172) are pure store reads + the `projectRecordingMeta` projection (a
  precedent for a handler-derived field). **During the whole STARTING window there is NO `sandbox_ref`:**
  `CreateSandbox` at `runs_dispatch.go:524` blocks; `SetSandboxRef` runs at :565 AFTER it returns — a live
  `Runner.Status` probe from `handleGetRun` is impossible, not merely costly. The seam that exists:
  `waitContainerRunning` (`k8s/sandbox.go:349-390`) and `waitPodIP` (:484-502) already `Pods().Get()` every
  `k8sPollInterval` = 200 ms (`canary.go:25`) on the caller's goroutine (no mutex needed);
  `orchestrator.go:224-250` forwards `spec` verbatim to `sub.CreateSandbox` — a new `SandboxSpec` field
  reaches the k8s driver with no orchestrator change. `terminalWaitingReasons` (`canary.go:71-78`:
  ImagePullBackOff, ErrImagePull, CreateContainerError, CreateContainerConfigError, InvalidImageName,
  CrashLoopBackOff) already fail fast in `waitContainerRunning` with "%s container stuck waiting (%s): %s" →
  `FailureHint` on FINAL failure only. **Docker can honestly say "first pull":** `ensureImage`
  (`docker/driver.go:1161-1175`) reaches `dockerutil.PullImage` only after `imagePresent` returned false; call
  sites :281 (agent image) and :299 (proxy image); `statusFromInspect` (`driver_helpers.go:19-45`) sets no
  Message for `created` — the wrong place to look. Store: `runInsertCols` / `runCols = runInsertCols + ",
  failure_hint"` (`store.go:299-304`, doc: a column written by a scoped UPDATE is appended to `runCols` alone);
  `scanRun` :313-331 the one reader; `SetRunFailureHint` :265-268 the scoped-UPDATE precedent; last migration
  `0062_approval_cancelled.sql`. **RBAC forbids events on the record** (`deploy/helm/wardyn/templates/rbac.yaml`
  header: "the kubelet's eviction verdict comes back through pods: get … never the Events API, so no 'events'
  verb belongs here"); client-go v0.36.4; no `ImagePullProgress` anywhere. **The real bounds:**
  `podIPWaitTimeout` 90 s (`canary.go:34`) bounds `waitPodIP` — the CNI assigns `PodIP` at PodSandbox
  creation BEFORE any application image is pulled, so a cold pull never trips it; it bites on SCHEDULING,
  which is what live case E's taint manufactures (`sso-member-recovery.spec.ts:551-563` says so). The agent
  image's pull sits under `canaryWaitTimeout` = 3 min (`sandbox.go:335-341`); the operator's 127 s / 131 s
  fit. **No bound changes in 0.7.6.** UI: `RunStateBadge` meta `primitives.tsx:167-180` (STARTING = info
  "Starting" pulse); header badge `run-detail-summary-header.tsx:227`, `failure_hint` chip :260-264 (365
  lines); board row badge `runs.tsx:691`, group header :580, `POLL_MS` 3000 (:61,:178); `DETAIL_POLL_MS` 4000;
  TS mirror `lib/types/runs.ts:127-132`; login-pane starting block `harness-login-pane.tsx:736-749`;
  `login-start-wait.ts` (`startWaitVerdict` starting|slow|retrying|unreadable; `RUN_POLL_SLOW_START_MS` 60 s,
  `RUN_POLL_RETRYING_AFTER_MS` 10 s, `RUN_POLL_UNREADABLE_AFTER_MS` 300 s, `RUN_POLL_MIN_FAILURES` 15; its
  header concedes "Nothing on the read path can [know a pull is happening]"). "Could not reach the control
  plane." = `TIMEOUT_MESSAGE` (`lib/api/core.ts:142`; wfetch 60 s `AbortSignal.timeout`, `LAUNCH_DEADLINE_MS`
  300 s for sandbox-creating calls) — a client fetch timeout, not a pane budget. `TestStatus_WaitingReasonSurfacedInMessage`
  (`lifecycle_test.go:84-104`).
- **F7a.** `harness-login-pane.tsx` (910 lines): the `window.open` block :592-600 inside `handleOutput`
  (:585-635), the `onOutput` PTY callback of `AttachTerminal` (:801) — never a user gesture; the comment
  concedes the block. `extractDeviceVerificationUrl` :328-340 prefers `user_code=`, rejects a still-streaming
  match, host-restricted to `device.sso.<r>.amazonaws.com` / `*.awsapps.com`; `extractAuthUrl` is the anthropic
  counterpart. Both Start-login buttons (:682-684, :710-712) call `launch()` (:425-460), whose first statements
  run synchronously inside the click's task; the first `await` is `harnessAuthApi.harnessLogin` at :444.
  Header link :783-793 (`data-testid="auth-url-link"`, `target="_blank" rel="noopener noreferrer"`). `cancel`
  :638-641. `test/awsssofake` returns a `verificationUriComplete` with `?user_code=`. **`window.open(url,
  "_blank", "noopener")` returns null by specification** — no handle to navigate later, so `noopener` cannot be
  the mechanism; `w.opener = null` on an `about:blank` handle severs the reverse link and keeps `w.location` /
  `w.close()`.
- **F7b.** Phase enum :83; helper branch of `handleOutput` :614-632 (fail marker before done marker;
  `doneMarker` → `savedRef.current = true; confirmCapture()`); `confirmCapture` :568-581 (kills the run, phase
  `saving`, `confirmCaptureWithServer`, then `done`+`onDone()` or `error`); `capture-confirm.ts`:
  `serverConfirmsCapture` (`captured && source_run_id === runId` → true; someone else's `source_run_id` →
  false; presence fallbacks `model_access` live/expiring for an old daemon), `confirmCaptureWithServer`
  (unreachable → one retry at 500 ms; up to `CAPTURE_CONFIRM_RETRIES`=3 re-reads at 500 ms; a thrown error is
  NOT retried), `CAPTURE_VERIFYING` = "Checking with Wardyn that the session was stored…" rendered :805-821 as
  `role=status` — **0.7.5 already narrates the corroboration.** The observed "only Cancel" screen is
  `phase==="attached"`, helper mode, no marker yet: :833-844 renders "Open the verification link above, enter
  the user code shown in the terminal, and approve. Wardyn captures the session automatically when the login
  completes." + Cancel — FALSE the moment the AWS CLI prints "Successfully logged into Start URL". `onDone` is
  wired to close at both mounts (`agents-tab.tsx:214`, `connection-cards.tsx:721`) — the pane closes; it never
  reaches `onDone`. Between the CLI line and the marker: the sandbox command is `aws sso login --sso-session
  wardyn --no-browser --use-device-code && wardyn-aws-sso` (:219; `deploy/images/aws-sso/agent-run`,
  `signin-pane.sh`); `wardyn-aws-sso`'s `resolveTimeout` 15 s bounds the portal reads; the login policy is
  `FirstUseDenyWithReview` (`harnesscred.go:476`) — an off-policy host refuses at once, never parks; the
  **chooser** `chooseAccountRole`/`chooseRole` (`cmd/wardyn-aws-sso/main.go:590-642`) blocks on `readLine`
  with no wall-clock bound when the row carries no account/role pin and more than one is reachable
  (`signin-pane.sh:114-121` documents it). The operator's row IS pinned (0.7.3 pin; the 0.7.5 report), so on
  that estate the seven minutes were most plausibly a marker that never reached `handleOutput` (a dropped
  attach socket, a respawn race, a buffer slice) — the server watch below covers that; the handoff sentence
  is written CONDITIONALLY so it is right on both estates. Server: `PUT /api/v1/internal/sso-token/{runID}`
  (`ssotoken.go:44-256`) audits `harness.credential.captured` synchronously :246-252 and answers 204; the
  upload is permitted for `terminalUploadGrace` = 5 min after the run goes terminal
  (`internal_live_run.go:18-43,169-182`); `harnessLoginIdleCap` 30 min (`harnesscred.go:55`); `/setup/status`
  has no cache; `source_run_id?: string` already in `lib/types/setup.ts:72`.

### Lane `starting-detail` (W2, **Opus** worker + Fable review; Go driver seam + store + API + UI) — Finding 6

**Design.**
1. **The seam — option (a): one new `SandboxSpec` field, no new `Runner` method.** `runner.SandboxSpec`
   gains `OnWaiting func(detail string)` — called while `CreateSandbox` is still BLOCKED, each time the
   substrate's reason for waiting CHANGES, with the substrate's own words (`"agent: ImagePullBackOff: …"`,
   `"proxy: Unschedulable: …"`, `"image: Pulling: <ref>"`); synchronous on `CreateSandbox`'s goroutine from a
   200 ms poll, so it must not block (the api implementation does one scoped UPDATE, only on change); never
   called after `CreateSandbox` returns; nil for driver-level callers (conformance, cmd/wardyn-runner). The
   dispatcher supplies it and writes `agent_runs.status_detail`. Rejected: a live probe (no `sandbox_ref`
   during STARTING); an audit row per change (the audit log is a hash chain, `recordAudit` SECURITY-DEFINER
   serialized — 5/s from a 200 ms loop is not a header).
2. **Staleness handled at READ, not by a clear-write.** `status_detail` is never cleared; `handleGetRun` /
   `handleListRuns` blank it for any run not in STARTING (one shared 6-line helper beside
   `projectRecordingMeta`). Zero extra writes on the hot path; the last-seen reason survives on a FAILED row for
   SQL postmortem (invisible in the console, where `failure_hint` is the field).
3. **Vocabulary — the server sends the raw reason, the UI owns the copy.** The column holds exactly
   `waitingDetail`'s shape `<component>: <Reason>[: <message>]`; a new 8-line `waitingReason(pod)` in
   `lifecycle.go` returns `waitingDetail(pod)` and, when that is `""`, `podStuckReason`'s condition as
   `"pod: <Reason>: <Message>"` (else `"pod: Pending"`). NEW CSS-free module
   `ui/src/app/components/screens/run-status-detail.ts` (importable by the pane, the header, the board AND
   Playwright — it must not reach `xterm.css`): `parseStatusDetail(raw)` splits the first two colons
   (component names and kubelet reasons never contain one; the message is the remainder — `// ponytail:` note),
   `statusDetailSentence(raw)`, `statusDetailChip(raw)` (a SECOND, short register for the ~20-character
   header chip — "Downloading the image" / "Waiting for a machine" / "Image pull failed" / "Container won't
   start" — because the sentence truncates to a restatement of the STARTING badge and the registry's words never
   appear; round-2 UX S9), `isTerminalStatusReason(reason)` (the TS twin of `terminalWaitingReasons`,
   cited by symbol):

   | reason (raw) | terminal? | sentence |
   |---|---|---|
   | `ContainerCreating`, `PodInitializing` | no | `STARTING_CONTAINER_CREATING` |
   | `Unschedulable` | no | `STARTING_UNSCHEDULABLE` |
   | `Pending` / empty | no | `STARTING_WAITING_FOR_NODE` |
   | `Pulling` (docker only) | no | `STARTING_FIRST_PULL` |
   | `ImagePullBackOff`, `ErrImagePull` | **yes** | `STUCK_IMAGE_PULL` + message |
   | `InvalidImageName` | **yes** | `STUCK_IMAGE_NAME` + message |
   | `CreateContainerError`, `CreateContainerConfigError` | **yes** | `STUCK_CREATE_CONTAINER` + message |
   | `CrashLoopBackOff` | **yes** | `STUCK_CRASH_LOOP` + message |
   | anything else | no | "Waiting: " + the raw server string (the same degradation `failure_hint` has, prefixed so a bare `pod: SomeReason: msg` never leads) |
4. **"First pull" — (ii) the kubelet's own `ContainerCreating`, hedged, + the free docker leg. No RBAC change.**
   Rejected (i) `events: get,list`: under `k8s.allowRunsInReleaseNamespace=true` it reads every co-tenant
   workload's event stream and a `fieldSelector` is a client convenience RBAC cannot enforce (the chart's own
   secrets comment makes the same argument). Rejected (iii) a `(node, image)` table: a migration + a write per
   run, wrong the first time a node is replaced or the kubelet GCs an image. Docker gets the UNCONDITIONAL
   sentence because `ensureImage` provably knows the host never had the image.
5. **Login pane verdict fold** (`login-start-wait.ts`): `startWaitVerdict` gains an optional `detail: string |
   null` and a new verdict `"stuck"`: a TERMINAL reason → `"stuck"` immediately, no clock (the pane ends the
   wait with the reason sentence — "ImagePullBackOff for two seconds is terminal"); a non-terminal reason →
   the clock is unchanged but `"slow"`'s sentence is replaced by the reason sentence ("ContainerCreating for
   two minutes is normal"); no detail (docker warm image, a pre-0.7.6 daemon) → today's behaviour byte for
   byte. The budget stops being the verdict and becomes the fallback for when there is no reason to read.

**Fix steps.** (1) `internal/runner/runner.go` — `OnWaiting` on `SandboxSpec` with the doc above. (2)
`k8s/lifecycle.go` — `waitingReason(pod)`; `statusFromPod` switches to it (strictly more informative; the
existing test still passes). (3) `k8s/sandbox.go` — `waitPodIP(ctx, podName, onWaiting)` and
`waitContainerRunning(ctx, podName, containerName, onWaiting)`: inside each poll compute `waitingReason(pod)`
and call `onWaiting` ONLY when it differs from a local `last` (the proxy pod's unscheduled fallback becomes
`"proxy: Unschedulable: …"`); `CreateSandbox` wraps `spec.OnWaiting` nil-safe once at the top. (4)
`docker/driver.go` — `ensureImage(ctx, ref, onPulling func())` calls `onPulling()` immediately before
`dockerutil.PullImage`; both call sites pass `spec.OnWaiting("image: Pulling: " + ref)`; `dockerutil`
untouched. (5) `internal/db/migrations/0063_agent_runs_status_detail.sql` — `ALTER TABLE agent_runs ADD COLUMN
status_detail text NOT NULL DEFAULT ''` (match `failure_hint`'s nullability exactly — read its migration
first). (6) `store.go` — `runCols += ", status_detail"`; `&r.StatusDetail` in `scanRun`; `SetRunStatusDetail`
beside `SetRunFailureHint` — **WITHOUT `updated_at=now()`**: the idle reaper and the killed-run tail-upload
grace measure from `updated_at` (`TouchRun`'s doc, `internal_live_run.go`), and a status heartbeat must not
extend either (say so in the doc comment). (7) `internal/types/types.go` — `StatusDetail string
`json:"status_detail,omitempty"`` documented: written only while STARTING, blanked at read otherwise, the
substrate's own words, display-only — AND `StatusReason string `json:"status_reason,omitempty"``, documented
as derived-at-read and never stored (the precedent is the projected fields at `types.go:213-218`,
`HasRecording` et al.); step 9's helper sets it beside blanking `StatusDetail` (round-2 general S2). (8) `runs_dispatch.go` — before `CreateSandbox` (:524) set
`spec.OnWaiting` via a NEW single-method interface `runStatusDetailSetter` type-asserted on `s.cfg.Store`
(mirroring the `SetRunFailureHint` setter assertion at `runs_lifecycle.go:541`; NOT a method on the store
interface — seven test stubs); errors logged at debug and discarded (a lost status line never fails a
dispatch). (9) `runs_policy.go` — `blankNonStartingDetail(runs)` in both `handleListRuns` arms and
`handleGetRun`. (10) `lib/types/runs.ts` — `status_detail?: string` AND `status_reason?: string` after `failure_hint`.
**Structured, not string-only (third-party review, adopted):** the ONE column keeps the raw substrate shape,
and the read helper in step 9 derives a second wire field `status_reason` = the bare reason token
(`ContainerCreating`, `ImagePullBackOff`, `Unschedulable`, `Pulling`, `Pending`…) so machines (metrics, live
specs, future automation) reason on a token while the UI owns the sentence; `parseStatusDetail` in the UI
prefers `status_reason` when present and parses the string only for a pre-0.7.6 daemon. Metrics:
`wardyn_run_start_wait_seconds{reason}` on the existing `/metrics`. The k8s copy stays CONDITIONAL ("if this
node has not pulled this image before") — never an assertion; only docker's `Pulling` (from `ensureImage`'s
own `imagePresent=false`) may assert a first pull. F4 and F6 share the `<component>: <Reason>[: <message>]`
vocabulary so a future unified `wait_state` (kind/reason/detail) can absorb both without a second protocol. (11) NEW
`screens/run-status-detail.ts`. (12) `run-detail-summary-header.tsx` — after the badge (:227) a Chip whose text is `statusDetailChip(raw)` and whose `title` is `statusDetailSentence(raw)`, `tone={isTerminalStatusReason(reason) ? "warning" : "info"}` (S9: a terminal reason is never an info chip; the chip is `max-w-[160px]` so the short register is the only one that survives it)
with `statusDetailSentence(run.status_detail)` when STARTING, same `min-w-0 shrink truncate` + `title`
treatment as the `failure_hint` chip and the same "may never hide at any width" rule. (13) `runs.tsx` — the
sentence as a second truncated line under the badge in the table row (:691) and in `RunCard`. (14)
`login-start-wait.ts` — `detail` input, `"stuck"` verdict, the two branches, the lead-in string. (15)
`harness-login-pane.tsx` — TWO edits only: pass `detail: run?.status_detail ?? null` into `startWaitVerdict`
in `pollRun`; in the `starting` block, `"stuck"` → the error phase with the reason sentence and "Try again" suppressed for a terminal image reason (U-11's `refused` precedent, `harness-login-pane.tsx:751-755`), `"slow"` with a
detail → the reason sentence instead of `LOGIN_SANDBOX_SLOW_START`. (16) `wardyn/copy.ts:378` `starting` —
unchanged (it answers "why the terminal is not open").

**DRAFT strings** (`screens/run-status-detail.ts` + `login-start-wait.ts`):
```ts
// DRAFT (M2 canon pending) — the ordinary wait. The kubelet reports ContainerCreating for a pull and for
// everything else it does before a container runs (canary.go's own comment), so the pull is named as the
// usual CAUSE, conditionally, never as the diagnosis — the hedge login-pane-copy.ts's U-12 note settled on.
export const STARTING_CONTAINER_CREATING =
  "Starting the sandbox. The first start after an update can take a couple of minutes while the image downloads.";  // UX round S9: no k8s nouns for a member; still conditional
// DRAFT (M2 canon pending) — docker only, the one place a FIRST PULL can honestly be ASSERTED (ensureImage).
export const STARTING_FIRST_PULL =
  "Downloading the image — this host has not run it before. The first start after an update takes a couple of minutes.";
// DRAFT (M2 canon pending) — the pod exists but nothing will take it. Not terminal.
export const STARTING_UNSCHEDULABLE = "Waiting for a machine with room for this sandbox.";
// DRAFT (M2 canon pending) — Pending with no container status at all.
export const STARTING_WAITING_FOR_NODE = "Waiting for a machine to start it on.";
// DRAFT (M2 canon pending) — TERMINAL; the registry's own words follow the colon because they name the fix.
export const STUCK_IMAGE_PULL = "The image could not be pulled:";
// DRAFT (M2 canon pending) — TERMINAL, and nothing about the cluster will change it.
export const STUCK_IMAGE_NAME = "That image reference is not valid:";
// DRAFT (M2 canon pending) — TERMINAL. The image exists; the kubelet would not make a container from it.
export const STUCK_CREATE_CONTAINER = "The sandbox container could not be created:";
// DRAFT (M2 canon pending) — TERMINAL for a START: the container starts and exits, repeatedly.
export const STUCK_CRASH_LOOP = "The sandbox container keeps exiting as it starts:";
// login-start-wait.ts — DRAFT (M2 canon pending) — the wait ending on a REASON rather than a clock; the
// sentence is the substrate's, this is only the lead-in that names the speaker (as SANDBOX_REFUSAL_LEAD_IN does).
export const LOGIN_SANDBOX_STUCK_LEAD_IN =
  "The sign-in sandbox cannot start — this needs your admin; trying again gets the same answer until they fix it.";   // round-2 UX S11: it also fires for CreateContainer*/CrashLoopBackOff, which are not image fixes
// (UX round S9: on a terminal image reason the pane's error phase SUPPRESSES "Try again", as U-11 does for `refused`.)
```

**Tests, red first.** Go: `k8s/lifecycle_test.go` `TestWaitingReason_FallsBackToScheduleCondition` (a Pending
pod with no container statuses and `PodScheduled=False{Reason:"Unschedulable", Message:"0/1 nodes are
available: 1 node(s) had untolerated taint {wardyn-coldpull: 1}"}` → begins `"pod: Unschedulable: "`; RED:
`waitingDetail` returns "") · `k8s/sandbox_test.go` (tag k8s) `TestWaitContainerRunning_OnWaitingFiresOncePerReasonChange`
(fake clientset; ContainerCreating for ~5 ticks → exactly ONE call; flip to ImagePullBackOff → a second; RED:
no parameter) · `store_test.go` (pg) `TestSetRunStatusDetail_RoundTripsAndDoesNotTouchUpdatedAt` · `runs_policy_test.go`
`TestGetRun_BlanksStatusDetailOnNonStartingRun` + `TestGetRun_ServesStatusDetailWhileStarting` ·
`runs_dispatch_test.go` `TestDispatch_WritesStatusDetailWhileCreateSandboxBlocks` (a fake runner calls
`OnWaiting` twice with the same string and once with another → the store saw TWO writes) ·
`docker/driver_test.go` (tag docker) `TestEnsureImage_ReportsPullingOnlyWhenAbsent`. vitest: NEW
`run-status-detail.test.ts` (ImagePullBackOff is terminal and carries the registry's words · ContainerCreating
renders the conditional first-pull sentence and is not terminal · an unknown reason renders the raw string ·
a message containing a colon survives · the terminal set equals `canary.go`'s list — the one cheap guard
against mirror drift) · `login-start-wait.test.ts` (a terminal reason ends the wait at two seconds with no
clock · ContainerCreating at two minutes is still a wait · a run with no `status_detail` grades exactly as
0.7.5 did — regression pin over the whole table) · `harness-login-pane-launch.test.tsx` (a STARTING run
reporting ImagePullBackOff shows the registry's words and offers Cancel ONLY — no Try again (the S9 ruling; a test asserting Try again would pin the pre-ruling behaviour) · ContainerCreating past 60 s shows
the first-pull sentence, not the generic slow-start one) · `run-detail-summary-header.test.tsx` (a STARTING
run's reason is on the header and never hides · a COMPLETED run carrying a stale detail shows nothing). Live
(**this lane owns case E and hands the others only constant names**): upgrade case E in place — the existing
`COLDPULL_TAINT` (:109) leaves the proxy pod `Pending`/`Unschedulable`, precisely `waitingReason`'s fallback →
add `expect(page.getByText(STARTING_UNSCHEDULABLE)).toBeVisible()` beside the :607-614 assertions (the pane now
names SCHEDULING for a wait that is not a pull — the whole of finding 6 in one assertion); NEW **case E2** "a
sign-in on an unpullable image fails in seconds with the registry's words" — launch with `WARDYN_AGENT_IMAGES`
pointed at a nonexistent tag, `STUCK_IMAGE_PULL` within 20 s (not 300 s). `scripts/kind-sso-walk.sh` unchanged.
Full vitest; `go test ./cmd/wardynd/...`; `make cover-check`.

**Docs (canon).** CHANGELOG: "A run that is slow to start now says what it is waiting on. The kubelet's own
reason (pulling an image, waiting for a node, a reference that will not pull) reaches the run header, the
Runs board and the sign-in pane, and a terminal reason ends the wait immediately instead of after five
minutes. The sign-in pane no longer grades a start purely on a clock." OPERATIONS.md (k8s substrate): the
reason vocabulary; the two real bounds (`podIPWaitTimeout` 90 s = a SCHEDULING bound on the proxy pod's CNI
IP, `canaryWaitTimeout` 3 min = the agent image's pull bound); a first pull after an upgrade does not fail a
run; **no chart diff, and why** (the reason comes from `pods: get`; the Events API is deliberately not read —
point at `rbac.yaml`'s paragraph so nobody "fixes" it by granting events). `make test-gaps` after cover-check.

**Risks / open.** The migration's `NOT NULL DEFAULT ''` is metadata-only on Postgres 11+ (confirm the floor
and match `failure_hint`'s nullability). Write volume is bounded by only-on-change (~3 writes/start; worst
case 5/s if a reason flaps — add a 1 s floor in the DISPATCHER closure, never in the driver, if the owner
worries). `updated_at` must NOT be bumped (confirm). A row that never leaves STARTING keeps its last detail
(postmortem; invisible). Open: the Runs board's group header (`runs.tsx:580`) aggregates N runs under one
badge — left out; owner's call (O-3).

**Size.** Go ~130 lines across 9 files + 1 migration; TS ~90 new + ~40 changed across 5 files; ~350 lines of
tests. ~1 Opus day, mostly the k8s fixtures.

### Lane `login-pane-tab` (W1, **Sonnet** worker + Fable review) — Finding 7a

**Design.** Open the tab ON THE CLICK, keep the handle, navigate it when the URL appears. Because
`window.open(url, "_blank", "noopener")` returns null by spec, the pattern is `const w = window.open("",
"_blank"); w.opener = null; w.document.write(placeholder); w.document.close();` — `opener` is a `[Replaceable]`
settable attribute, so assigning null is the standard pre-`noopener` mitigation and keeps the forward handle;
after navigation the document is cross-origin but `w.location = url` (write-only navigation), `w.close()` and
`w.closed` still work. NEW CSS-free module `settings/auth-tab-handle.ts` exporting `type AuthTab = {navigate(url):
void; close(): void}` and `openAuthTab(): AuthTab | null` (null when blocked). Lifecycle in the pane — one
ref, four call sites, **no give-up timer** (every exit path closes the tab; `// ponytail:`): (1) top of
`launch()` BEFORE the first `await` (a loud comment: a refactor that hoists an `await` above it silently
reverts the lane): `authTabRef.current?.close(); authTabRef.current = openAuthTab();` (2) in `handleOutput`
the `window.open` block becomes `authTabRef.current?.navigate(url)`; `setAuthUrl(url)` stays unconditional so
the header link still appears; (3) `cancel` and the `error` phase close it; (4) unmount cleanup, and the
success arms of `saveToken` / `confirmCapture` close it before `onDone()`. Popup blocked even on a gesture
(Safari strict / managed policy): `openAuthTab()` returns null, every later call is a no-op, 0.7.5's header
link is untouched, and `AUTH_TAB_BLOCKED_NOTE` renders under the device-code blurb (:838-844) when `authUrl &&
!authTabRef.current`. Both providers and both `startURLManaged` paths are covered because both Start buttons
call `launch()` and `handleOutput` already branches on `flow.capture`.

**DRAFT strings** (`settings/auth-tab-handle.ts`):
```ts
// DRAFT (M2 canon pending) — written into an about:blank tab the moment the person CLICKS Start, because
// that click is the only user gesture the flow ever gets. Plain text, no stylesheet, no font, no network.
export const AUTH_TAB_PLACEHOLDER_HTML =
  "<!doctype html><meta charset=utf-8><title>Wardyn — waiting for the sign-in page</title>" +
  "<body style=\"font:15px/1.6 system-ui,sans-serif;margin:3rem auto;max-width:34rem;padding:0 1rem\">" +
  "<p>Wardyn is starting your sign-in sandbox.</p>" +
  "<p style=\"color:#666\">This page changes to the AWS verification page by itself — usually within seconds, " +
  "up to a couple of minutes the first time. The code to enter is shown on the Wardyn tab; if nothing happens, " +
  "go back there — the link is on the sign-in panel too.</p>";   // the RULED body (round-1 UX S10); the constant is what ships
// DRAFT (M2 canon pending) — shown ONLY when the browser blocked the tab even on a click, once a link exists.
export const AUTH_TAB_BLOCKED_NOTE =
  "Your browser blocked the automatic tab — use the link above to open the verification page.";
```

**UX round rulings (S4, S10, S11):** the placeholder body reads: *"Wardyn is starting your sign-in sandbox.
This page changes to the AWS verification page by itself — usually within seconds, up to a couple of minutes
the first time. The code to enter is shown on the Wardyn tab; if nothing happens, go back there — the link is
on the sign-in panel too."* (the gesture-opened tab is FOREGROUNDED, so the person stares at it for the whole
sandbox start — 2 s warm, ~130 s cold — while the terminal and any refusal are on the other tab). The
device-code blurb at `:838-844` becomes *"In the tab that opened (or the link above), enter the user code
shown in the terminal and approve. Wardyn captures the session automatically when the login completes."*
Esc / overlay-close of any dialog hosting the pane routes to `cancel` while a run is live, and the unmount
cleanup closes the tab.
**Tests, red first.** NEW `auth-tab-handle.test.ts`: opened empty (`window.open` called with `("", "_blank")`,
no `noopener` feature) · the opener is severed before navigation (order recorded) · a blocked popup yields
null and later calls are no-ops. `harness-login-pane-launch.test.tsx`: clicking Start opens a tab
SYNCHRONOUSLY before the launch POST resolves (`harnessLogin` mock never resolves; `window.open` already
called — **the case that goes red on any refactor that moves the open below an await**) · the verification
URL navigates the tab already open (`location` set on the same stub; `window.open` called exactly once) · a
blocked popup still surfaces the header link and `AUTH_TAB_BLOCKED_NOTE` · Cancel closes the tab. Playwright
`ui/e2e/providers.spec.ts` (this lane's spec): `const pagePromise = context.waitForEvent("page"); await
startSignIn.click(); const tab = await pagePromise;` (RED today: nothing opens until a URL arrives, and then
it is blocked) and assert the new page's title is the placeholder's ("Wardyn — waiting for the sign-in page"). The
NAVIGATION cannot be asserted on the walk: `extractDeviceVerificationUrl` admits only
`https://device.sso.<r>.amazonaws.com/…` and `https://*.awsapps.com/…` (:329), and the fake prints
`http://wardyn-awsssofake:8090/verify?user_code=…` (`server.go:324-325`) over plain http — so the proof is
SPLIT: vitest pins `location` on the stubbed handle; Playwright/W5 pins that a page opens ON THE CLICK. One
sentence in TEST-GAPS: on the kind walk the URL is never extracted, so neither the auto-navigation nor 0.7.5's
header link is exercised live (the extractor is not widened for a test host — that would put a test hatch in
the console). Full vitest.

**Docs (canon).** CHANGELOG: "Signing in opens its own tab. The tab is opened on the click that starts the
sign-in — the only user gesture in the flow — and navigated to the verification page when it appears, so
browsers no longer block it. If your browser blocks it anyway, the link on the panel still works and now
says so."

**Risks / open.** Confirm the console's CSP does not `sandbox` an opened window before relying on
`document.write` (fallback `w.document.body.textContent`). Very old Safari ignored `opener = null`; the
destination is AWS's page, so the residual is not a threat — say so in the comment. Open (O-4): the anthropic
flow gets the tab too by construction — a behaviour change the report did not measure.

**Size.** ~70 new lines, ~20 pane lines, ~120 test lines; half a Sonnet day.

### Lane `login-pane-confirm` (W1, same Sonnet agent, after `login-pane-tab`) — Finding 7b

**What is actually missing (reconciled).** The corroboration narration and its bounded retry shipped in
0.7.5; the missing state is ONE STEP EARLIER — the window between the CLI's success line and the marker,
where the pane still shows "open the link, enter the code, approve" + Cancel — and a path to `onDone` that
does not depend on the sandbox's marker byte reaching the browser.

**Design** — three additions, none weakening S-13:
1. **A "signed in — handing over" state driven by a hint that is never trusted.** `extractSignedIn(buf)`
   (case-insensitive `successfully logged into`; the CLI's string, not Wardyn's — keep the matcher loose) sets
   a `signedInRef` + `signedIn` flag; NOT a phase, authorises NOTHING — it swaps one sentence and starts one
   poll (say so beside the S-13 reasoning). The sentence is written conditionally so it is right whether or
   not the row is pinned: `CAPTURE_HANDOFF` below.
2. **A background server watch from the same moment.** `watchForCapture({provider, runId, signal})` in
   `capture-confirm.ts` polls the login run's OWN audit — `GET /audit?run_id=<login run>&action=harness.credential.captured` (written synchronously at `ssotoken.go:246-252`; a member can read their own run's trail; exact and strict by construction — it is THIS run's capture or nothing) — on a back-off schedule, and on a hit reads `/setup/status` ONCE for the S-13 corroboration (`strict`) and to refresh the door; never `/setup/status` per tick (the "expensive endpoint" the door lane refused to poll faster than 5 min), `CAPTURE_WATCH_SCHEDULE` (2 s → 3 s after 30 s → 10 s after 2 min → 30 s after 10 min; immediate ticks on the CLI line, a run-state transition and return-to-visible — O-5) while `signedIn &&
   !savedRef.current`; on confirmation → `setAutoCaptured(true); setPhase("done"); onDone()` — **the pane
   closes on the server's word, with or without the marker.** Two hard rules: **strict predicate only** —
   `serverConfirmsCapture(status, provider, runId, {strict:true})` requires `captured && source_run_id ===
   runId` and REFUSES the presence fallbacks (a free-running poll with the fallback would confirm on a
   PRE-EXISTING credential and close the pane over a sign-in that had not happened — the one dangerous line
   in the lane; the Fable review must confirm the watch never uses the fallbacks); **every read error is a
   tick, not a verdict** (the watch's only terminal outcome is a confirmation; it can never refuse). **Bound =
   the server's own window, no new number:** poll `GET /runs/{id}` on the same tick; once terminal, keep
   watching `/setup/status` for `CAPTURE_POST_RUN_GRACE_MS` = 5 min (mirrors `terminalUploadGrace`, cited by
   symbol) then stop — the ceiling is the run's own life (`AutoStopAfterSec` 30 min) + 5 min.
3. **The marker path stops refusing prematurely.** `confirmCapture`'s `{confirmed:false}` at 1.5 s hands back
   to the watch (keep `CAPTURE_VERIFYING` visible) while the window is open; `CAPTURE_NOT_CORROBORATED` /
   `CAPTURE_CHECK_UNREACHABLE` fire when the WATCH expires. S-13 preserved: a forged marker never converges and
   still lands on `CAPTURE_NOT_CORROBORATED` — later, and having stopped accusing an honest capture still in
   flight (write this in the comment — the first question a security reviewer asks).

| situation | 0.7.5 | 0.7.6 |
|---|---|---|
| `attached`, no CLI success line | blurb + Cancel | unchanged |
| `attached`, CLI success line seen | same blurb + Cancel (the bug) | `CAPTURE_HANDOFF` + Cancel; watch running |
| `attached`, chooser prompting | same blurb | `CAPTURE_HANDOFF` — points at the terminal |
| marker seen, server agrees | `done`, `onDone()` | unchanged |
| marker seen, server disagrees at 1.5 s | `error` | `CAPTURE_VERIFYING`, watch continues |
| no marker, server has the credential | waits forever | `done`, `onDone()` — the pane closes |
| forged marker | `error` at ~1.5 s | `error` when the watch expires |
| run ended, capture lands in the 5 min grace | waits forever / errors | `done`, `onDone()` |

**Fix steps.** (1) `capture-confirm.ts` (owned by this lane): `strict` option (default false — every existing
caller and S-13 test unchanged; consider a required positional so a new caller cannot forget it);
`CAPTURE_WATCH_POLL_MS`, `CAPTURE_POST_RUN_GRACE_MS`; `watchForCapture` (pure enough to test without React);
`extractSignedIn`; `CAPTURE_HANDOFF`. (2) `harness-login-pane.tsx`: call `extractSignedIn` in the helper branch
before the marker checks (once, via ref); a `useEffect` starting `watchForCapture` when `signedIn && phase ===
"attached"`, aborted on unmount / phase change; the blurb at :838-844 renders `CAPTURE_HANDOFF` when
`signedIn`; `confirmCapture`'s `{confirmed:false}` → back to `attached` with `CAPTURE_VERIFYING` while the
watch lives. (3) Server: **no change** (grace, audit and `/setup/status` are already right).

**DRAFT strings** (`capture-confirm.ts`):
```ts
// DRAFT (M2 canon pending) — 0.7.5 field report, finding 7b. The AWS CLI prints "Successfully logged into
// Start URL" and hands over to wardyn-aws-sso, which uploads the session — and which, on a roster row with
// no account/role pin and more than one reachable, ASKS WHICH ONE over this terminal (chooseAccountRole;
// signin-pane.sh says the same). Until now the pane kept showing "open the link, enter the code, approve".
// A HINT, NEVER A VERDICT: the CLI's line is a PTY string, forgeable like the done marker (S-13). It changes
// ONE sentence and starts ONE server poll; the server's own /setup/status is the only thing that closes
// this pane, and it is checked strictly (source_run_id must equal THIS run's id).
export const CAPTURE_HANDOFF =
  "The sign-in tool reports you are signed in. Wardyn is waiting for the sandbox to hand over your session. If the terminal above lists accounts or roles, click or tab into it, type the number you want and press Enter — it may ask twice, account then role.";  // round-2 UX S10 + nits: no "Signed in." verdict on forgeable evidence (CONSOLE-RULES §10); chooseAccountRole prompts twice (`wardyn: account [1-N]:` then `wardyn: role [1-N]:`, cmd/wardyn-aws-sso/main.go:276-280); the pane is keyboard-focusable
```

**Tests, red first.** `harness-login-pane.test.tsx` (holds the 19 S-13 / `serverConfirmsCapture` pins — regression only, no new cases; the file is at the gate) + NEW `capture-confirm.test.ts`: strict corroboration refuses a live
`model_access` with no `source_run_id` — **the most important case in the lane** · non-strict unchanged
(regression pin over every S-13 case) · the watch treats a thrown read as a tick · …an unreachable payload as
a tick · confirms and stops the moment `source_run_id` matches · expires five minutes after the run goes
terminal (fake timers). NEW `harness-login-pane-confirm.test.tsx` (the 944-line file is at the gate; same
split `-launch.test.tsx` established): the CLI's success line replaces the blurb with the handoff sentence —
RED · a capture the server confirms closes the pane though the marker never arrived (`onDone` called) — **the
headline** · a marker whose corroboration fails at 1.5 s keeps verifying instead of refusing — RED · a forged
marker still ends in `CAPTURE_NOT_CORROBORATED` once the watch expires · the pane does NOT close on a
credential that predates this sign-in (someone else's `source_run_id`; `onDone` never called). Live: no
spec change needed (cases C/D walk a successful capture and pass sooner); the marker-less path has no hook in
the walk — deferred, said in the REPORT. Full vitest.

**Docs (canon).** CHANGELOG: "The sign-in panel now says when you are signed in and Wardyn is waiting on the
sandbox to hand over the session — including when the sandbox is asking which AWS account or role to use,
which it does in the terminal on that panel. And the panel closes itself as soon as the server confirms the
session was stored, whether or not the sandbox's own success message reaches the browser." OPERATIONS.md (AWS
SSO): pin `sso_account_id`/`sso_role_name` on the roster row to skip the chooser (0.7.3), pointing at
`signin-pane.sh`'s paragraph.

**Risks / open.** Poll volume is bounded by the back-off schedule (O-5, resolved); `/setup/status` is uncached,
so the schedule's fast phase is the first two minutes only. The CLI's string will change one day; the failure
mode is 0.7.5's behaviour. Auto-open (7a) and auto-close (7b) are ENHANCEMENTS, never dependencies: with a
blocked tab or a lost marker the 0.7.5 link and the Cancel path still work — pin both in tests.

**Size.** ~90 lines in `capture-confirm.ts`, ~35 in the pane, ~200 test lines; half a Sonnet day to a day.

**Ordering on `harness-login-pane.tsx` (910 lines, three lanes):** `login-pane-tab` → `login-pane-confirm`
(W1, one agent) → `starting-detail`'s two call-site edits rebase on top (W2); every new function lands in a new module and the pane gets call
sites only (target ≤ 965 lines).

### Lane `e2e-sso-path` (W3, **Opus** worker + Fable review) — the live proof; sole owner of `ui/e2e/live/**`

Cut from the merged tip after W1+W2. Adds to `ui/e2e/live/sso-member-recovery.spec.ts` the cases each lane
handed over by constant name (their canon docs files list the imports): **I** (banner: never-signed-in member
sees the strip on `/runs` and `/workspaces`; signs in from it; the strip clears without reload) · **A(rail)+**
(`RAIL_MODEL_ACCESS.NOT_SIGNED_IN` visible, `NO_PROVIDER` count 0) · **J** (run door — only if the hold lane's
awsssofake `invalid_grant` control can drive a run to a DISPATCH-time refusal; else the Playwright fixture is
the pin and J is recorded as not-live in TEST-GAPS) · **K / K(resume)** (the held run and its resume, same run
id) · **E+** (`STARTING_UNSCHEDULABLE` under the coldpull taint) and **E2** (`STUCK_IMAGE_PULL` within 20 s on a
bad tag) · the tab-open case lives in `ui/e2e/providers.spec.ts` (owned by `login-pane-tab`). Iterate with
`local/v076/scratch/live-iter.sh` (copy 0.7.5's) — 30 s–3 min per spec run against the cluster a walk left —
never a 12-minute walk per iteration. `scripts/kind-sso-walk.sh` gains the fake's reauth control only. **Negative live cases (third-party
review, adopted where the walk can drive them):** banner — a shared-credential member gets no sign-in CTA;
an admin with a live credential sees no strip; the strip is absent on `/settings` and `/setup`; a sign-in
clears it without reload. Re-auth — a capture by the WRONG user does not resolve the hold (I2); a sign-in
started BEFORE the request does not resolve it (I6; drive with two login runs); the held run killed during
the hold → the row goes CANCELLED and the SDK gets 401 within one poll; the hold times out (short knob on the
walk) → 401 + the banner and row survive; `awsMount` / bearer lanes show no re-auth behaviour. K asserts, on
resume: same run id, same sandbox ref, same policy, no new run, no new grant, no `run.create` row after the
hold. Proxy — a bad `WARDYN_DAEMON_PROXY_URL` refuses boot (unit), the override host bypasses (unit).

### Lanes `docs` (W3, **Sonnet**) and `release` (W7, **Opus**)

`docs` assembles `local/v076/canon/*-{changelog,docs}.md` into CHANGELOG `[0.7.6]` (Added / Fixed / Known gaps —
incl. retiring the two 0.7.5 gaps named above, naming both migration stems, carrying forward the three
already-promised 0.7.6 items as 0.7.7 unless O-1 says otherwise), OPERATIONS.md, THREAT-MODEL.md, ENV.md,
AUDIT-ACTIONS.md, helm README, TEST-GAPS.md, ROADMAP.md's Shipped table, `docs/adoption/*` notes; ONE Fable
docs-claims review, looped to clean; merges LAST. `release` copies 0.7.5's `local/v075/release/` tooling and
`RUNBOOK-W4-W7.md`, produces the release commit and `make release-check` on it, and STOPS at the owner gate
(O-2).

## Waves and fan-out

- **W1 (parallel; ≤4 agents alive incl. reviewers; ≤3 gate-runners):** in launch order —
  `daemon-egress-proxy` (Sonnet; independent, unblocks the operator) · `sso-refresh-visibility` (Sonnet; lands
  before the hold lane) · `ui-model-access-door` (Opus; lands FIRST of the UI lanes incl. the CONSOLE-RULES
  re-cite) → then `ui-new-run-model-access` (Sonnet) and `run-credential-door` (Opus) in parallel ·
  `login-pane-tab` then `login-pane-confirm` (ONE Sonnet agent, sequential, one worktree — both touch
  `harness-login-pane.tsx`). Each lane: red-first tests → fix → gates → REPORT → **blind Fable review**
  (FIX-FIRST loop; the coordinator re-reviews fix diffs on the diff, running an independent single-clause
  mutation when a lane's mutation log looks cumulative) → coordinator rebases onto `feat/v0.7.6` and
  ff-merges.
- **W2 (after `ui-model-access-door`, `run-credential-door` and `sso-refresh-visibility` merge):**
  `credential-reauth-hold` (Opus; needs the door dialog as the every-screen surface, the `credential` ending
  kind, and the spent-aware grading; its docker-gated SDK-tolerance measurement runs FIRST and sets the knob
  default) · `starting-detail` (Opus; independent files; may start during W1 if a slot is free; its two
  `harness-login-pane.tsx` call-site edits rebase on top of the two login-pane lanes).
- **W3:** `e2e-sso-path` (Opus) · `docs` (Sonnet; merges LAST after ONE Fable docs-claims review loop).
- **W4:** clean-tree gates on the final tip in `~/wt-v076-final` (make ci; test-report-pg; full Playwright
  sweep one spec per call; docker-gated SSO pair + TestBootEgress on REBUILT images; k8s conformance).
- **W5:** kind SSO walk ×2 (REBUILD, fresh install each) on the final tip, `local/v076/evidence/kind-sso/walk-<n>/`;
  a Fable evidence verifier reads the walk artefacts, not the tail.
- **W6:** blind Fable lenses on the final tip — S (security: the MITM'd portal.sso lane, the hold's fail-closed
  + bound, the internal token endpoint's run-token binding, the reauth request's owner scoping, the daemon proxy
  knob), U (console copy truth: banner / rail / door / pane / starting sentences on every deployment shape and
  audience), I (images + kind test fake). Exit rule: no finding that would ship a defect. Fix wave → re-round on
  changed areas only.
- **W7:** release per `local/v076/RUNBOOK-W4-W7.md` (0.7.5's copy): make-notes → release-commit --dry-run →
  --apply → `make release-check` on the release commit → Opus notes truth-check loop until CLEAN → **STOP:
  hand the owner the exact push/tag/release commands** (or execute them under an explicit 0.7.6 `/goal`).

## Verification (the definition of done)

1. Every lane: red-first logs under `local/v076/evidence/<lane>/`, gate exit codes 0 (Go: full
   `go test ./cmd/wardynd/...` + `make cover-check`; UI: FULL vitest + the screen's Playwright spec), Fable
   review CLEAN (or NIT-only, coordinator-swept).
2. W4 green on the final tip: `make ci` uninterrupted; `test-report-pg` 0; Playwright sweep 30+/30+;
   docker-gated 0 on rebuilt images; k8s conformance 0.
3. W5: **two** green kind SSO walks on REBUILT images from fresh installs, each including the new live cases
   (banner, rail, door, reauth hold, starting detail, tab open, confirm-close) — evidence copied per walk.
4. W6: three blind lenses with no defect-shipping finding open.
5. Docs: CHANGELOG `[0.7.6]` + Known gaps assembled from canon; Opus truth-check CLEAN; `release-check` green
   on the release commit.
6. Memory + ledger updated; Iris writes retried (or queued).
7. **Success criteria (measurable, per finding):** F2 — a never-signed-in member sees the sign-in action on
   `/runs` without visiting Getting Started (live case I). F1 — the rail names the person's state and never
   says "No model provider is connected" on a per-user row (A(rail)+). F3 — a credential-refused run's page
   carries the sign-in where the person is (Playwright fixture; J when forceable). F5 — a refresh token AWS
   has retired never grades `live` after the daemon observed it (the named regression test). F6 — a stuck
   start ends the pane's wait in seconds with the registry's words; a slow start names scheduling or pulling
   (E+, E2). F7 — the tab opens on the click (providers.spec) and a confirmed capture closes the pane within
   one watch tick of the server's answer (unit + C/D). F8 — the daemon's CreateToken succeeds through a
   forward proxy with no process-wide proxy env (unit; documented operator recipe). F4 — measured SDK
   tolerance ≥ the shipped default; a mid-run lapse holds and the SAME run resumes (K/K(resume)).
8. **Upgrade and rollback:** W4 runs the migrations against a 0.7.5 database snapshot (existing approvals and
   runs still list/decide; the new kind round-trips; `status_detail` reads blank on old rows); OPERATIONS
   states that a downgrade to 0.7.5 with `credential_reauth` rows present is unsupported and the runbook's
   pg_dump is the rollback; mixed-version (an old replica) is already refused by `claimSingleInstance`.
9. **Provenance in W5 evidence:** each `walk-<n>/` carries `MANIFEST.json` — git SHA, the five image digests
   (node == host ids), chart version, `wardynd` version from `/healthz`, cluster name, build timestamps — so
   the walked artefact is provably the commit under test.
10. **Secret non-observability (F4):** one test greps proxy logs, audit rows, HTTP bodies, the sandbox cache
    file and the walk's captured output for the injected token and the refresh token — must find nothing.

## Open decisions for the owner (each has a default the plan executes unless overridden)

- **O-1** Bundle the three already-promised 0.7.6 follow-ups (per-person advisory lock for the supersede race,
  the k8s cache volume, the admin-tier 5xx sweep)? Default: NO — the field report is the release; they move
  to 0.7.7 in Known gaps (the hold lane notes the supersede race explicitly).
- **O-2** Release authority: the plan ends at "release commit verified + exact commands delivered" unless a
  `/goal` for 0.7.6 says otherwise (as for 0.7.5).
- **O-3** `starting-detail`: also render the waiting sentence on the Runs board's GROUP header (`runs.tsx:580`,
  one badge over N runs)? Default: NO (row + run header + pane only).
- **O-4** `login-pane-tab`: the Anthropic sign-in flow gets the auto-opened tab too (it shares `launch()`).
  Default: YES — identical code, and the same popup rule applies.
- **O-5** RESOLVED (third-party review): the server watch backs off — 2 s for the first 30 s, 3 s to 2 min,
  10 s to 10 min, 30 s after — with an immediate tick on the CLI's success line, on a run-state transition and
  on return-to-visible; worst case on an abandoned sign-in ≈ 150 requests, not 1400.
- **O-6** RESOLVED (third-party review): the capture resolves the re-auth through a dedicated
  `credential.reauth.resolved` audit action (the row still moves to APPROVED so the existing list/count UI
  works); `approval.decide` is never written for a decision nobody clicked.
- **O-7** `daemon-egress-proxy`: a credentialed corporate proxy (`user:pass@`) is refused in 0.7.6; a
  secret-ref form (`WARDYN_DAEMON_PROXY_SECRET`, mirroring `UpstreamProxySecretRef`) is a follow-up. Default:
  refuse + document.
- **O-8** `sso-refresh-visibility`: the spent mark stays in-memory (a daemon restart re-grades `live` until the
  next dispatch re-derives it). Default: keep in-memory; persisting is a migration for a re-derivable fact.
- **O-9** `credential-reauth-hold`: `WARDYN_CREDENTIAL_REAUTH_TIMEOUT` default 600 s is provisional until the
  docker-gated SDK-tolerance measurement lands; the lane reports the measured number and the owner confirms
  the default before the release notes are cut.
- **O-10** `credential-reauth-hold`: the default of the kill switch `WARDYN_AWS_SSO_PROXY_INJECT`.
  Recommendation: `on` only if the docker-gated measurement AND the dedicated F4 security round are CLEAN on
  the release tip; otherwise ship 0.7.6 with the lane `off` by default (documented as the way in) rather than
  slip the seven UX/observability lanes. Either way the flag exists and is the rollback.
- **O-11** `ui-model-access-door`: the strip's dismissability — RULED by the two UX rounds: "Not now" (per
  viewer, per browser session) for `not_configured` and `shared_expired`; `expiring` and `expired_signin`
  undismissable. The owner can override.

## Contradictions with the field report (the code, with evidence — the plan follows the code)

1. **F1 — the warning is keyed on `hasLlmPath(st)` (`readiness.ts:59-61`), not on the server's `llm_ready`.**
   Same conclusion (a deployment-wide fact, true under a per-user row), but the CHANGELOG must not claim
   `llm_ready` was misread.
2. **F1/F2 — "they can complete the whole New Run form and launch": on 0.7.5 the launch is REFUSED at the
   click** with a 422 from `enforceCreateLLMMechanism` for every model-calling shape; a non-interactive
   workspace-bound run is a scan run with no model call. "Find out by failing" is exactly right; "launches"
   is not, so the rail's sentence says "refused at launch" and no client-side Launch block is added.
3. **F3 — "the Runs board row (same truncated chip)": there is no chip on the board** (`run-card.tsx` never
   reads `failure_hint`). The board keeps its clone door; the run page carries the sign-in.
4. **F2 — the pane is already mountable in a pre-sized Dialog** (`connection-cards.tsx:705-725`), which works
   in focus mode and on the cockpit — the strip's door is that Dialog, not an inline expansion; and the strip
   is NOT rendered on `/setup` and `/settings`, which already mount the pane (one control per page, U-13).
5. **`model_access` grades the claude-code row only, while `per_user` is savable on a codex row** — the gate
   is `id === "claude-code" && isPerUserSsoRow(row)` (`connection-cards.tsx:461`), never `isPerUserSsoRow` alone.
6. **The 0.7.5 Known gap "a shared-row admin is offered the member-side CTA" describes a working path**
   (`authorizeHarnessLogin` admits any operator, `harnesscred.go:708-710`; `agents-tab.tsx` mounts the pane
   with `startURLManaged=false`); members never see an actionable shared state (`memberModelAccess`). Retired
   with the citation, not fixed.
7. **F4 — the proxy never sees the per-user Bedrock call** (`mitm.go:176-182`) and the credential is resolved
   once at dispatch; there is no place to hold until Phase B creates one — Phase B is the code's own named
   next step (`runs_bedrock.go:568-574`), and the mid-run renewal channel already exists as the injector's
   re-resolve loop (`inject.go:135-157`). The hold is ~150 lines of that lane, not "a second caller of
   `ResolveWait`" (whose call site is host-scoped and never reached for an allowlisted host).
8. **F4 — Phase B does not change `credential_residency` or the rail chip:** the SigV4 role credentials the SDK
   mints stay resident (THREAT-MODEL's own row); only the captured-SSO row's prose changes.
9. **F4 — `WARDYN_GIT_APPROVAL_TIMEOUT` lives in the proxy SIDECAR, read with `os.Getenv`, with NO upper clamp**
   — copy the location, not the missing clamp. A new approval kind costs a migration (the column CHECK). A
   member cannot decide a non-`egress_domain` approval, so the re-auth item is a door, not a decision. The
   kind fake's Service port (`:8090`) must reach the authored injection host or the walk 401s silently.
10. **F5 — the retry already exists:** `createAWSSSOTokenWithRetry` retries once after 400 ms; the 01:09 `EOF`
    was two attempts, both at the network — the more likely reading given F8. And the grading bug is WORSE than
    reported: a spent token grades `live` until the client REGISTRATION lapses (days), not for an hour. The
    right target state while the access token still works is `expiring` (actionable, with a real deadline).
11. **F6 — the 90 s bound is a SCHEDULING bound** (`waitPodIP`; the CNI assigns the IP before any pull), the
    agent pull sits under the 3-min canary, the 127/131 s starts were expected — no timeout changes. During
    the whole STARTING window there is no `sandbox_ref`, so a live probe is impossible, not merely costly. The
    chart grants no `events` verb BY DESIGN; the honest k8s first-pull sentence is conditional — and docker
    is the one substrate that can assert a first pull (`ensureImage`).
12. **F7b — the corroboration narration and its bounded retry shipped in 0.7.5** (`CAPTURE_VERIFYING`,
    `confirmCaptureWithServer`); the missing state is one step EARLIER (between the CLI's success line and the
    marker), and the pane closes on `onDone` — it just never reaches it. The seven minutes: on an unpinned
    row the chooser blocks on a human (`signin-pane.sh:114-121`); on the operator's pinned row, a marker byte
    that never reached the browser — the strict server watch closes both.
13. **F8 — `SiteConfig.upstream_proxy_url` cannot serve the path that matters most:** OIDC discovery runs at
    boot before any SiteConfig read, and the field is admin-editable at runtime while the daemon's own
    `CreateToken` carries the client secret — a deployer-tier env knob beside `WARDYN_TRUSTED_CA_FILE` is the
    supported answer.

14. **The operator's closing line — "Step 4 is a second caller of `ResolveWait`. That is the whole list." —
    overstates F4.** Seven findings are existing-signal-to-existing-surface work; F4 is a new SSO proxy
    injection path plus bounded hold semantics, shipped as a gated security feature (see the lane).

## Principles (how to read the lane text)

- **Hard constraints:** no behaviour regression; the file-size cap; the audit, envdoc, migration-doc and
  citation guards; the security invariants I1–I8; public API compatibility; the strings law. **Implementation
  preferences** (a "zero-line diff", "2 lines", "~35 lines", a named line number): guidance that keeps a lane
  small and a guard green — never a reason to contort a design. If the clean shape is a new package, make it.
- **Strings are hypotheses until the UX round rules.** Every DRAFT string in this plan passes the UX Fable
  round (and the general round's truth check on every deployment shape) BEFORE a lane brief carries it; lanes
  implement the RULED strings verbatim and never treat a draft as a requirement.
- **One primary recovery action per state per screen** (not "one button with this name per page"). On the
  run cockpit, where three surfaces could carry "Sign in to AWS" once F4 lands, ownership is fixed: the
  held-approval row while the run is held, the failure block when the run FAILED for a credential, the
  strip's button otherwise (the strip keeps its sentence, drops its button, when the page has claimed the door).
- **Recovery never widens authorization:** a recovery event may refresh credential material; it must not
  re-authorize an operation against a different principal, source, mechanism, account, role, region or host.

## Plan review protocol (before execution — the owner's instruction)

- **Round 1 (two Fable agents in parallel, blind, read-only on the repo at dfa89f60):** (a) a GENERAL
  `independent-reviewer` reading this plan end to end against the code — design soundness, every file:line,
  the laws, test red-first-ness, sequencing, size caps, migration/audit/envdoc guards, security of the hold
  and the daemon proxy; (b) a separate **UX-specialist** reviewer reading only the person's journey — the
  banner, the rail, the run door, the login pane's states and copy, the starting sentences, the held-run row;
  one-control-per-page, focus mode, a11y (role=status, accessible names, keyboard), the DRAFT strings' truth on
  every deployment shape/audience, and whether a member who has never read a release note can act on every
  screen. Both return findings as `local/v076/plan-review/ROUND-1-{general,ux}.md`-shaped reports (blocker /
  should / nit, each with the plan line and the evidence). The coordinator applies every blocker and should,
  records each ruling in a "Review rulings" appendix, and copies the full plan text to the owner's clipboard
  (`clip.exe`) the moment round 1 is fanned out.
- **Round 2 (one Fable general reviewer on the applied plan; the UX reviewer re-checks only the UX sections
  that changed):** apply; then `ExitPlanMode`.
- Fable is probed with one small sync agent first; on 429 / stall the round runs on Opus 5 and the ledger says so.

## Review rulings

**Round 1 — third-party review (owner-supplied, 2026-09-18).** Adopted: F4 elevated to a gated
security-sensitive lane with Phase 0 threat-model delta, invariants I1–I8, an explicit state machine,
concurrency and cancellation contracts, generation-based stale-capture protection (I6), the roster-drift
substitution hole closed by a dispatch-time scope snapshot (I3 — verified: `awsSSOScopeFor` reads the roster
at call time, `modelaccess.go:111-117`), dedicated `credential.reauth.resolved` instead of `approval.decide`
(O-6), the kill switch `WARDYN_AWS_SSO_PROXY_INJECT` (O-10), per-run masking instead of `AddGlobal` (the global
set has no expiry), the dedicated F4 security round S1–S13 + a state-machine test, secret non-observability,
metrics on the existing `/metrics`, measurable success criteria, upgrade/rollback and provenance verification,
negative live cases, the K resume assertions, structured `status_reason` beside `status_detail` (F6), the
in-process `Cause` for F5, the F7b back-off schedule (O-5), `URL.User` refused at parse + the F8 test matrix,
the "Not now" session dismissal for `not_configured` (O-11, pending the UX round), "one primary recovery
action per state per screen", hard-constraints-vs-preferences, strings-as-hypotheses, the non-goals list,
Step 0 artifact lineage. Rejected / modified with reasons: **split F4 into 0.7.7** — kept in 0.7.6 behind the
switch, sequenced last, so the seven UX lanes never wait on it (the owner's ask is all eight; the switch is
the containment); **the public README says v0.6.0** — the in-repo README at dfa89f60 names v0.7.5 (a stale web
view), lineage is still verified in Step 0; **`status_reason` as a second COLUMN** — one column, two derived
wire fields (the token is derivable at read); **load tests at 100 runs** — a proxy unit test with 64
concurrent resolvers proves the one-workflow property; the kind walk is not a load rig; **the unified
`wait_state` abstraction** — noted as a 0.7.7 design; F4 and F6 share the vocabulary so it can absorb both.

**Round 1 — Fable general (blind, 2026-09-18).** All five BLOCKERS adopted: B1 the injection grant host stays
BARE and the port rides the egress allowlist so `AuthoredPortFor` credentials the fake lane (was: a
port-qualified grant host that `byHost` would never match); B2 the hold polls the APPROVAL by id with the
existing `approvalClient.poll` and re-resolves the injection exactly once on APPROVED (was: re-polling the
injection URL, which re-mints and writes a `credential.mint` audit row per tick); B3 the kind fake gains
token-TTL and role-cred-TTL knobs and case K gets a 12-minute timeline (was: infeasible against the fake's
fixed 1-hour TTLs); B4 the tab proof is split — vitest pins navigation, Playwright/W5 pins the page opening on
the click — because the extractor's host allowlist excludes the fake's http host; B5 the S-13 pins live in
`harness-login-pane.test.tsx`, `capture-confirm.test.ts` is NEW. All nine SHOULDs adopted: S1 hand re-cite of
all three app-shell citations (the guard cannot see the drift); S2 `decide()` refuses the new kind (409) and
the server-side raise is capped per run; S3 no hold at proxy boot; S4 the `expiring` deadline is
`ExpiresAt − skew`; S5 the run door is gated on the viewer owning the run and refreshes the door on mount; S6
six `DefaultTransport` consumers with the OIDC internal issuer and the content-scan sidecar auto-bypassed; S7
the confirm watch polls the login run's own audit row, not `/setup/status`; S8 pane ordering = W2's; S9
cockpit door ownership fixed (row / block / strip). NITs applied (line numbers, the new secret-name constant,
the pending-row query, the CLI print order, RequireSetup).

**Round 1 — Fable UX specialist (blind, 2026-09-18).** All four BLOCKERS adopted: B1 the strip is withheld
on `/settings` / `/providers` only for an operator (a member's Settings AWS button is admin-only), and the
per-user refusal sentence's destination becomes the banner / Getting Started with "again" dropped for
`not_configured`; B2 the cockpit's three false sentences ("approve to let it through", "requires the admin
role", "blocked until an admin decides") exclude the reauth kind, a `REAUTH_HEADING` arm is added and the
Approve/Deny pair is removed for it; B3 a distinct approval-kind label "AWS sign-in" with its own
`/approvals` title and no blast-radius banner (was: labelled "Mint a scoped credential"); B4 the deadline
goes on the wire and renders through `absoluteTime` (was: a raw RFC3339 UTC stamp on every screen). SHOULDs
adopted: S1 the rail's own link-variant sign-in with a distinct accessible name and the four rail sentences
rewritten; S2 shared-admin banner sentences; S3 hairline tone for `expiring`; S4 Esc/overlay-close cancels the
login run and closes the tab; S5 success toasts + focus to main; S6 "Waiting for your AWS sign-in" on the
board and header; S7 the door note rewritten for focus mode; S8 aria-labels on the two extra doors; S9 plain
nouns in the starting sentences, warning tone for a terminal reason, "Try again" suppressed on a stuck image;
S10 the placeholder tab's text; S11 the blurb and the chooser instruction; S12 `/providers` suppression for
operators; S13 `shared_expired` renders the action alone. NITs applied (dialog title/description, the row
hint, the shared-credential Known gap, the resolved focus-mode/CSP risks). Journey verdicts before the fixes:
(h),(i) delivered; (a),(b),(c),(e),(f),(g) partly; (d) not — every named cause is addressed above.

**Round 2 — model switch recorded.** Both round-2 Fable reviewers (general + UX re-check) were killed by a
Fable session-limit 429 (`claude-fable-5-1`, resets 03:30 America/New_York) before producing a finding; per
this plan's protocol and the owner's standing rule, round 2 re-ran on **Opus 5** with the same briefs.

**Round 2 — UX re-check (Opus 5, blind, 2026-09-18).** Seven BLOCKERS, all adopted: B1 `door.claim()` is now
defined on the context hook (ref-counted ownership; the strip's button renders only at count 0); B2 the
placeholder tab's ruled body moved INTO the constant; B3 the ImagePullBackOff test now asserts Cancel-only
(it had pinned the pre-ruling "Try again"); B4 the contradictory "warning tint for every state" sentence
deleted; B5 `EXPIRED` reworded to be true of a pin-contradicted LIVE session ("no longer works for Claude
Code"), strip and rail; B6 `{when}` = `relativeTime(deadline)` with `absoluteTime` as the title (the helper
renders seconds); B7 the door note no longer names a header control focus mode does not render. All
fourteen SHOULDs adopted: S1 the server action renders only where it carries what the sentence and button
cannot; S2 the rail claims the door (no New Run exception); S3 the "Not now" flag is keyed on the viewer's
subject; S4 "Not now" extended to `shared_expired`; S5 the refusal's destination leads with Getting started;
S6 "paused" for people, "held" for the wire; S7 `EXPIRED_SHORT` when a page owns the door; S8
`waitingReauth(n)` keeps a co-pending count; S9 a short chip register; S10 no "Signed in." verdict on
forgeable evidence; S11 the stuck lead-in no longer says "image"; S12 the dialog description string; S13 the
strip's control is the underlined text button; S14 `expiring` uses the info state tone. NITs applied
(sentence-case "Getting started", the two-prompt chooser, "click or tab", the raw-reason prefix, focus to
Launch from the rail, the skip-link a11y note).

**Round 2 — general (Opus 5, blind, 2026-09-18).** Four BLOCKERS, all adopted: B1 the `ssoEgressHosts`
port-qualified entry is now an explicit fix step (today the function returns one bare host; the walk would
have 401'd silently); B2 the content-scan sidecar is the PROXY's client, not wardynd's, and its URL is a
per-policy DB field unknown at install time — removed from the daemon-proxy bypass and consumer list (five
consumers; the round-1 S6 ruling is corrected); B3 `buildInjector` no longer calls the holding wrapper (no
hold at boot); B4 the sequence diagram and one test now show the snapshot equality check, per-run masking,
`ResolveReauth` + `credential.reauth.resolved` with the I6 filter. All ten SHOULDs adopted: S1 "undismissable"
qualified in the summary row, the test and the CHANGELOG; S2 `StatusReason` on the Go wire type as a
derived-at-read field; S3 both audit rows named; S4 the three window-0 re-cites the F4/F5 insertions shift;
S5 per-run masking is additive to the refresh path's existing `AddGlobal` (kept); S6 `types.ApprovalKinds` +
the missing `approvals.kind` enum-parity entry; S7 the deadline-on-wire fix steps, TS mirror, the two renders
and a redaction test; S8 `copy.ts` (961 lines) measured, all new strings in `model-access-copy.ts`; S9 the
fake's role-credential expiry stamped per call and the SDK cadence measured before the walk budgets K; S10
`isMITMHost`'s boundary comment gains the third source. NITs applied (package-qualified `policy.go`,
`handleGetRun :181`, the hold's ctx derived from the request, the approval-poll residual named, `claims.Sub`
named for I3, "new dispatches only" for the kill switch, the grant TTL, the sentinel-arm wording). Reviewer
verdict before these: "CLEAN after nits" once B1–B4 and S1–S10 are fixed — they are.

## Appendix A — verbatim transcription of `HANDOFF-wardyn-076-sso-ux.md` (7 photos, 252 lines, no gaps)

Photo → line coverage: IMG_0201 1–44 · IMG_0202 41–83 · IMG_0203 78–120 · IMG_0204 118–159 · IMG_0205 157–199 ·
IMG_0206 201–240 (line 200 blank, confirmed on the full-resolution crop) · IMG_0207 230–252 (end of file).
Two table cells on lines 218–219 run past the right edge of the photo; the cut words are marked `[…edge]` and are
recovered from the source files they quote (`awssso_refresh.go`, `docs/ENV.md`) in the plan body.

```
Consolidated feedback on ONE journey: a person getting AWS SSO working and running a Claude Code agent,
on a deployment where the agent roster says `bedrock_sso` + `credential_source: per_user`. From running
0.7.1 through 0.7.5 on a private-endpoint Kubernetes estate with an admin and two colleagues.

**The premise.** Per-user credentials are the direction the product has committed to — 0.7.3 added the
account/role pin, 0.7.4 gave members their own sign-in door, 0.7.5 made the copy honest about whose
credential it is. That design means **N humans each hold a credential that expires**, rather than one
operator credential an admin babysits. Re-authentication stops being an incident and becomes routine
traffic, and the people meeting it do not know what a refresh token is, have not read the roster, and
will not read a release note.

Everything below is one question: **when a person's model access is missing or lapsed, does the product
put them on the path to fixing it, or does it let them find out by failing?** Today, mostly the latter
— and the pieces needed to fix it already exist.

## The state space, and where each state is actually visible

`/setup/status` grades model access per principal into six states (`internal/api/modelaccess.go`), which
is the right vocabulary and covers the real cases. The gap is not the model; it is reach.

`model_access` is read by exactly three surfaces (grep across `ui/src/app`): Settings' connection cards,
`/providers`' agents tab, and the member Getting Started. That is **two admin-only screens and one
landing page**.

| Surface | Reads `model_access`? | Consequence |
|---|---|---|
| member Getting Started | yes | the one place a member is told, and only if they visit it |
| Settings → Model provider | yes | admin-only |
| `/providers` → Agents | yes | admin-only |
| **New Run** | **no** | the place you launch — see below |
| **Run detail** | **no** | where the failure lands, with no action on it |
| **app shell / header** | **no** | nothing global, so nothing unmissable |

A person who clicks **+ New run** in the header — the most obvious button in the product — never
encounters their own credential state at any point before the run fails.

## Finding 1 — New Run's model warning is keyed on a deployment fact, so it is silent for exactly the people who need it

The rail does have a warning, and its wording is good: *"No model provider is connected. This run
launches; its first model call fails."* with a `Connect →` link. But:

```tsx
showModelWarning={isAgent && llmReady === false}   // new-run-screen.tsx:955
```

`llm_ready` is DEPLOYMENT-level — "this deployment has a model provider configured". Under
`credential_source: per_user` it becomes true the moment the ADMIN saves the roster row, before any
member has signed in. So `showModelWarning` is false for every member, and the warning built for this
exact situation never renders.

**This is the same defect 0.7.5 fixed one screen over.** The member Getting Started's "Your model key"
card had precisely this bug (`llm_ready` where `model_access` belonged) and 0.7.5 corrected it. New Run
was not included.

**Fix:** key the rail on `model_access` when a `per_user` row governs the selected agent, and let the
existing warning render with the existing CTA. `llm_ready` remains right for a shared credential; it is
simply the wrong question for a per-person lane.

## Finding 2 — nothing is proactive: every state is discovered by failing

Of the six states, three want a human to act — `not_configured`, `expired_signin`, `expiring` — and none
of them reaches a person unless they happen to open Getting Started.

- **A member who has never signed in** is not blocked, warned, or redirected. They can complete the
  whole New Run form and launch. Nothing at any point says "you need to sign in to AWS first."
- **`expiring`** exists as a state and carries a deadline, and is therefore the product's one chance to
  be ahead of the problem. It is shown on a page nobody visits twice. An expiry a person could have
  pre-empted in 30 seconds instead becomes a failed run later.
- **`shared_expired`** — an admin's dead credential — has correct member copy ("ask them to reconnect
  it") and no path to actually telling them.

**Fix, and the mechanism already exists:** the member-mode banner is a global, undismissable,
app-shell-level strip with an action in it. That is exactly the shape this needs. One banner for an
actionable model-access state, carrying the **"Sign in to AWS"** button, visible on every screen until
resolved, would convert all three states from "discovered by failing" to "impossible to miss." Nothing
new is required — a second caller of a pattern already shipped.

## Finding 3 — the failure names a destination instead of being one

When a run is refused, the sentence is genuinely good — it says what happened, that the sign-in is or
is not still valid, and that Wardyn will not substitute another provider:

```
this run's model access is configured as Amazon Bedrock (captured AWS SSO session), and that session
can no longer be renewed — sign in again under Settings → Model provider. Wardyn does not substitute
a different model provider.
```

But "sign in again under Settings → Model provider" is a navigation instruction handed to someone who
has just been interrupted. The sign-in pane is already mountable inline — the member Getting Started
does it — so the run that failed should offer the sign-in, not describe where to find it.

Same for the run's failure chip and the Runs board row: a run that died for a credential reason should
carry the action, since that is the screen the person is looking at.

## Finding 4 — hold the agent's call mid-run instead of killing the run

A credential can lapse **while a run is working**, and today that is terminal: the model call fails and
an agent mid-task loses its context. For an interactive session a person is watching, that is the
difference between a pause and lost work.

**Wardyn already has the right mechanism, in production, for a structurally identical problem:
`wait_for_review`.** `approvals.go`'s `ResolveWait` "HOLDS the caller until the host's approval reaches
a terminal state (approved/denied) or the hold deadline passes", polling the control plane every
`holdPollInterval`, bounded by `holdTimeout`/`maxHolds`, and **failing closed** — on deadline it returns
pending, which becomes a 403 with the approval left PENDING, so `wait_for_review` degrades to
`deny_with_review` and never to allow. The ceilings exist: `maxHoldTimeout` 600s, `maxHoldsCeiling` 256.

The same shape for credentials:

1. The proxy is about to sign or inject a model call and finds the credential lapsed or unrenewable.
2. Instead of failing, it **holds the request** — the same parks-and-polls shape as `ResolveWait`.
3. The control plane raises a **re-authentication request** against that principal, visibly: the run
   header's "N waiting" surface already exists, and the owner gets the sign-in action itself.
4. The person completes the device-code flow. The capture lands. The hold resolves.
5. The held call proceeds on the new credential. The agent never knew.

**A long timeout is fine for a first version** — this is a human-in-the-loop wait and people need
minutes. The knob shape already exists (`WARDYN_GIT_APPROVAL_TIMEOUT` bounds git-mint approval waits the
same way). On expiry, fail exactly as today; nothing gets weaker.

Three properties worth keeping deliberately, because they are what make the egress version safe: **fail
closed** (never proceed uncredentialed, never fall through to another provider —
`enforceConfiguredLLMMechanism` already guarantees the last part); **bounded** (a per-run cap, as
`maxHolds` does); and **audited** (a row when the hold opens and when it resolves or expires, carrying
the owner, so "whose credential, held how long, resolved by whom" is answerable from the trail).

## Finding 5 — one transient network failure permanently destroys a person's model access, silently

Two dispatch attempts, ninety minutes apart, same principal, same roster row:

```
01:09  harness.credential.refresh  FAILURE
       aws sso create-token: Post "https://oidc.us-east-1.amazonaws.com/token": EOF
       spent: false
02:37  harness.credential.refresh  FAILURE
       aws sso refresh credential is spent: invalid_grant
       spent: true
```

The first failure was ours — the control plane had no route to AWS, because its own egress is a separate
channel from the sandbox proxy's. Fixed on our side (and see Finding 8).

**The second row is the finding.** Once the call could reach AWS, AWS answered `invalid_grant`: the
refresh token was already gone. The `EOF` attempt evidently DID reach AWS, which rotated the token and
sent a reply that never came back. So Wardyn recorded `spent: false` — correctly, it got no response —
while the session was in fact already dead. Nothing said so until the next launch, and the only recovery
was a human re-signing in.

We are **not** filing the `spent` bookkeeping as a bug: a client cannot distinguish "never arrived" from
"arrived, reply lost", and treating an unanswered call as unspent is the right default. The point is
what it proves — **a single dropped packet on the renewal path costs a person their model access, with
no signal until they next try to work.** That is the strongest possible argument for Findings 2 and 4:
if this state is reachable by accident, it must be visible without being hunted for, and survivable
without losing a run.

## Finding 6 — say what a starting run is waiting ON, because the data is already read

Every slow start looks identical: the run sits in `STARTING`, or a pane spins, and nothing says whether
it is pulling an image, waiting to schedule, stuck on a bad reference, or hung. Measured here:

| | `harness.login.started` → `run.interactive` |
|---|---|
| warm image | **2s** |
| cold image, first pull after a version bump | **127s** and **131s**, on two occasions |

A two-minute silence is indistinguishable from a hang, and it fires on EVERY upgrade because the image
tag changes. The first time, it produced a "Could not reach the control plane" error and a wrong
diagnosis; the second time we only stayed calm because we had measured it before.

**Wardyn already reads the answer.** `internal/runner/k8s/lifecycle.go`'s `waitingDetail` walks
`pod.Status.ContainerStatuses` and formats exactly the needed sentence —
`"agent: ImagePullBackOff: …"`, `"agent: ContainerCreating"` — and `statusFromPod` assigns it to
`st.Message` for both waiting phases. The kubelet's reason is in hand at the substrate boundary and
never reaches the console.

**Ask:** surface that message on a starting run — run header, Runs board row, and the login pane's
waiting state. Two refinements worth more than they sound: **tell "slow" from "stuck" by the reason
rather than a timer** (`ContainerCreating` for two minutes is normal after an upgrade;
`ImagePullBackOff` for two seconds is terminal — this is also the honest fix for the login pane's
readiness budget, which still counts elapsed time rather than reading why), and **say when it is a
first pull**, because "this image has not been pulled on this node before; the first start after an
upgrade takes a couple of minutes" is derivable and is the difference between waiting calmly and filing
a bug.

## Finding 7 — the verification URL should open itself, and the pane should close when the server says so

Two smaller things in the sign-in flow itself.

**The tab does not open.** The pane extracts the device-verification URL and calls `window.open` with
this note: *"Best-effort auto-open. A browser may block a popup not tied to a user gesture; the
surfaced link below is the reliable one-click fallback."* It fires from `handleOutput`, an async
PTY-output callback — by definition not a user gesture — so browsers block it essentially always, not
occasionally. The extraction is not the problem: `extractDeviceVerificationUrl` matches both the device
endpoint and the org portal and deliberately prefers the `?user_code=` variant that pre-fills the code.
**Fix:** open the tab on the CLICK that starts the sign-in (a real gesture, never blocked), keep the
handle, and set its `location` when the URL appears. To be fair, 0.7.5 already surfaces the URL as a
proper button in the pane header, so the one-click fallback is good now; only the automatic part is
missing.

**The pane does not close on success.** Observed: the terminal printed `Successfully logged into Start
URL`, the server recorded `harness.credential.captured` seven minutes later, and the dialog still
offered only Cancel. 0.7.5 deliberately stopped trusting the PTY success marker — correct, it is
sandbox-forgeable — and now waits for `/setup/status` to confirm independently. But that leaves a real
window where the login has succeeded, the server has the credential, and the only thing saying
otherwise is the dialog in front of the person. **Fix:** show the waiting-on-server state explicitly
("signed in — confirming with the server"), and make sure the corroboration poll is bounded and
retried, so the pane cannot sit on a sign-in that worked.

## Finding 8 — there is no supported way to put the daemon's own AWS calls behind a corporate proxy

Recorded because it cost us an outage and the next deployment behind a proxy will hit it.

Two files in the repo give opposite instructions:

| Source | Says |
|---|---|
| `awssso_refresh.go` | the renewal uses `http.DefaultTransport` so it *"honours the PROCESS proxy environment … wardynd's […edge] sandbox proxy's, and a deployment behind a corporate proxy needs this hop to follow it"* |
| `docs/ENV.md` | of `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY`: *"**Never set as wardynd runtime env**" — process-wide proxy […edge] discovery, audit webhooks, GitHub App minting and AWS credential chain"* |

Both are accurate, and `WARDYN_HOST_PROXY_B64` — the only proxy-named knob — is **diagnostics only** and
routes nothing. So the renewal needs the proxy and the docs forbid the only mechanism that provides it.
We took the forbidden route and it worked, but only because this deployment has no audit sinks and no
GitHub App, and Entra could be excluded by name; getting `NO_PROXY` wrong once took the control plane
down for nine minutes.

**Ask:** a scoped setting for the daemon's own outbound calls. The operator has ALREADY told Wardyn how
to reach the internet — `SiteConfig.upstream_proxy_url`, which today governs only sandbox egress. Reuse
it for daemon-side AWS SSO calls (or add a dedicated knob) so a corporate-proxy deployment is not asked
to choose between a broken renewal and a documented foot-gun.

## What "seamless" would look like

The through-line across all eight: **the facts exist and the mechanisms exist; they are just not
connected to the person.** `model_access` grades the state; the CTA exists; the banner pattern exists;
`ResolveWait` holds calls; `waitingDetail` explains slowness. Almost nothing here asks for new
machinery — it asks for existing machinery to reach one more surface.

Concretely, the journey we would call seamless:

1. A member who has never signed in **cannot miss it** — a global banner with the sign-in action, and
   New Run says so before they fill in the form rather than after they launch.
2. Sign-in **opens its own tab**, and the pane closes itself the moment the server confirms.
3. An expiring credential is **flagged before it lapses**, on any screen, with one click to fix.
4. A credential that lapses mid-run **pauses the run** rather than killing it, and the person is asked
   to sign in, exactly as they are asked to approve an egress host today.
5. When something is slow, the product **says what it is waiting on**.
6. When something is genuinely broken, the message is **the door**, not directions to it.

Steps 1, 3, 5 and 6 are re-pointing existing signals at existing surfaces. Step 2 is a small change to
where a click is handled. Step 4 is a second caller of `ResolveWait`. That is the whole list.
```
