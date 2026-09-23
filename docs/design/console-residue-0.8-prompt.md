# Console residue — the batch with no surviving mock

This is the mock round for the 0.8 console-residue batch named by #157: findings from the 0.7.6/0.7.7
readiness reviews that had no design artefact and whose review packages no longer exist anywhere, so
the finding text is only recoverable from `CHANGELOG.md`, `ROADMAP.md` and commit bodies. Twelve
findings were named as residue; seven have no surviving description at all and are struck from the
roadmap rather than guessed at (see §7 for the id mapping). This document covers the five the changelog and roadmap still
describe, plus the unsaved-changes guard, which shipped (#217) ahead of the frozen tables that were
supposed to precede it.

Citations name **symbols**, never line numbers (`CONSOLE-RULES.md`'s convention).

Static mock: `docs/design/console-residue-0.8-mock/index.html` (open it in a browser). One
classification holds for this file and the mock:

| Section | Status |
|---|---|
| §1 audit liveness chip | frozen as shipped — no new string |
| §2 episode-catalog member-chip count | frozen as shipped — no new string |
| §3 security-tier reason | frozen as shipped — no new string |
| §4 drives preview's dropped fields | **proposed** — the two new keys in this document |
| §5 F049-mock, the drive editor's home-template rule | frozen as shipped (variant (a)); the owner rules Q13 — (a) or the losing (b) |
| §5.1 the drives card's unloaded state | frozen as shipped — extra to the five, no new string |
| §6 unsaved-changes guard | frozen as shipped — `model-access-prompt.md`'s sense, byte for byte |

## 1. The audit liveness chip (`components/screens/audit.tsx`)

**Frozen as shipped.** The `Live · appending` chip next to the audit trail's event count is not a
constant "is polling configured" indicator — it answers "did the LAST read actually answer".
`pollStale` (`audit.tsx#tick`) flips true the moment a background poll tick's `fetchEvents()`
rejects, and back to false on the next tick that answers; the chip's guard reads
`status === "ready" && !pollStale` (`audit.tsx`, the toolbar row), so a poll that has started
failing stops asserting liveness on the very next tick, rather than riding the initial `"ready"`
status forever (`CONSOLE-RULES §10` — a result verb needs a real result behind it, cited at the
flag's declaration). The event count beside it also renders only while `status === "ready"`.

| State | Chip | Body |
|---|---|---|
| Initial load in flight (`status === "loading"`) | none, and no event count | `TableSkeleton` |
| Initial load failed (`status === "error"`) | none, and no event count | `ErrorState` — `STATES.ERROR_TITLE` / `STATES.ERROR_DEFAULT`, `STATES.RETRY` |
| Loaded, and the most recent read answered (`status === "ready" && !pollStale`) | `Live · appending` (success, dot, pulse) | the rows |
| Loaded, the most recent poll tick rejected (`status === "ready" && pollStale`) | none — the count and rows on screen are the last good read, unlabeled as live | the last good rows |

This sits beside, and is independent of, `GroundTruthChip` (`groundTruth`, the eBPF sensor's own
`unavailable`/`degraded` state) — a dead poll and a dead sensor are different failures and the two
chips never collapse into one.

## 2. Episode-catalog member-chip count (`components/screens/onboarding/episode-card.tsx`)

**Frozen as shipped.** The finding (F112, changelog: "the declared e2e gate is RED:
episode-catalog.spec.ts hard-codes 2 member-chip rows, and the 0.7 user-drives episode 04d made it
3") named a spec that asserted a literal count. The shipped assertions derive it instead: both
`ui/e2e/episode-catalog.spec.ts` and `episode-card.test.tsx` count
`EPISODES.filter((e) => e.path === "multi" && e.audience === "member")` and compare the rendered
`"For your members"` chip count (`EPISODES_COPY.FOR_YOUR_MEMBERS`, an `info` chip,
`episode-card.tsx#EpisodeList`) against that, so a future episode added with `audience: "member"`
moves the expectation with it. Today that is three rows under `GROUP_DEPLOYMENT_MULTI`: 04b "A
member's own workspace", 04d "Your drive" and 13 "Your terminal, our cluster". There is no chip on a
`single`-path episode, in the collapsed other-path list, or on a `multi`-path episode with any other
audience.

## 3. Security-tier reason (`components/wardyn/copy.ts#SECURITY_ONLY_REASON`)

**Frozen as shipped.** `SECURITY_ONLY_REASON` ("Requires the admin or security admin role.") has
carried a `DRAFT (M2 canon pending)` comment since it shipped for X3-F6: a control gated on
`isSecurityOperator` (admin OR security admin) must not tell a refused reader
`OPERATOR_ONLY_REASON` ("Requires the admin role."), which names a narrower door than the one that
actually decided. This freezes it as the one reused string for every SECURITY-tier surface — never
re-worded per caller — currently rendered at:

| Surface | Symbol |
|---|---|
| Live-approvals panel hint + ScopeMenu's Always reason | `components/wardyn/live-approvals.tsx` |
| The Approvals screen's decide-gate chip | `components/screens/approvals.tsx` |
| Run detail's Approvals tab decision gate | `components/screens/run-detail.tsx` |
| Allowed-hosts card's per-row remove control (the disabled remove button's `title`) | `components/screens/workspace-detail/allowed-hosts-card.tsx#hostRows` |
| Denied-hosts card's per-row remove control (same) | `components/screens/workspace-detail/denied-hosts-card.tsx` |
| Permissions screen's two write panes | `components/screens/permissions.tsx` |
| Governance screen's profile editor | `components/screens/governance/governance-screen.tsx` |
| Workspace detail's record pane | `components/screens/workspace-detail/record-pane.tsx` |
| `reason-dialog.tsx`'s Always reason | `components/wardyn/reason-dialog.tsx` |

The rule the string encodes: **which reason renders is picked by the gate that fired, never by a
comparison against the reader's own role.** The allowed-hosts card shows both on one list. A member
reads `SECURITY_ONLY_REASON` on every row's remove control. A security admin passes that gate, so on
a row backed by an operator-authored requirement they read `OPERATOR_ONLY_REASON` instead, because
clearing that row is `PUT /workspaces/{id}/requirements`, an operator-only route. The two strings
coexist rather than one subsuming the other.

**Provenance is inferred, not recovered.** The roadmap/changelog item this section answers is
"R4-F070 security-tier probes surface" (`CHANGELOG.md`, the R4 deferred-proposed six). No surviving
text describes R4-F070 beyond that phrase, and none of the nine sites above is a probes surface.
Mapping it to `SECURITY_ONLY_REASON` is this round's reading of "security-tier … surface", not a
recovered description; the owner may instead strike R4-F070 with the seven (see §7).

## 4. The drives preview's dropped fields (`docs/design/user-drives-prompt.md` §7.3)

**Proposed.** Already recorded as a known gap in the frozen user-drives canon (R4-F093): `POST
/drives/preview` answers seven fields: the five the `<dl>` renders (`FIELD_HOME`,
`PREVIEW_OBJECT_LABEL` + `PREVIEW_OBJECT_HINT`, `COL_SIZE`, `COL_MODE`, `PREVIEW_ENFORCEMENT_LABEL`)
and two the console drops:

- `home_subject` — the submitted claim VALUE the directory name was derived from, verbatim.
  `driveHomeSubject` (`internal/api/user_drives_resolve.go`) picks it positionally from the pasted
  claims: the first one, or the last one for an `email_local` drive. It is a subject id or an
  address, never a claim key.
- `warning` — server-composed (`drivePreviewWarning`, `internal/api/user_drives_preview.go`:
  *"the directory name keys on the sign-in subject; paste it first"*), raised for a `hash`/`sub`
  drive previewed with an address pasted first.

Both are already typed in `ui/src/app/lib/api/drives.ts` and rendered by nothing. This mock stages
the two rows the residue batch owes. `user-drives-prompt.md` §7.3 keeps two ideas apart, and so does
the label: "as typed" bounds MATCHING, while `home_subject` names the claim the directory name was
DERIVED from. The label is therefore a derivation word, not a matching one.

| Key | String | Renders when |
|---|---|---|
| `PREVIEW_SUBJECT_LABEL` | Derived from | `home_subject` is non-empty; the value is the submitted claim the name was derived from, verbatim, mono; last row of the `<dl>` |
| `PREVIEW_WARNING` | {server `warning` text, verbatim — its lowercase first letter included} | the endpoint returns a non-empty `warning` string; renders as a `warning`-tone inline note directly under the `<dl>` — inline, not a toast, the way `ALLOC_REPLACED` is (a note worth reading twice) |

Both rows are additive to the existing five — no existing row's key, position or string changes.
`PREVIEW_OBJECT_HINT`'s existing qualifying language (`user-drives-prompt.md` §7.3, the paragraph
starting "Until it does") is unchanged by this: once `PREVIEW_WARNING` renders the caveat inline, a
later round may tighten that hint, but that edit is out of this document's scope.

## 5. F049-mock — the drive editor's home-template rule (`components/screens/drives/drive-editor.tsx`)

**Frozen as shipped (variant (a)); the owner rules Q13.** This is the "drives-mock third state" #157
names. `ROADMAP.md` records it as **F049-mock**: "the drives-mock State 3b … authored as a mock
STATE on the providers mock rather than built, so it is the same sitting's to rule". It is drawn in
`docs/design/workspace-providers-mock/canon.html` as "State 3b (drives mock, F049)", and the
question it poses is `workspace-providers-prompt.md` **Q13**, still unanswered there.

On a Wardyn-managed backend (`isManagedBackend`: `docker_volume`, `k8s_pvc`) the directory-name
segment is concatenated into the object name that `docker volume ls` and `kubectl get pvc` print
(`types.DriveObjectName`), so only `hash` may name one (`types.ManagedBackendRejectsTemplate` on the
server). `host_path`, a share, is the mirror of that case: `hash` cannot name a directory the share
already has (`types.ShareBackendRejectsTemplate`). The mock below (variant (a)) is drawn for a Docker
runner only — the managed-vs-share choice it shows is `docker_volume` vs `host_path`. The two
candidate editor shapes:

| Variant | Directory-name field on a managed backend | Status |
|---|---|---|
| (a) disabled with reason | all three options (`HOME_HASH`, `HOME_SUB`, `HOME_EMAIL_LOCAL`) render, each with its hint; every non-`hash` option is disabled (`drive-editor.tsx#homeDisabled`); `HOME_HINT` and `HOME_RULE` render under the field and say why | **shipped**; recommended (the drives round's Q3 rule: disabled-with-reason over absent) |
| (b) not offered | only `HOME_HASH` renders; `HOME_HINT` and `HOME_RULE` unchanged | the losing variant, drawn for the ruling |

`k8s_pvc_static` is neither of those two: `ShareBackendRejectsTemplate` calls it a Wardyn-named object
(`DriveObjectNamedByWardyn`) exactly like a managed claim, so it does not fall on the `host_path` side
of the mirror rule above. The server's actual rule for it (`ManagedBackendRejectsTemplate` +
`ShareBackendRejectsTemplate` together) is a third shape: `hash` is allowed, and is the default for an
admin who would rather read the preview endpoint than the claim roster; `sub` is allowed too; only
`email_local` is refused, because the static driver's identity check has no per-drive label to keep two
claims from silently sharing one pre-created volume. The editor does not implement this third shape —
`isManagedBackend` only covers `docker_volume`/`k8s_pvc`, so on `k8s_pvc_static` (offered on a k8s
runner, `backendsFor`) the editor's `homeDisabled`/`pickBackend` read it as a share: `hash` is disabled
and moved off, `email_local` is offered. That is the shipped editor blocking the server's recommended,
default template and offering one the server 400s on. This is a known gap in the shipped editor, not a
row neither variant can author; it is tracked as a follow-up (#808) rather than fixed in this document.

Picking a backend moves an incompatible selection with it (`drive-editor.tsx#pickBackend`: a managed
backend selects `hash`, a share backend moves off it). On `docker_volume`/`k8s_pvc`/`host_path` this
means neither variant can author the row the server refuses; on `k8s_pvc_static`, per the gap above, it
can. The API path, which `wardyn drive apply` and an older row meet, still gets the server's 400 under
`SAVE_REFUSED_TITLE`. No new string either way.

### 5.1 Extra to the five: the drives card's unloaded state (`components/screens/setup/user-drives-card.tsx`)

**Frozen as shipped; not one of the five findings.** `user-drives-mock` State 9 and
`user-drives-prompt.md` §7.5 draw the card's populated and empty bodies. The shipped component has a
third: `counts` starts `null`, and the summary meta line inside the `CARD_OPEN` button is left out
until `getDrives()` resolves. It stays out for good if the fetch rejects
(`user-drives-card.tsx#UserDrivesCard`: *"a failed read leaves the summary ABSENT rather than
claiming 'No drives yet.' — a confident empty state over an unloaded snapshot is a false claim"*).
`CARD_LEAD` renders in every state.

| State | Title | Lead | Button label | Button meta line |
|---|---|---|---|---|
| Loading / read failed (`counts === null`) | `DRIVES.TITLE` | `DRIVES.CARD_LEAD` | `DRIVES.CARD_OPEN` | none |
| Populated (`counts.drives > 0`) | `DRIVES.TITLE` | `DRIVES.CARD_LEAD` | `DRIVES.CARD_OPEN` | `DRIVES.CARD_SUMMARY(CARD_DRIVES(n), CARD_ALLOCATIONS(m))` |
| Empty (`counts.drives === 0`) | `DRIVES.TITLE` | `DRIVES.CARD_LEAD` | `DRIVES.CARD_OPEN` | `DRIVES.CARD_EMPTY` |

## 6. The unsaved-changes guard (`lib/use-unsaved-guard.tsx`) — frozen as shipped

Shipped for #217 ahead of this document. `UnsavedGuardProvider` wraps the shell
(`app-shell.tsx#AppShell`) and holds ONE blocking confirm dialog shared by every registered dirty
form; `useUnsavedGuard(dirty: boolean)` registers a form's dirty flag by a stable per-mount id (a Set,
not one boolean — more than one dirty form can be mounted at once) and arms `window.beforeunload`
while `dirty` is true; `useGuardedNavClick` intercepts an ordinary left-click on a sidebar nav link
(`app-shell.tsx#SidebarNav`) when anything is dirty, and lets a modifier/middle click (new tab) pass
through untouched since the dirty form in the current tab is never touched by it.

Two callers today, each guarding a separate document:

| Caller | Draft it guards | Dirty flag |
|---|---|---|
| `providers/providers-screen.tsx` | the Git and Storage tabs' combined `WorkspaceProviders` draft | `useUnsavedGuard(changedLines.length > 0)`, `changedLines` from `readableDiff(original, draft)` |
| `providers/agents-tab.tsx` | the Agents tab's own `AgentProviders` draft (`SiteConfig.agent_providers`, its own GET/PUT) | the same pair over its own `original` and `draft` |

In each, the diff the guard reads is the one the "Copy my changes" 412 recovery reads, so the guard
and the conflict banner never disagree about what counts as dirty. `git-tab.tsx` and
`storage-tab.tsx` edit `providers-screen.tsx`'s draft through props and never call the hook
themselves, which is why the guard survives a Git/Storage tab switch inside the same screen instead
of resetting.

| State | Trigger | Surface |
|---|---|---|
| Clean | `changedLines.length === 0` | no dialog; nav links and the browser tab's own close/reload behave normally |
| Dirty, navigating away in-app | a sidebar nav link clicked while dirty | blocking `AlertDialog`: `UNSAVED_GUARD.TITLE` / `.BODY`, `.STAY` (ghost) / `.LEAVE` (destructive) |
| Dirty, closing or reloading the tab | `window.beforeunload` while dirty | the browser's own native "leave site?" prompt (`e.preventDefault(); e.returnValue = ""`) — no console copy renders here, browser-owned surface |
| Dirty then saved | a successful `PUT`; its response becomes both `draft` and `original` | `changedLines` recomputes to `0`; the guard un-registers on the next render, no dialog for the save that just happened; `toast.success(PROVIDERS.SAVED_TOAST)`, or, when the save narrowed what the org admits (`result.sourcesNoLongerAdmitted > 0`), `toast.warning(PROVIDERS.SAVED_TOAST, { description: PROVIDERS.SAVED_NARROWED(n) })` (`providers-screen.tsx`) |
| Save failed, still dirty | the `PUT` rejects: a 412 raises the saved-elsewhere banner, a 400 renders `PROVIDERS.SAVE_REFUSED_TITLE` over the server's text, anything else toasts `PROVIDERS.SAVE_ERROR` | `original` is left unchanged, `changedLines` stays non-zero; the guard stays armed exactly as it was before the failed save |

Strings (`components/wardyn/copy/shell.ts#UNSAVED_GUARD`):

| Key | String |
|---|---|
| `TITLE` | Leave without saving? |
| `BODY` | This form has changes that aren't saved. Leaving now discards them. |
| `STAY` | Keep editing |
| `LEAVE` | Discard changes |

## 7. Scope this document does not cover

`ROADMAP.md`'s R3/R4 residue batch (the "console copy/state items" line, plus F049-mock) names twelve
ids. This is the mapping that `PLAN.md`:425's release-reconciliation step needs to strike the covered
ones against `ROADMAP.md`, so no id in that line is left unexplained:

| Id | Disposition |
|---|---|
| F132-followup | covered — §1, the audit liveness chip |
| F112-nit | covered — §2, the episode-catalog member-chip count |
| F070 | covered — §3, the security-tier reason (provenance inferred, see §3) |
| F093 | covered — §4, the drives preview's dropped fields |
| F049-mock | covered — §5, the drive editor's home-template rule |
| F141-panes | struck — no surviving description |
| F143-control | struck — no surviving description |
| F142-copy | struck — no surviving description |
| F004-followup | struck — no surviving description |
| F027-a | struck — no surviving description |
| F069-a | struck — no surviving description |
| F051-a/F092-a | struck — no surviving description |

Seven ids are struck, not six. The base findings each `-followup`/`-panes`/`-control`/`-copy` id
extends shipped in commit `71738a7d` (R4-F141: a side fetch that fails no longer claims the control
plane is down; R4-F142: the tile fill rule; R4-F143: a stalled 101 becomes a failed attempt; R4-F004: a
failed recording fetch is reported as a failure) — that base behavior is not in question. What has no
surviving description in `CHANGELOG.md` or a recoverable commit body is the *follow-up delta* each of
the seven struck ids named beyond that base fix (plus the sign-in gate's three-way probe in `App.tsx`,
Settings' unread proxy posture, and the drives allocations truncation note, none of which map to a
roadmap id in the batch above), which is why they are struck rather than guessed at here.

This document does not itself edit `ROADMAP.md`: #170, the reconcile issue, is already closed, so the
strike above is applied at release reconciliation (PLAN.md:425) reading this table, not by this commit.
The four `user_drive_unavailable` sentences `ui/src/app/lib/api/health.ts` names (R4-F052) are already
frozen in `user-drives-prompt.md` §7.6 (`NR_UNAVAILABLE` / `NR_GOVERNANCE_UNAVAILABLE`, "R1-F139 ==
R4-F052") and are not re-covered here.
