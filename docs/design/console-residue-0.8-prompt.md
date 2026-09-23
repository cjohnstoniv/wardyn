# Console residue — the batch with no surviving mock

This is the mock round for the 0.8 console-residue batch named by #157: findings from the 0.7.6/0.7.7
readiness reviews that had no design artefact and whose review packages no longer exist anywhere, so
the finding text is only recoverable from `CHANGELOG.md` and commit bodies. Twelve findings were
named as residue; six have no surviving description at all and are struck from the roadmap rather
than guessed at. This document covers the five the changelog still describes, plus the
unsaved-changes guard, which shipped (#217) ahead of the frozen tables that were supposed to precede
it — its state list here is a freeze of the shipped hook, in `model-access-prompt.md`'s sense, not a
proposal.

Citations name **symbols**, never line numbers (`CONSOLE-RULES.md`'s convention).

Static mock: `docs/design/console-residue-0.8-mock/index.html` (open it in a browser). Sections 1-4
are proposed copy — new strings, staged for implementation issues this round consumes. Sections 5-6
are frozen-as-shipped, like `model-access-prompt.md`'s four surfaces: the value below is the shipped
value, byte for byte.

## 1. The audit liveness chip (`components/screens/audit.tsx`)

**Proposed, freezing the shipped behavior.** The `Live · appending` chip next to the audit trail's
event count is not a constant "is polling configured" indicator — it answers "did the LAST read
actually answer". `pollStale` (`audit.tsx#tick`) flips true the moment a background poll tick's
`fetchEvents()` rejects, and the chip's guard reads `status === "ready" && !pollStale"`
(`audit.tsx`, the toolbar row) so a poll that has started failing stops asserting liveness on the
very next tick, rather than riding the initial `"ready"` status forever (`CONSOLE-RULES §10` — a
result verb needs a real result behind it, cited at the flag's declaration).

| State | Chip |
|---|---|
| Initial load in flight (`status === "loading"`) | none |
| Initial load failed (`status === "error"`) | none |
| Loaded, every poll tick since has answered (`status === "ready" && !pollStale`) | `Live · appending` (success, dot, pulse) |
| Loaded, the most recent poll tick rejected (`status === "ready" && pollStale`) | none — the count and rows on screen are the last good read, unlabeled as live |

This sits beside, and is independent of, `GroundTruthChip` (`groundTruth`, the eBPF sensor's own
`unavailable`/`degraded` state) — a dead poll and a dead sensor are different failures and the two
chips never collapse into one.

## 2. Episode-catalog member-chip count (`components/screens/onboarding/episode-card.tsx`)

**Proposed, freezing the shipped behavior.** The finding (F112, changelog: "the declared e2e gate is
RED: episode-catalog.spec.ts hard-codes 2 member-chip rows, and the 0.7 user-drives episode 04d made
it 3") named a spec that asserted a literal count. The shipped assertion derives it instead:
`episode-card.test.tsx` counts `EPISODES.filter((e) => e.path === "multi" && e.audience ===
"member")` and compares the rendered `"For your members"` chip count (`T.FOR_YOUR_MEMBERS`,
`episode-card.tsx#EpisodeList`) against that, so a future episode added with `audience: "member"`
moves the spec's expectation with it. There is no chip on a `single`-path episode or a `multi`-path
episode with any other audience.

## 3. Security-tier probes surface (`components/wardyn/copy.ts#SECURITY_ONLY_REASON`)

**Proposed, freezing the shipped behavior.** `SECURITY_ONLY_REASON` ("Requires the admin or security
admin role.") has carried a `DRAFT (M2 canon pending)` comment since it shipped for X3-F6: a control
gated on `isSecurityOperator` (admin OR security admin) must not tell a refused reader
`OPERATOR_ONLY_REASON` ("Requires the admin role."), which names a narrower door than the one that
actually decided. This freezes it as the one reused string for every SECURITY-tier surface — never
re-worded per caller — currently rendered at:

| Surface | Symbol |
|---|---|
| Live-approvals panel hint + ScopeMenu's Always reason | `components/wardyn/live-approvals.tsx` |
| The Approvals screen's decide-gate chip | `components/screens/approvals.tsx` |
| Run detail's Approvals tab decision gate | `components/screens/run-detail.tsx` |
| Allowed-hosts card's add control | `components/screens/workspace-detail/allowed-hosts-card.tsx` |
| Denied-hosts card's remove control | `components/screens/workspace-detail/denied-hosts-card.tsx` |
| Permissions screen's two write panes | `components/screens/permissions.tsx` |
| Governance screen's profile editor | `components/screens/governance/governance-screen.tsx` |
| Workspace detail's record pane | `components/screens/workspace-detail/record-pane.tsx` |
| `reason-dialog.tsx`'s Always reason | `components/wardyn/reason-dialog.tsx` |

The rule the string encodes: **which reason renders is picked by the gate that fired, never by a
comparison against the reader's own role.** A viewer who is not an operator at all still reads
`OPERATOR_ONLY_REASON` at an operator-tier control beside a security-tier one on the same screen —
the two strings coexist rather than one subsuming the other.

## 4. The drives preview's dropped fields (`docs/design/user-drives-prompt.md` §7.3)

**Proposed.** Already recorded as a known gap in the frozen user-drives canon: `POST
/drives/preview` answers seven fields: the five the `<dl>` renders (`FIELD_HOME`,
`PREVIEW_OBJECT_LABEL` + hint, `COL_SIZE`, `COL_MODE`, `PREVIEW_ENFORCEMENT_LABEL`) and two the
console drops — `home_subject` (which submitted claim the directory name was derived from) and a
server-composed `warning` (`drivePreviewWarning`, `internal/api/user_drives_preview.go`: *"the
directory name keys on the sign-in subject; paste it first"*, raised for a `hash`/`sub` drive
previewed with an address pasted first). Both are already typed in `ui/src/app/lib/api/drives.ts`
and rendered by nothing. This mock stages the two rows the residue batch owes.

| Key | String | Renders when |
|---|---|---|
| `PREVIEW_SUBJECT_LABEL` | Matched on | `home_subject` is non-empty; value is the claim key verbatim (`sub`, `email`, or the role/group name), mono |
| `PREVIEW_WARNING` | {server `warning` text, verbatim} | the endpoint returns a non-empty `warning` string; renders as a `warning`-tone inline note directly under the `<dl>`, same placement as `ALLOC_REPLACED`'s precedent (a note worth reading twice, not a toast) |

Both rows are additive to the existing five — no existing row's key, position or string changes.
`PREVIEW_OBJECT_HINT`'s existing qualifying language (`user-drives-prompt.md` §7.3, the paragraph
starting "Until it does") is unchanged by this: once `PREVIEW_WARNING` renders the caveat inline, a
later round may tighten that hint, but that edit is out of this document's scope.

## 5. The user-drives card's unlabeled third state (`components/screens/setup/user-drives-card.tsx`)

**Proposed, freezing the shipped behavior; the mock adds the missing state.** `user-drives-mock`
State 9 draws the card's `populated` and `empty` bodies only. The shipped component has a third,
undocumented body: `counts` starts `null` and the summary line renders as `""` — no lead sentence
below the title, no meta line under "Manage drives" — until `getDrives()` resolves, and stays that
way permanently if the fetch rejects (`user-drives-card.tsx#UserDrivesCard`: *"a failed read leaves
the summary ABSENT rather than claiming 'No drives yet.' — a confident empty state over an unloaded
snapshot is a false claim"*). This state was a deliberate choice at implementation time with no
frozen row of its own; this document gives it one.

| State | Title | Body | Link label |
|---|---|---|---|
| Loading / read failed (`counts === null`) | `DRIVES.TITLE` | `DRIVES.CARD_LEAD` only — no summary line | `DRIVES.CARD_OPEN` |
| Populated (`counts.drives > 0`) | `DRIVES.TITLE` | `DRIVES.CARD_LEAD` + `DRIVES.CARD_SUMMARY(CARD_DRIVES(n), CARD_ALLOCATIONS(m))` | `DRIVES.CARD_OPEN` |
| Empty (`counts.drives === 0`) | `DRIVES.TITLE` | `DRIVES.CARD_LEAD` + `DRIVES.CARD_EMPTY` | `DRIVES.CARD_OPEN` |

No new string: the loading/failed state renders the existing `CARD_LEAD` alone and omits the summary
line rather than inventing placeholder copy — consistent with the file's own stated reason for
leaving a failed read unlabeled.

## 6. The unsaved-changes guard (`lib/use-unsaved-guard.tsx`) — frozen as shipped

Shipped for #217 ahead of this document. `UnsavedGuardProvider` wraps the shell
(`app-shell.tsx#AppShell`) and holds ONE blocking confirm dialog shared by every registered dirty
form; `useUnsavedGuard(dirty: boolean)` registers a form's dirty flag by a stable per-mount id (a Set,
not one boolean — more than one dirty form can be mounted at once) and arms `window.beforeunload`
while `dirty` is true; `useGuardedNavClick` intercepts an ordinary left-click on a sidebar nav link
(`app-shell.tsx#SidebarNav`) when anything is dirty, and lets a modifier/middle click (new tab) pass
through untouched since the dirty form in the current tab is never touched by it.

`providers-screen.tsx` is the one caller today: it owns the Git/Storage tabs' combined draft and
calls `useUnsavedGuard(changedLines.length > 0)`, `changedLines` from `readableDiff(original, draft)`
— the same diff the screen's own "Copy my changes" 412 recovery reads, so the guard and the conflict
banner never disagree about what counts as dirty. `git-tab.tsx` and `storage-tab.tsx` feed their own
draft state up into that one array; neither calls the hook directly, which is why the guard survives
a Git/Storage tab switch inside the same screen instead of resetting.

| State | Trigger | Surface |
|---|---|---|
| Clean | `changedLines.length === 0` | no dialog; nav links and the tab browser's own close/reload behave normally |
| Dirty, navigating away in-app | a sidebar nav link clicked while dirty | blocking `AlertDialog`: `UNSAVED_GUARD.TITLE` / `.BODY`, `.STAY` (ghost) / `.LEAVE` (destructive) |
| Dirty, closing or reloading the tab | `window.beforeunload` while dirty | the browser's own native "leave site?" prompt (`e.preventDefault(); e.returnValue = ""`) — no console copy renders here, browser-owned surface |
| Dirty then saved | a successful `PUT` resets `original` to `draft` | `changedLines` recomputes to `0`; the guard un-registers on the next render, no dialog for the save that just happened |
| Save failed, still dirty | the `PUT` rejects | `original` is left unchanged, `changedLines` stays non-zero; the guard stays armed exactly as it was before the failed save |

Strings (`components/wardyn/copy/shell.ts#UNSAVED_GUARD`):

| Key | String |
|---|---|
| `TITLE` | Leave without saving? |
| `BODY` | This form has changes that aren't saved. Leaving now discards them. |
| `STAY` | Keep editing |
| `LEAVE` | Discard changes |

## 7. Scope this document does not cover

The six residue findings with no surviving description (run-cockpit side-fetch panes, the attach
terminal's stalled 101, cockpit tile fill, recording-fetch failure copy, the sign-in gate's
three-way probe in `App.tsx`, Settings' unread proxy posture) and the drives allocations truncation
note are struck from the roadmap rather than covered here, per #157's own open question — no
description of the shipped or intended behavior survives in `CHANGELOG.md` or a recoverable commit
body to freeze against. The four `user_drive_unavailable` sentences `health.ts` names (R4-F052) are
already canon in `workspace-providers-prompt.md` §7.6 (U3) and are not re-covered here.
