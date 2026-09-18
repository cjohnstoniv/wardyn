# Wardyn 0.7.6 plan — independent implementation-readiness review

Reviewed 2026-09-18 by Codex with three parallel reviewers (security, lifecycle/integration, and UI/docs), against repository base `dfa89f608fa223b469b0a2435226538f0035d533`.

Plan: `/home/cjohn/.claude/plans/please-see-and-plan-graceful-newell.md`.
Latest reviewed plan SHA-256: `4fd2933a21c9c8870e62123f060a7d15e1830625a54176c71d0a8d088d6b5428` (2,220 lines). The plan changed during review; findings below incorporate the changes through this version. Section names and code symbols are used instead of unstable plan line numbers.

This is a plan/code review, not certification of an implementation. No product changes, merges, pushes, releases, or changes to the main checkout were made for this review.

## Recommendation

The evidence gathering, explicit security invariants, independent lanes, red-first tests, and rebuilt-image provenance are strong. However, I would not start implementation from the plan unchanged: several proposed seams cannot deliver their stated guarantees.

I recognize the owner's instruction to address all eight findings. I am not silently removing F4 from that scope. Nevertheless, Phase B changes credential delivery, adds a persisted approval kind, and introduces a distributed recovery protocol. Treat its scope/default as an explicit security-feature decision, not an ordinary low-risk patch. Keep the other fixes independently deliverable. A default-off flag does not undo the schema/API changes or disable already-dispatched runs. No 0.8 work is proposed here.

## Fix the following contracts before handing out implementation briefs

### 1. F4: successful capture must not strand a pending hold

The capture step stores the new credential, then best-effort resolves pending reauth approvals. The proxy subsequently polls only the approval. If `ListApprovals`/`ResolveReauth` fails, or the daemon crashes between those writes, the credential is valid but the existing request remains PENDING until timeout. Replaying that upload does not repair it: `handleUploadSSOToken` rejects an already-captured `SourceRunID`.

Specify idempotent, recoverable completion using persisted capture provenance, principal/scope checks, and the generation rule. Logging a failed one-shot notification is insufficient. Also cover a pending row created concurrently with the capture's scan.

Required tests: credential stored followed by failed approval update; restart in that gap; retry/reconciliation resolves the original row without another human login; wrong-owner/stale capture still cannot resolve it.

### 2. F4: one mutex is not one bounded workflow

The new request-context propagation is a good correction, but it only bounds the active holder. `injector.resolve` still queues on an uncancellable `reMu`; a failed resolution leaves the entry expired. Each queued caller can then start a fresh full timeout against the SAME approval and increment the workflow counter. The successful 64-caller test does not catch this failure path.

Specify one shared deadline/result per approval workflow, cancellable waiter admission, and behavior for requests arriving after that workflow timed out. Count distinct workflows, not wrapper entries. A canceled leader must not silently renew the budget for its followers.

Required tests: 64 callers with NO capture; queued callers disconnect; leader disconnects; late arrival after timeout; exactly one workflow count and a common terminal result within the advertised bound.

### 3. F4: the proposed audit seam does not enforce I7

I7 requires captured -> resolved -> credential-bearing retry. Inserting resolution immediately after `storeAWSSSOBlob` places it BEFORE the existing `harness.credential.captured` emission in `handleUploadSSOToken`. Furthermore, the existing approval-decision pattern publishes state before its separate audit write; `recordAudit` can fail or spool.

Define when APPROVED becomes observable relative to its durable audit predecessor, and how incomplete transitions recover. Merely adding a dedicated action name does not establish the invariant. If the intended guarantee is weaker, revise the invariant and threat-model claim explicitly rather than leaving code and security prose contradictory.

Required tests: failure/crash at each boundary, audit sink failure, and a concurrent resolver proving that no credential-bearing retry bypasses the promised predecessor.

### 4. F4: terminate authoritative denial and actually deliver the timeout knob

`approvalClient.poll` currently treats EVERY non-200 response as PENDING. After run kill/revocation, internal authentication/liveness middleware may return 401/403 before the CANCELLED row can be read. The plan's accepted 404/transport-error residual does not address this case. Give reauth polling a classified terminal-denial result; preserve transient retries and existing approval semantics deliberately.

Separately, reading `WARDYN_CREDENTIAL_REAUTH_TIMEOUT` with `os.Getenv` in the proxy is not enough. Managed Docker and Kubernetes sidecars do not inherit arbitrary daemon variables. Add it to `runner.ProxySidecarEnvKnobNames` and supported deployment forwarding, with both substrate tests. Otherwise the operator override and shortened live timeout case silently use the default.

Required tests: kill with delayed/failed proxy teardown; authoritative denial ends the hold promptly; transient 503 remains retryable; nondefault timeout appears in generated Docker/Kubernetes proxy environments and changes the live wait.

Also, the proposed holding helper captures a token string across the entire wait. Use the existing current `tokenSource` for EVERY control-plane call, including final injection resolution. Otherwise approval polling can succeed with a renewed run token while the final resolve uses the obsolete token and refuses. Test token rotation during the hold with the old token rejected.

### 5. F4: pending approval is not proof that a request is still paused

The state machine deliberately leaves the approval PENDING after the hold times out and the SDK receives 401. The UI nevertheless continues saying the run is paused and will continue after sign-in; APPROVED triggers “the run is continuing.” Neither claim follows from the row: the request may already have failed, the client disconnected, or the final injection resolve refused scope drift.

Use copy that states only what is known (“AWS sign-in needed”; “Signed in”), unless active-wait/resumption evidence is available. Do not promise automatic continuation merely because an approval changed state. Test timeout followed by late capture and APPROVED followed by failed final resolve.

The updated “off affects new dispatches only” wording is correct. Add an on -> off restart test with both an active and held existing run, and document how an operator handles those existing runs. `claimSingleInstance` excludes another daemon, not old sidecar/browser/CLI versions; cover relevant version combinations explicitly.

### 6. F5: align grading with the actual dispatch refusal boundary

The proposed `spent && !expired -> expiring` rule remains wrong inside the refresh skew. For an otherwise renewable blob, `refreshAWSSSOBlob` refuses a known-spent token as soon as `needsRefresh(now)` becomes true, ten minutes BEFORE nominal expiry.

Example: now 12:00, access expiry 12:05, registration tomorrow, refresh fingerprint marked spent. The proposed UI says this run launches and gives a deadline of 11:55; dispatch refuses immediately. Grade against the same effective serving/refusal boundary, not only `blob.expired(now)`.

Also, `modelAccessDeadline` currently picks the registration deadline when `blob.renewable(now)` is true. A spent parameter added only to state grading will not produce `ExpiresAt - skew` “for free.” Update deadline derivation explicitly. The test still expecting nominal `ExpiresAt` contradicts the design.

Required tests: just before, exactly at, and just after the skew boundary; nominal expiry; spent versus non-spent; valid versus lapsed registration; per-user and shared-member projection. Preserve the existing transient-refresh-with-usable-token behavior.

### 7. F2/F3: the shared-admin dead state is missing from the repair predicate

The proposed predicate makes only `not_configured`, `expired_signin`, and `expiring` actionable, with no operator capability input. But `awsSSOCredentialState(..., perUser=false, ...)` returns `shared_expired` for a missing or dead shared credential, INCLUDING for its admin. `setupModelAccess` uses the credential scope, not the viewer's role. Only a pin contradiction separately forces `expired_signin`.

Consequently, an admin with an ordinary expired/missing shared credential receives “ask your admin” and no repair button in the new global surfaces. The plan's shared-admin table assumes a different server state.

Make shared-dead repair audience-aware: an authorized operator gets the shared-credential sign-in (`startURLManaged=false`); a member gets the admin instruction and no button. Use real server-shaped fixtures for shared admin absent/expired/pin-mismatched, shared member, and per-user cases.

This is distinct from the rejected shared-expiring-member issue: `memberModelAccess` already folds shared expiring to live. Do not resurrect that phantom bug or add a blanket `perUser` CTA gate that breaks valid operator repair.

### 8. F7b: audit visibility cannot be a prerequisite for capture confirmation

`handleUploadSSOToken` stores the credential before best-effort `recordAudit`; a queryable audit event is not guaranteed to appear synchronously. The proposed audit-first watcher can therefore miss a genuinely stored capture forever. Use the audit event as an optimization/wake-up hint, with a bounded authoritative `/setup/status` fallback. Keep strict `captured && source_run_id === thisRun` corroboration.

Required tests: storage succeeds while audit write fails or is delayed; strict status still completes; old/wrong-run credentials and forged markers never complete. A generic read failure is not proof of success or failure.

### 9. F7b: unify watcher state, cleanup, and an actual time bound

Three integration details need explicit treatment:

- The marker sets `savedRef.current=true` before `confirmCapture`, while the watch requires it false. Returning unsuccessful confirmation to `attached` does not reset that latch. A marker-only arrival also need not set `signedIn`. Start/continue one confirmation state machine from either hint, with separate in-flight/completed state and once-only completion.
- Markerless confirmation calls `onDone()` without the existing `confirmCapture` cleanup. The login image deliberately remains alive after upload. Route both confirmed-success paths through once-only login-run termination and tab cleanup, AFTER strict corroboration. Test racing marker/watch confirmations and exactly one cleanup.
- `AutoStopAfterSec=30m` is an IDLE limit, not a maximum login lifetime; attach keepalives can extend it. Also, outages can prevent the watcher from observing terminal state. Thus “run life plus five minutes” is not the claimed hard ceiling. Specify a local absolute watch deadline independently of terminal grace, and test persistent read failure plus continuous activity.

If both PTY hints are lost, the proposed watch never starts. Either cover that path with a conservative server-driven trigger or narrow the marker-independent completion claim and document the residual accurately.

### 10. F6: best-effort diagnostics need their own short database deadline

`OnWaiting` is synchronous and “must not block,” yet performs an ordinary database UPDATE. Discarding an error does not bound the wait. `dispatchRun` uses `context.WithoutCancel`, and `store.execRun` passes its context to the pool: a blocked row/pool can trap this callback beyond the Kubernetes startup timeout.

Give each diagnostic write a short explicit deadline, drop overdue updates, and keep startup progress independent of diagnostic persistence. Do not create an unbounded goroutine per update. Test a setter that blocks until its context is canceled and prove the driver can continue/cancel.

### 11. F6: terminal startup presentation must survive missed intermediate polls

`waitContainerRunning` immediately errors on ImagePullBackOff, after which dispatch marks the run FAILED. Blanking detail/reason outside STARTING means a normal browser poll can miss the reason entirely. The existing FAILED branch displays `failure_hint` without setting the refusal flag, so the planned sentence and Cancel-only behavior are bypassed.

Retain/derive sufficient durable startup-failure evidence to present the same result after FAILED. Test STARTING -> FAILED entirely between browser polls, not only a fixture held in STARTING with ImagePullBackOff. This is necessary for the proposed live E2 assertion to be reliable.

Minor copy correction: Docker `imagePresent=false` proves the image is not cached now, not that this host has never run it. Prefer “Downloading the image” over a historical claim invalidated by image pruning.

### 12. F1/F2: return the refresh promise and test actual launch shapes

`refreshSetupStatus` currently returns void. Passing it unchanged to `usePoll` bypasses that hook's promise-based in-flight protection. Return the request chain and test interval/visibility/manual-refresh overlap, stale completion after authentication changes, and last-known-state behavior on failure.

The rail's “Launch is refused” is also stronger than an `agentRow` predicate establishes. Trace actual composed requests: batch plus a pure-ephemeral primary emits `workspace_id` in `wizard-spec.ts`; `llmMechanismGateApplies` exempts noninteractive workspace-bound requests at create, while persisted workspace linkage can differ and dispatch can refuse later. Do not duplicate an oversimplified server gate in the UI. Add request-shape tests (interactive, batch/ephemeral, genuine scan, exec, different agent/mechanism), or use non-verdict copy stating credential readiness without promising the exact refusal stage.

### 13. F8: preserve and document the real proxy-default behavior

Removing the content-scanner client from the daemon inventory was correct. A remaining contradiction: “unset = direct” and standard `HTTPS_PROXY` being “still refused” do not match the proposed unset test, which leaves `tr.Proxy` untouched. The baseline default transport uses `ProxyFromEnvironment`; we found no runtime refusal of those standard variables.

Choose and test actual precedence. The low-risk option is to leave existing transport behavior unchanged when the Wardyn knob is unset and document standard variables as unsupported/discouraged, not runtime-rejected. Rejecting/clearing them would be a separate compatibility decision. Retain the already-identified custom-kubeconfig residual. Make malformed-URL diagnostics incapable of echoing a credential-bearing raw URL or parser error.

### 14. F3: an AWS door must correspond to the failed run's credential lane

The new `reason: model_credential` covers every `enforceConfiguredLLMMechanism` failure, including Codex/OpenAI. The proposed button checks that reason, viewer ownership, and the viewer's current actionable `model_access`, which grades only Claude Code. A failed Codex/OpenAI run can therefore offer “Sign in to AWS — for this failed run” merely because the same person's unrelated Claude AWS sign-in is missing.

Bind the repair action to the failed run's relevant agent/mechanism, accounting for roster changes since the failure. Owner gating alone is insufficient. Test an unrelated provider failure with simultaneously actionable Claude SSO status, and a historical failure after its agent's configured mechanism changes.

### 15. F2/F7: specify one dialog controller and a complete cancellation path

The added `claim()` ref count resolves one gap, but the hook still lacks an opening/open-state contract for its three callers to control ONE dialog. Reusing a dialog component is not necessarily sharing one instance. Similarly, the pane's existing `onCancel` is a child-to-parent callback; it does not let an external Escape/overlay handler invoke the pane's private cancellation/`runId` cleanup.

Specify ownership of open state and cancellation registration, including the case where the launch POST returns a newly created run AFTER the dialog was dismissed. Test all callers, focus-mode mounts, claim release on navigation/state becoming live, Escape/overlay during pending launch, tab cleanup, focus return, and exactly-once completion.

For accessibility, keep the status live region mounted and update its announcement text. Keying/remounting the region by state conflicts with the plan's own warning about initial live-region content; it does not establish reliable state-change announcements. Include keyboard/screen-reader verification in the mock/browser round.

## UX, test, and execution requirements

- Add a real visual mock/approval round before visual implementation, as required by `AGENTS.md` and `CONSOLE-RULES.md`. Written copy review is not that gate. Cover narrow/wide viewports, banner stacking, member/admin/shared states, focus mode, keyboard focus restoration, and reduced motion. The placeholder tab currently introduces inline hex/ad-hoc sizing; use an approved token-based or plain/native presentation. O-4 also enables Anthropic: its placeholder must not claim AWS.
- The popup browser test should exercise navigation and cancellation, not only creation of `about:blank`. A browser-routed allowlisted verification URL can exercise the real helper without weakening the production allowlist. Cover blocked popup, user-closed tab, cancellation while launch is pending, repeated URL output, and focus restoration.
- Keep the measured SDK hold tolerance >= the actual shipped default, as the latest acceptance criterion says. The plaintext fake walk does not prove the production TLS/MITM path; explicitly include production-shaped TLS/header injection/CA trust/timeout evidence and distinguish simulated checks from live proof.
- Add explicit `test-race-pg` alongside `test-report-pg`, especially for approval/generation recovery and startup-detail updates. Preserve the full-tree `make ci` and full console `scripts/run-ui-e2e.sh` gate; focused UI specs alone are not completion.
- W6 fixes invalidate earlier W4/W5 evidence when they alter the tested behavior/artifacts. State the rerun rule in the runbook: appropriate gates and rebuilt-image walks must certify the resulting candidate SHA. Account explicitly for W7 release-commit/version changes rather than labeling a previous SHA the final release artifact.
- Step 0 must separate immutable 0.7.5 release provenance from the campaign base. If #71 merges first, `origin/main == v0.7.5` is no longer a valid invariant. Record both SHAs and the intentional intervening diff.
- Keep one owner of `ui/e2e/live/**`: F6 supplies assertions/constants and the live-test lane edits case E. Record database/port/cluster ownership before any fresh install; do not reuse another campaign's environment accidentally.
- Reconcile remaining stale prose/tests before generating briefs: blanket Settings suppression versus operator-only suppression; “no new field” versus `deadline`; F5 nominal-expiry expected value versus skew deadline; repeated 423 injection-poll tests versus approval-only polling; the F4 grant-authoring scope must actually include every I3 snapshot field, not merely mention the snapshot in the resolver. The newest dismissal, no-startup-hold, Go `StatusReason`, enum, copy-size, and deadline-redaction corrections should NOT be reported as still missing.

## Coordination with the independent 0.7.x patch campaign

Our main checkout remains untouched at `dfa89f60`. Patches are isolated, separately reviewable commits; none is automatically part of 0.7.6. The local handoff ledger is `/tmp/wardyn-review-0.7x-ledger/local/review-0.7.x/PATCH-LEDGER.md` on branch `review/0.7x-ledger`.

Most relevant overlap:

| Commit | Patch | Coordination |
|---|---|---|
| `46dbcf6d` | Terminal Kubernetes pod handling in Wait/AgentStatus | Overlaps F6's `k8s/lifecycle.go` and OPERATIONS. Integrate first or preserve its semantics explicitly: unknown failed-pod exit remains nil; actual ephemeral termination wins; terminal pods stop both waits. Retain `terminal_pod_test.go`. |
| `fe5c40a9` | Reject oversized brokered uploads instead of scanning truncated content | Independent proxy security fix; consider separately from the injection feature. |
| `6ee6f585` | Record-pane security-tier explanation | Full browser sweep passed for this patch; overlaps console integration, not model-access behavior. |
| `3f1f4c95` | Reduced-motion documentation truth | May overlap CONSOLE-RULES re-citation. |
| `58557df6`, `0a7e23ec` | Live-gate docs and release-version ordering | Reconcile before copying old release tooling/runbooks. |
| `8da1885b`, `b2042f52` | SSH revocation boundaries; audit-spool recovery docs | OPERATIONS/threat-model doc overlap; preserve the corrected operational claims. |

Additional independent docs commits: `ed9a56d4` (UI command recipe), `117c9e04` (OIDC email-verification wording). Review/cherry-pick individually, not an umbrella branch blindly. Support-bundle and overlay-ref work is unfinished and is NOT a ready patch.

Do not reuse old superseded commits `55301f29` (terminal-pod predecessor) or `a470804d` (record-pane predecessor) alongside the replacements above. The combined campaign has not yet completed an uninterrupted green `make ci`; patch-local and partial integration evidence is not a combined-release certification. The terminal-pod live tests establish runner behavior, not full API finalization/reauth cancellation: add eviction -> terminal run -> approval cancellation coverage when combining with F4.

## Already improved in the latest revisions — retain these

Dispatch-time credential-scope equality and subject/host binding; strict source-run corroboration; shared-member expiring-state projection; no hold during proxy startup; request-derived active-hold context; additive per-run masking without deleting existing refresh masking; dedicated reauth audit vocabulary; a distinct reauth UI kind and no Approve/Deny controls; deadline wire/redaction tests; first-run/shared-dead session dismissal keyed by viewer; a new strings module to respect size caps; the corrected daemon transport inventory; and the explicit new-dispatch-only rollback caveat. These are meaningful corrections, not issues to reopen.

Bottom line: resolve the remaining state/recovery contracts and make the tests prove those boundaries, then execute the independently reviewable patch lanes. Keep F4 behind its explicit security/readiness decision and preserve the owner gate before push/tag/release.
