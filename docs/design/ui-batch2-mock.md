<!--
Copyright 2025 The Wardyn Authors
SPDX-License-Identifier: Apache-2.0
-->

# UI batch 2 — mock round (D9, D33, D7, D8, M3)

Mock round for five items whose *backend* landed on `feat/v0.6f-ui`'s parent history
(`75e1c300` D7+D9, `dc50ca1c` D8, `0027f514` M3's M1–M4 — ownership + member-safe mounts).
Only M3's UI half (M5, per `docs/design/member-role-desktop.md`) is unbuilt. No `ui/src`
code in this stage — owner law: the
mock is UI source of truth, and the canon strings below become app strings verbatim when
implementation lands (same shape as `permissions-copy.ts` following
`permissioning-prompt.md`). Design-verified before implementation begins.

Read against: `local/enterprise-poc-review/REGISTER.md` rows D7/D8/D9/D33;
`docs/design/member-role-desktop.md` §DECISIONS + the M3 row of its M1–M5 table; the
live `ui/src` surfaces cited under each item (paths + line numbers are current as of this
worktree's base).

**What's already true on the wire**, so this doc doesn't re-litigate it:

- **D9** — `AgentRun.FailureHint` (`internal/types/types.go:197`, migration `0044`) is
  stamped server-side on every pre-agent-start failure arm. `ui/src/app/lib/types.ts`'s
  `AgentRun` does not carry it yet, and nothing renders it. That gap is this doc's scope.
- **D7** — `WARDYN_ALLOW_AGENT_TELEMETRY` (`internal/api/runs_dispatch_mounts.go:186-187`)
  suppresses the CLI's own telemetry by default; unset is the safe default and needs no
  UI. The gap is the **opt-in** case (an operator sets it) and any **historical** run from
  before the switch: the Datadog host still shows up as a plain unlisted-host approval
  with nothing distinguishing it from a real off-policy request.
- **D8** — `first_use_hold_seconds` / `max_holds` (`internal/types/policy.go`, `POLICIES.md`
  §`first_use_approval` modes) are configurable per saved policy; 0/absent keeps the 30s/16
  defaults. The gap is copy: `wait_for_review`'s body still reads open-ended.
- **D33** — no backend gap. Confined's card body just says something the default rule
  (`deny_with_review`) doesn't do.
- **M3** — the backend is merged (`0027f514`): `WARDYN_MEMBER_MODE`/`_WORKSPACE_ROOTS`/`_MAP`
  config (`cmd/wardynd/boot_flags.go`), `owned_by` ownership stamping on `POST /workspaces`
  (migration `0048`, `internal/api/workspaces.go:379-402`), and `ValidateMemberMountSource`
  bind-time enforcement (`internal/runner/member_mount.go`, threaded through
  `internal/api/runs_dispatch.go`). Only the UI (M5) is unbuilt. This is a forward mock for
  how the merged `WARDYN_MEMBER_WORKSPACE_ROOTS`/`_MAP` constraint (member-role-desktop.md
  §c, §DECISIONS O1/O3) will surface in the one screen that authors a member `local_dir`
  source: `AddWorkspaceDialog`.

---

## D9 — FAILED badge hint render

**Placement.** `ui/src/app/components/screens/run-detail-summary-header.tsx`, immediately
after the existing exit-code chip (lines 146–150), inside the same non-wrapping 52px row.
Today that chip is the *only* place a FAILED run says why, and it renders nothing when
`exitCode` is `undefined` — exactly the pre-agent-start failure class D9 exists for (the
agent never ran, so there is no exit code at all). The hint chip is independent of the
exit chip: a run can show one, the other, both, or neither.

Wire addition this render depends on: `AgentRun.failure_hint?: string` on
`ui/src/app/lib/types.ts`'s `AgentRun` (mirrors the Go field 1:1, same optional/empty-string
shape `record-pane.tsx`'s `rr.failure_hint` already uses).

No role gate. A viewer only reaches this header at all once they can see the run
(operator, or the run's owner) — `member` and `operator` render identically. Only the run
being FAILED-with-a-hint changes anything.

### ASCII mock

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│ Runs │ 🤖 fix the flaky retry test                    payments-svc  · a1b2c3   │
│      ⛔ Failed  ⚠ workspace mount unavailable  [CC2] [▮▮░]  ⏱ 0m 4s      ⛔ Kill │
└─────────────────────────────────────────────────────────────────────────────────┘
                     ^state           ^NEW hint chip (no exit chip — agent never ran)

┌─────────────────────────────────────────────────────────────────────────────────┐
│ Runs │ 🤖 refactor the auth middleware                 payments-svc  · d4e5f6   │
│      ⛔ Failed  exit 1                          [CC2] [▮▮░]  ⏱ 3m 12s    ⛔ Kill │
└─────────────────────────────────────────────────────────────────────────────────┘
                     ^ exit chip only — agent ran and exited nonzero; no failure_hint
                       stamped for this arm (unchanged from today)

┌─────────────────────────────────────────────────────────────────────────────────┐
│ Runs │ 🤖 add the export button                        payments-svc  · 77aa99   │
│      ✓ Completed                                [CC2] [▮▮░]  ⏱ 1m 40s    ⛔ Kill │
└─────────────────────────────────────────────────────────────────────────────────┘
                     ^ neither chip — terminal-but-not-FAILED never renders either
```

Chip shape: `Chip tone="danger" mono={false}` (matches `exitCode !== 0`'s existing tone),
`max-w-[280px] truncate`, full text on `title` — same truncate-with-title convention the
repo/workspace-path spans in this same row already use (lines 113–123). A leading
`ShieldAlert`-class icon is redundant with the state badge's own icon (`CircleX` on
`RunStateBadge`, primitives.tsx:154) — the hint chip carries no icon, text only, so it
reads as an annotation on the state rather than a second alarm.

### Canon strings — `run-detail-summary-header.tsx` (inline, no copy.ts table needed: one
render site, one string, the value itself is server-authored free text)

| id | string | notes |
|---|---|---|
| `d9:hint-chip-title-prefix` | *(none — bare `run.failure_hint` text, no prefix)* | The state badge already says "Failed"; prefixing the chip with "Reason:" repeats what the adjacency already carries. Bare server text, same convention as the exit chip showing bare `exit {n}`. |

---

## D33 — Confined copy matches its default rule

**Placement.** Two sites, same string family, both already reading off
`ui/src/app/components/screens/new-run/wizard-types.ts`'s `UNLISTED_RULES` (the shared
source of truth `network-dialog.tsx:59-75` defines and `new-run-screen.tsx:722` re-renders
verbatim) — except the Confined card's OWN body at `new-run-screen.tsx:592-593`, which is
hand-written and never reads `UNLISTED_RULES` at all. That divergence is the bug: the
Confined card promises one behavior, the Unlisted-hosts card three sections below it (same
screen, same scroll) states the true one, and the wizard's own default
(`wizard-types.ts:307`, `firstUseApproval: "deny_with_review"`) matches the *second*
description, not the first.

Fix is copy-only, matching the register's second fix option (no default-value change —
that is a behavior change, out of a mock round's remit): reword the Confined card body to
state what `deny_with_review` actually does, in the same voice `UNLISTED_RULES.deny_with_review.body`
already uses.

### ASCII mock — Confinement section, `new-run-screen.tsx` (before → after)

```
BEFORE                                              AFTER
┌ ○ Confined ─────────────────────────┐             ┌ ○ Confined ─────────────────────────┐
│ Default-deny. New hosts are held    │             │ Default-deny. A new host is refused │
│ at the door for your approval.      │      →      │ and raised for your review — approve │
│                                      │             │ it once and a retry gets through.    │
└──────────────────────────────────────┘             └──────────────────────────────────────┘
```

No change to the Unlisted-hosts card or the NetworkDialog rule cards further down the
page — they already say this correctly; the Confined card now agrees with them instead of
contradicting them.

### Canon strings — `new-run-screen.tsx`

| id | current string (line 593) | new string |
|---|---|---|
| `d33:confined-body` | `Default-deny. New hosts are held at the door for your approval.` | `Default-deny. A new host is refused and raised for your review — approve it once and a retry gets through.` |

### States covered

- Member and operator see the identical card — Confinement mode choice is not role-gated.
- No empty/error state applies (static card copy).

---

## D7 — known-telemetry tag on an approval row

**Placement.** `ui/src/app/components/wardyn/live-approvals.tsx`'s per-row render (the
`pending.map` block, lines 253–313) and its `rowLabel()` helper (lines 100–106) — the one
place an `egress_domain` approval's host is rendered as a bare string today. The 5-second
version people actually read in a live cockpit: a first-time pilot with
`WARDYN_ALLOW_AGENT_TELEMETRY` set (or replaying an old run from before the default
suppressed it) sees `http-intake.logs.us5.datadoghq.com` as an unexplained pending host,
indistinguishable from something the agent chose to reach mid-task.

A tag, not a different flow: this stays a normal `egress_domain` approval, decided through
the exact same Approve/Deny/scope controls every other row has — D7's fix sketch's second
option was explicitly "a curated chip with copy," not a special-cased lane.

Recognition source: a small closed list of known agent-CLI telemetry hosts (today: just
`http-intake.logs.us5.datadoghq.com`, the one `DATA-FLOW.md:27` and `DEMO-SCRIPT.md:664`
already name) — a client-side constant mirroring `HOST_GROUPS`'s shape
(`network-dialog.tsx:39-57`), not a server round-trip. A row matches by exact host or
`*.`-suffix, same matching the allow-list already uses.

### ASCII mock — LiveApprovals strip, one held row

```
┌ Sandbox is waiting — approve to let it through ──────────────────────────────┐
│ ⏱ http-intake.logs.us5.datadoghq.com  [Agent telemetry]  waiting             │
│                                          [Approve ▾] [Deny ▾]                 │
│ ⚠ api.internal.acme.com                                    waiting            │
│                                          [Approve ▾] [Deny ▾]                 │
└─────────────────────────────────────────────────────────────────────────────┘
                                          ^ NEW tag, only on a recognized host
```

Tag shape: `Chip tone="neutral" className="h-5 px-1.5"` — deliberately the quietest tone
on the row (neutral, not warning/info) so it reads as identification, not as a second
severity signal competing with the row's own held/pending state. `title` carries the
one-line explanation (below) so a hover gives the "why is this here" answer without
spending row width on it.

### Canon strings — new block in `ui/src/app/components/wardyn/copy.ts` (beside
`RUN_COCKPIT`, since it's cockpit-adjacent copy, not a screen of its own)

| id | string |
|---|---|
| `d7:telemetry-tag-label` | `Agent telemetry` |
| `d7:telemetry-tag-title` | `The agent CLI's usual diagnostics endpoint. Approve or deny it like any other host.` |

### States covered

- Operator: full row, tag, both action buttons enabled.
- Member (non-owner-viewable rows never reach this strip at all — `LiveApprovals` is
  mounted per-run and the run itself is already owner-or-admin-gated): tag renders
  identically; Approve/Deny stay `disabled={!operator}` exactly as every other row's
  buttons already are (line 279) — the tag is informational only, it grants no new power.
- A held (`wait_for_review`) telemetry row and a passive (`deny_with_review`) telemetry
  row both get the tag — recognition is host-based, independent of the approval mode.
- No known-telemetry host present: nothing renders differently (today's behavior,
  unchanged).

---

## D8 — hold-window copy stops promising an open-ended wait

**Placement.** `ui/src/app/components/screens/new-run/network-dialog.tsx`'s
`UNLISTED_RULES` table (lines 59–75) — the single source both the dialog's rule cards
(lines 301–327) and `new-run-screen.tsx`'s Unlisted-hosts summary (line 722) render from,
so one string edit fixes both surfaces at once. Today's `wait_for_review` body: "The
connection waits, live, until you approve or deny it. Nothing is refused behind your
back." — true of the *approval row* (PENDING for up to 24h,
`live-approvals.tsx:70`'s `approvalExpiryAfter` comment) but false of the *held
connection*, which fails closed at `first_use_hold_seconds` (default 30s,
`live-approvals.tsx:76`'s `HOLD_TIMEOUT_MS`, now server-configurable per
`POLICIES.md`'s `first_use_approval` table). The UI already knows the bound —
`isHeld()` (`live-approvals.tsx:90-95`) computes off the same 30s constant — it just
doesn't say it in the promise text a run author reads before launch.

Kept out of scope for a copy-only mock: exposing `first_use_hold_seconds`/`max_holds` as
wizard fields. They live on a saved `RunPolicySpec`, edited (today) as policy JSON, not
per-run in this wizard; a field for them is a separate, larger design question this batch
doesn't own.

### ASCII mock — "When a run reaches a host that isn't on the list" card, `wait_for_review` row

```
BEFORE                                                    AFTER
┌ ● Hold it for approval ───────────────────────┐         ┌ ● Hold it for approval ───────────────────────┐
│ The connection waits, live, until you approve  │         │ The connection waits, live, for the standard   │
│ or deny it. Nothing is refused behind your      │    →    │ 30-second window. Decide in time and it goes   │
│ back.                                           │         │ through; miss it and it's refused — the        │
│                                                  │         │ approval itself stays open for you to decide.  │
└──────────────────────────────────────────────────┘         └──────────────────────────────────────────────────┘
```

The `30 seconds` figure is the shipped *default*, hard-coded here the same way
`HOLD_TIMEOUT_MS` already is client-side — worded as "the standard 30-second window"
rather than a flat promise, because it isn't universally true: a saved `RunPolicySpec`
(picked in the wizard's rail — `new-run-screen.tsx:736-745` renders under
`confinement === "saved"` same as any other selection) can carry its own
`first_use_hold_seconds` (shipped in `dc50ca1c`), and this card still renders off
`state.firstUseApproval` regardless of which confinement mode is selected
(`new-run-screen.tsx:717-722`, patched from the loaded spec at line 158). A saved policy
authored with a non-default hold makes the flat "30 seconds" wrong on that render path;
this mock accepts that display gap rather than closing it (wiring the card to the
selected policy's real `first_use_hold_seconds` is a separate, larger change — same
"kept out of scope" boundary as the wizard-field question above) and picks wording that
reads as the common case, not a per-run guarantee.

### Canon strings — `network-dialog.tsx` (`UNLISTED_RULES`)

| id | current string (lines 63) | new string |
|---|---|---|
| `d8:wait-for-review-body` | `The connection waits, live, until you approve or deny it. Nothing is refused behind your back.` | `The connection waits, live, for the standard 30-second window. Decide in time and it goes through; miss it and it's refused — the approval itself stays open for you to decide.` |

No change to `deny_with_review` or `always_deny` bodies (accurate already) or to
`live-approvals.tsx`'s own `waiting` label / `RUN_COCKPIT.waitingHeld` (`"N waiting ·
sandbox held"`, copy.ts:384) — that string is a live fact about right now, not a
forward-looking promise, and stays true either way.

### States covered

- Both mount sites of `UNLISTED_RULES` (the standalone `NetworkDialog` and
  `new-run-screen.tsx`'s inline Unlisted-hosts card) — one string, both surfaces, can't
  drift apart (same guarantee the register's D33 finding says the codebase currently
  lacks between two DIFFERENT strings; this keeps the ONE string it already shares).
- Member vs operator: identical — the New Run wizard's Network section isn't role-gated.

---

## M3 — member local_dir onboarding, root-constrained

UI mock for shipped backend: `0027f514` (M1–M4 of `member-role-desktop.md`) is merged —
`POST /workspaces` is already member-allowed and stamps `owned_by` (`internal/api/routes.go:254`,
`internal/api/workspaces.go:379-402`), `ValidateMemberMountSource` already enforces the
root allowlist and credential-dotfile deny at bind time (`internal/runner/member_mount.go`),
and `WARDYN_MEMBER_WORKSPACE_ROOTS`/`_MAP` are already read at boot (`cmd/wardynd/boot_flags.go`).
Only M5 — the UI — is unbuilt. This mocks the ONE screen `member-role-desktop.md`'s M5 row
names for it — `AddWorkspaceDialog` — under the root/dotfile constraints the merged backend
already enforces (§DECISIONS O1/O3).

**Placement.**

1. `ui/src/app/components/screens/workspaces.tsx` has three sites gating today on
   `!operator` that all need to lift together, because `POST /workspaces` is no longer
   operator-only: the header's disabled "Add workspace" button (line 118) and its adjacent
   `OPERATOR_ONLY_REASON` chip (line 117), and the zero-workspaces `EmptyState`'s disabled
   "Add your first workspace" button (line 161) and its `OPERATOR_ONLY_REASON`-appended
   description (line 158) — a brand-new member's most likely first view. All four collapse
   to the operator behavior that already exists at each site (button enabled, no chip, plain
   description) once `!operator` stops gating workspace creation; no new member-specific
   copy is needed for the empty state, since it becomes the operator's existing string.
   Separately, the `PageHeader` description itself (line 114) becomes role-aware, mirroring
   the pattern `runs.tsx:268-270` uses for "Your runs · N" — except the shipped
   `handleListWorkspaces` (`internal/api/workspaces.go:268-297`) returns a member's own
   owned rows **union every operator-owned row**, not a grant-filtered subset: the
   `capWorkspace` grant gates only *launching* a run against a workspace, never the list
   (the handler's own comment says so explicitly, to avoid a per-row capability check on
   the console's hot path). So a header reading "Your workspaces · N" would overclaim
   exclusivity for rows that are actually shared with every other member. This mock drops
   "Your" instead: `` (n: number) => `Workspaces · ${n}` `` — still member-scoped (never
   another member's owned rows, per the union above), just not personal-possessive about
   operator-owned rows that aren't.
2. `ui/src/app/components/screens/add-workspace-dialog.tsx` — the `kind === "local_dir"`
   branch (lines 242–252) grows a root-constraint hint and a conditional writable control
   for a member session; the `repo`/`ephemeral` branches and the whole Advanced disclosure
   (image choice, mount path) are unchanged — M3's additive checks are `local_dir`-only.

**New wire this mock assumes** (implementation adds it, mock doesn't build it): a
`memberLocalDirRoot: string | null` field on `ShellMeta`/`GET /api/v1/me`
(`app-shell.tsx:55-72`'s `ShellMeta` interface) — `null` when no root applies to this
signed-in member (§DECISIONS O1: the per-member map has no entry AND the shared list is
empty), otherwise a short human-readable label of the constraint (the operator's own
prose, e.g. `"under /home/agent-projects"` — not a dump of every configured prefix). This
is presentational only; the actual root list is never sent to the browser as a value to
trust — enforcement is `ValidateMemberMountSource` at bind time
(`member-role-desktop.md` §c), same non-authoritative-hint relationship
`OPERATOR_ONLY_REASON` already has to server-side `requireOperator`.

### ASCII mock — Workspaces list header (member vs operator)

```
OPERATOR                                          MEMBER
┌ Workspaces ──────────────────────────┐          ┌ Workspaces ──────────────────────────┐
│ A repo or directory a run can attach.│          │ Workspaces · 3                        │
│ Runs can only attach what's listed   │          │                       [+ Add workspace]│
│ here.               [+ Add workspace]│          └────────────────────────────────────────┘
└────────────────────────────────────────┘
```

### ASCII mock — AddWorkspaceDialog, `local_dir` selected, member session

```
Root configured for this member:
┌ Add workspace ─────────────────────────────────────────────┐
│ Source: ( Repository ) [ Local directory ] ( Empty )        │
│                                                               │
│ Path on this host                                            │
│ ┌───────────────────────────────────────────────────────┐   │
│ │ /home/agent-projects/payments-svc                      │   │
│ └───────────────────────────────────────────────────────┘   │
│ Mounted from this machine into the sandbox. Must be under    │
│ /home/agent-projects — your admin set this boundary.         │
│                                                               │
│ ▸ Advanced                                                    │
│   (writable checkbox is OMITTED — no                          │
│    WARDYN_MEMBER_WRITABLE_ROOTS configured; §DECISIONS O3      │
│    default is no writable member mounts at all)               │
│                                                               │
│                                        [Cancel] [Add workspace]│
└───────────────────────────────────────────────────────────────┘

No root configured for this member (memberLocalDirRoot === null):
┌ Add workspace ─────────────────────────────────────────────┐
│ Source: ( Repository ) [ Local directory · unavailable ]    │
│         ( Empty )                                            │
│                                                               │
│ Local directories aren't set up for your account. Ask your   │
│ admin to configure a projects root, or use a repository.     │
│                                                               │
│                                        [Cancel] [Add workspace]│
└───────────────────────────────────────────────────────────────┘
```

The "unavailable" `local_dir` `OptionCard` stays visible-but-disabled rather than removed
— the k8s-runner precedent (`add-workspace-dialog.tsx:117-118`) hides the option outright
because it is *structurally* impossible there; a missing member root is an *admin
configuration* gap the member should be told about, not a control that silently vanishes
and leaves them wondering why Repository is the only choice. Selecting it does nothing
destructive — the Field beneath it explains, `canSubmit` stays gated on a non-empty path,
and the server's `WARDYN_MEMBER_WORKSPACE_ROOTS`-empty fail-closed (`member-role-desktop.md`
§c, "empty list ⇒ member local_dir mounts are unavailable") is what actually stops the
request if a stale client ever raced past this hint.

**Server-refused source (dotfile / outside-root) on submit** — same non-field-level toast
pattern `submit()` already uses for every other failure (line 172-174), no new UI
component: `toast.error("Failed to add workspace", { description: getErrorMessage(e) })`
renders the server's `ValidateMemberMountSource` message
(`member-role-desktop.md` §c) verbatim, same as a repo-clone failure would.

### Canon strings — new block in `ui/src/app/lib/permissions-copy.ts` (member-facing
onboarding copy belongs beside `DENIED`, the file's existing home for inline
member-facing "why not" moments)

| id | string |
|---|---|
| `m3:root-hint` | `(root, string interpolated)` → `` Mounted from this machine into the sandbox. Must be under ${root} — your admin set this boundary. `` |
| `m3:local-dir-unavailable-option` | `Local directory · unavailable` |
| `m3:local-dir-unavailable-body` | `Local directories aren't set up for your account. Ask your admin to configure a projects root, or use a repository.` |
| `m3:workspaces-header-member` | `` (n: number) => `Workspaces · ${n}` `` (deliberately *not* `runs.tsx`'s `` `Your runs · ${n}` `` shape — that list really is member-exclusive; this one includes every operator-owned row too, so "Your" would overclaim) |

### States covered

- Operator: byte-identical to today — root constraint and the unavailable state never
  render for an operator session (`useOperator()` true short-circuits the whole branch,
  same shape as every other member-only annotation in this codebase, e.g.
  `selectedWorkspaceUngranted` in `new-run-screen.tsx:565-567`).
- Member with a configured root: path field usable, hint names the boundary, writable
  checkbox present only if `WARDYN_MEMBER_WRITABLE_ROOTS` also applies to them (a second,
  independent boolean on the same `/me` shape — omitted from the ASCII mock's "no
  writable" branch above since O3's default is off).
- Member with no root at all: `local_dir` stays selectable but explains itself instead of
  submitting; `Repository` and `Empty` are unaffected — a member can always onboard those.
- Server-side rejection (root escape, dotfile match, TOCTOU-era symlink swap): existing
  toast-error path, no new component.
