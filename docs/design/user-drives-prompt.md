# User drives — the admin's drive registry, the member's per-run mount

This is the mock round for the user-drives surfaces — the design gate before any console code
(owner law: the mock is UI source of truth; canon strings are app strings). The model below is
decided in `local/review-0.7/user-drives/DESIGN.md` (0.7 RC) and its eight owner defaults are
accepted; nothing here is open for re-design, only for drawing. **Six** drawing-level calls
remain and are listed as Q1–Q6 in §9, with an Adjudication section at the end for owner answers
and this round's notes.

One round covers all four surfaces at once (the precedent: one prompt + one static mock,
frozen strings byte-exact, Q-numbered owner calls, Adjudication at the end):

1. the **admin's** `/drives` screen (a new screen, SUPER-only in v1, no nav item),
2. the **security admin's** door — a third limit row in the governance profile editor,
3. the **member's** inline moments (no new screen — the New Run Workspace card, Getting
   Started, and the launch refusals), and
4. **canon frozen here, shipped later** — the run rail's drive line (§7.8), which waits on
   run-row persistence, and the storage `enforcement` vocabulary that `docs/OPERATIONS.md`
   and the two `DiskMiB` warn sites adopt.

Static mock: `docs/design/user-drives-mock/index.html` (open it in a browser).
Frozen strings: §7 below. No TS copy module exists yet — this is a mock round, the same way the
governance round preceded `ui/src/app/lib/governance-copy.ts`. The implementation stage creates
`ui/src/app/lib/user-drives-copy.ts` **from §7 verbatim**; it does not retype copy from this
document, and every string in the mock HTML matches §7 byte-for-byte. Its test,
`user-drives-copy.test.ts`, clones `governance-copy.test.ts`'s `parseFrozenTables()` over
§7.2–§7.8 of this file — which is why every table from §7.2 on is exactly two columns, `Key`
and `String`, and why §7.1 (reused canon and server strings) is not.

---

## 1. What exists today (the thing being grown)

The symbols are the durable half; the line numbers are not. They were checked once against the
0.7 RC (`feat/v0.7-profiles @ fa910735`, the tree `DESIGN.md` cites) — resolve the symbol,
re-locate the line at implementation.

**Nothing persists between runs, and nothing is per-person.** A run's writable layer dies with
the sandbox; `disk_mib` caps that layer only (`internal/runner/docker/hardening.go`, the
`--storage-opt size` branch). The only per-user mount facet in the tree is `RootsByPrincipal`
(`internal/runner/member_mount.go`) — a *narrowing allowlist over a path the member types* —
and the Kubernetes driver refuses every mount outright (`errMountsUnsupported`,
`internal/runner/k8s/sandbox.go`). Wardyn has no way to say "this directory is Alice's", on
either runner.

**Workspaces are what a run attaches.** The `/workspaces` screen (`screens/workspaces.tsx`) is
a `PageHeader` with one teal `Add workspace`; the setup funnel's `workspaces` step
(`setup/step-bodies.tsx`, `WorkspacesStep`) lists what is onboarded and opens the same
`AddWorkspaceDialog`; the New Run **Workspace card** (`new-run-screen.tsx`,
`SectionCard title="Workspace"`) is a `<Select>` whose empty option is "Ephemeral scratch — no
repo", with a ghost `Add workspace` under it. An ungranted workspace is annotated per row with
`DENIED.WORKSPACE_CHIP` and explained **on the selection, as one line**
(`DENIED.WORKSPACE_BODY`) — "the reason rides the SELECTION, not each row", because a Radix
item's content is what the closed trigger renders. That one-line rule is what the drive's
reason line inherits (§2.5). The Add-workspace dialog's three `OptionCard`s and its k8s
kind-switch are untouched by this round (§3).

**Governance profiles ship.** `/governance` (`screens/governance/`) is the security admin's
console; its profile editor's **Limits** section (`profile-editor.tsx`) renders two `LimitRow`s
— `GOV.LIMIT_EXEC_LABEL` / `GOV.LIMIT_INTERACTIVE_LABEL` — over `types.GovernanceLimits`, a
struct of booleans with zero DDL. A member's New Run reads one additive field,
`governance_profile_name`, off `GET /policies/default` (`lib/api/policies.ts`) — the **name**,
not the limits. Nothing client-side today can tell whether a given limit is set for the caller.

**`/permissions` owns the subject vocabulary.** `Segmented` (`screens/permissions.tsx`) and
`PERM.SUBJECT_USER / SUBJECT_GROUP / SUBJECT_ALL` with their three hints; the governance round
reused them wholesale and added `GOV.FIELD_PRIORITY / PRIORITY_HINT / PRIORITY_NA` and the
`DirectoryCombobox` canon (`DIRECTORY.*`, governance-prompt §7.9). Subjects resolve through the
same `capabilitySubjects` the drive resolver reads.

**Member Getting Started.** `member-getting-started.tsx` renders a "What's set up for you"
`SectionCard` whose body is a chip row — `BARRIER_CHIP`, a model-access chip,
`SIGNIN_SSO_CHIP`, and since the governance round `MEMBER.GS_CHIP(name)` — over
`SETUP_SUMMARY_HELPER`; below it an "Add your workspace" card (`WORKSPACE_TITLE /
WORKSPACE_BODY`). Every chip is `Label · value`.

**Settings summarises and links.** `screens/settings/settings-screen.tsx` is four cards; where a
card's subject has a real home elsewhere it *delegates* — the "Corporate proxy & egress"
disclosure is a two-line link button (label + meta) into the funnel step, read from the same
`SiteConfig` so it cannot go stale. That is the shape the drives card borrows in both places
(§6).

**The setup funnel's ORDER rule.** `setup/steps.ts` records why `corp_network` earned a step:
the order, not a banner, was the fix — later steps look broken without it. A drive is
orthogonal to onboarding: nothing after it in the funnel fails without one. **No `storage`
step** (`DESIGN.md` §4.1).

**`/me` is the member's "what applies to me".** `member_local_dir_root` is `null` when no root
applies (`internal/api/me.go`, the nil-means-unavailable convention). `user_drive` joins it with
the same convention: `null`, or `{name, size_mib, writable, backend, enforcement, paused}`
(`DESIGN.md` §5.1) — plus the one bit §2.5 needs and names.

**How a member is refused today.** A capability door is a 403 composed server-side by
`denyMemberField` (`internal/api/runs_create_validate.go`), audited as `authz.denied`; "you
are authorised and simply have nothing" is a **422 with no audit** (`denyMemberRunQuota`, same
file). Both bodies are `writeError` text: a lowercase-opening clause naming the wire field, and
**the console renders them verbatim** (`new-run/run-warnings.ts`'s rule). §7.7 is written in
that shape for that reason.

**The run rail.** `/runs/:id`'s `IdentityWidget` (`run-detail/widgets/identity.tsx`) is a
`<dl>` — Run · Identity · Image · Policy · Runner · Sandbox · Started — fed entirely from the
run row. A drive line belongs there and cannot be fed from the run row in v1 (§2.7).

### 1.1 What this round REUSES rather than builds

- **The subject picker and vocabulary are `/permissions`'s, unchanged** — `Segmented`,
  `PERM.SUBJECT_* / HINT_* / COL_WHO / FIELD_WHO / REMOVE`. An allocation and a capability grant
  never disagree about what "Everyone signed in" means.
- **The directory combobox is the governance round's** — `DIRECTORY.*` (governance-prompt
  §7.9): when a directory connector is configured the allocation's subject field IS that
  combobox; absent mode is silence. No string is added for it here.
- **Priority is the governance round's** — `GOV.FIELD_PRIORITY / PRIORITY_HINT / PRIORITY_NA`,
  because the tie-break rule is `governanceTierOrder` copied verbatim (`DESIGN.md` §2.4).
- **The preview's field is the People step's** — `PREVIEW.FIELD_CLAIMS / FIELD_CLAIMS_HINT`,
  and `GOV.PREVIEW_NOT_SAVED / PREVIEW_RESULT_UNKNOWN`: the drive preview is the same claims
  dry run over the same resolver shape (`POST /drives/preview`, `DESIGN.md` §2.5).
- **The door's row is the profile editor's `LimitRow`, unchanged** — a third instance of the
  ten-line switch; its two strings live in `governance-prompt.md` §7.2 and
  `governance-copy.ts`, never a second home (§5 #5).
- **The New Run Workspace card is the existing `SectionCard`** — it gains a checkbox, a hint
  and a reason line under the `<Select>`; the Select, its placeholder and its ghost button are
  byte-for-byte today's. (The card moves to `new-run/workspace-card.tsx` for the size gate;
  that is a file move, not a redesign.)
- **Refusals ride today's paths** — the 403/422 body render and the launch toast; no new
  member surface.
- **`EmptyState`, `ErrorState`, `TableSkeleton`, `PageHeader`, `Chip`, `Field`** — the shipped
  primitives; nothing new in `wardyn/`.

## 2. The model this round mocks

### 2.1 A drive

A **drive** is one place storage comes from: `{name, backend, host_root | storage_class,
home_template, size_mib, writable, reclaim}` in its own table (`user_drives`, migration `0054`
— never `SiteConfig`, `DESIGN.md` §2.2). Four backends, two kinds, and the kind is derived from
the backend in Go, never stored:

| Backend (wire) | Kind | Runner | What binds bytes | `enforcement` |
|---|---|---|---|---|
| `docker_volume` | managed | Docker | nothing — a volume has no byte cap | `none` |
| `host_path` | share | Docker | the share's own quota | `external` |
| `k8s_pvc` | managed | Kubernetes | the PVC request, **if** the storage class is a block class | `request` |
| `k8s_pvc_static` | share | Kubernetes | the share's own quota | `external` |

A drive's backend must match the deployment's runner (a write-time 400 on the API); the console
offers only the runner's two (Q3). A `host_path` drive additionally needs the deployment to set
`WARDYN_USER_DRIVE_HOST_ROOTS`, and its root must sit inside one of them — unset means no
host-path drives at all, the same fail-closed posture as `WARDYN_MEMBER_WORKSPACE_ROOTS`.

`size_mib` is an **allocation**, never a quota: it is what each person is shown, and on
Kubernetes the volume request. `0` shows no allocation. `reclaim` is a **declared intent**
(`retain` / `delete`) that v1 records and the admin carries out by a documented command; the
preview prints the exact object name that command needs (§2.3).

### 2.2 An allocation and precedence

An **allocation** binds one subject to one drive: `{subject_type, subject, drive_id, priority,
size_mib_override, writable_override, home_override, enabled}`, unique per subject, `ON DELETE
RESTRICT` against the drive. The same three subject types capability grants use. **One drive
per person** falls out of `LIMIT 1` over the ordered query, so an allocation form never asks
"which of your drives".

Precedence is `governanceTierOrder` verbatim, rendered as `PRECEDENCE` (§7.3): a person beats a
group beats everyone; within a person the sign-in subject beats the email; between groups the
higher priority wins, then the drive **name**. `priority` is meaningful only inside the group
tier — user and everyone rows show `GOV.PRIORITY_NA`.

The drive name is not the LAST key, because it cannot separate two allocations naming the SAME
drive — two groups one person is in, each granted one drive, both at the default priority. The
resolver's floor is therefore the allocation's **subject**, ascending: the alphabetically first
group's row wins. It matters because this resolver returns the ALLOCATION, and the allocation
carries the size, mode and directory overrides and the paused flag, so the tie decides whether
an admin's read-only narrowing applies. `PRECEDENCE` (§7.3) stops at the drive name and is
FROZEN; naming the last key in the console is a copy change for a later round.

Overrides are columns on the allocation row. A size override replaces the drive's size; a
writable override replaces the drive's mode **for that subject** (it may widen or narrow — an
admin's call); a home override names one person's exact directory and is accepted **on a
user-tier row only** (a group cannot share one directory). `enabled=false` **pauses** the
subject — it wins its tier and yields "paused", it does not fall through to a wider row — so
that turning Bob's row off cannot silently hand him the group's writable drive.

### 2.3 The home name, and what a member's request carries

The per-person directory (subdirectory / volume / PVC suffix) is **derived, never stored**:

- `hash` → `d-` + 20 hex of `sha256(drive.id + "\n" + winning-subject)`. Stable, DNS-safe,
  says nothing about the person. Default for managed backends.
- `sub` / `email_local` → the claim, lowercased, matching `^[a-z0-9][a-z0-9._-]{0,62}$`
  (lowercase letters and digits, then `. _ -`, up to 63 characters). Required for a share,
  because a corporate home is named by the corporation.
  A claim that cannot name a directory is a **422 at that person's run, never a guess**.
- `home_override` on a user-tier allocation wins over the template.

Object names: every name Wardyn MINTS carries the drive's slug — Docker volume and PVC alike,
`wardyn-drive-<drive-slug>-<home>` — because a `home_override` is written on the GRANT and does
not move when that grant is re-pointed at another drive; a share is `<host_root>/<home>`, scoped
by its root and named by whoever owns the tree. **The member's request carries `drive: {enabled, read_only}` and
never a path**; the server resolves subject → allocation → drive → home from the authenticated
identity. Only the subdirectory is bound — a run never sees the root or another person's
directory. The mount target is the reserved literal `/home/agent/drive`.

### 2.4 What the console refuses, and what it only warns about

Every write outcome is drawn, because each is a different kind of "no":

- **A drive whose host root is outside `WARDYN_USER_DRIVE_HOST_ROOTS` (or under a denied
  prefix) — REFUSED (400).** The roots are an env-borne ceiling the console cannot read, so this
  is drawn **post-attempt**: the console's heading (`SAVE_REFUSED_TITLE`) over the server's
  message, verbatim. The field's hint states the rule up front (`HOST_ROOT_HINT`).
- **A backend the runner cannot mount — REFUSED (400).** API-path only in practice: the console
  offers the runner's two backends (Q3), and on Docker with no roots set the `host_path` option
  is **disabled with its reason** (`BACKEND_UNAVAILABLE_DOCKER_ROOTS`) rather than offered and
  refused.
- **A share drive with a `hash` directory name — REFUSED (400).** A corporation names its own
  homes; the console disables the derived option for share backends and says why in
  `HOME_HINT`, and the server refuses the API path.
- **A managed drive with any non-`hash` directory name — REFUSED (400), mirror of the above
  (scope widened 2026-09-03).** A managed backend names the object after the directory, so a
  claim-derived template (`sub` or `email_local`) would publish the principal into a
  `docker volume ls` / `kubectl get pvc` name and collide two people who share it; the console
  disables every non-derived option for managed backends, `HOME_HINT` says why, and the server
  refuses the API path (§7.1).
- **Deleting a drive that is still allocated — REFUSED (409).** `ALLOCATED_COUNT` is
  client-visible, so at a non-zero count the delete dialog opens **pre-filled** with the
  refusal and its confirm disabled; the 409 stays authoritative for the race the count cannot
  see and is drawn post-attempt too — exactly the governance shape. Deleting the row never
  deletes data: directories stay until the admin reclaims them, and the dialog says so.
- **A size — never refused, never warned, only stated.** Wardyn does not enforce a drive's size
  itself. The console does not warn that a number "may not bind"; it says once, in one
  sentence, where each backend's size is enforced, requested, bounded elsewhere, or not at all
  (`HONESTY`, §7.2), and puts a one-line gloss on every size it renders (`ENFORCEMENT_*`).

At **launch**, the doors this feature adds are the server's, and they split the way today's
do: the profile **door** is a **403** with an audit row (`DENIED_DRIVE`); everything else —
no allocation, paused, an unusable claim, a missing share directory or claim, a backend the
runner cannot mount, `read_only:false` against a read-only allocation — is a **422 with no
audit**: the caller is authorised and simply has nothing to mount (§7.7).

### 2.5 The member's inline moments

The 0.6 doctrine holds: **no member drives screen, inline moments only.** Member nav is
unchanged.

- **New Run, Workspace card** — under the workspace `<Select>`, a checkbox `NR_CHECKBOX` with
  `NR_HINT` (name, size, mode) and one mode sentence (`NR_RW_NOTE` / `NR_RO_NOTE`); when the
  allocation is writable, `NR_READONLY_TOGGLE` beside it. The drive is **orthogonal to the
  workspace**: it renders whatever the Select says, including "Ephemeral scratch", and it is
  never a fourth `OptionCard` in the Add-workspace dialog. Two client-knowable reasons ride
  the selection as one line, the Workspace card's own rule: paused (`NR_PAUSED`) and the door
  (`NR_DENIED`). With `/me.user_drive` null **and** no door, there is no checkbox and no line —
  today's card byte-for-byte, the absent-row doctrine — so **no-allocation is not a reason line
  at all**: it is the absent row, and a sentence for it would have no state left to render in.
- **The door needs one bit on the wire.** `GET /policies/default` carries the profile's
  *name* only, and `DESIGN.md` §5.1's `/me.user_drive` shape carries no door — so `NR_DENIED`
  is drawable client-side only if `/me.user_drive` also carries `denied_by_profile` (the
  profile name, present only when the door is shut). That is the field this round assumes
  (Q6 asks the owner to confirm the home). The 403 stays authoritative post-attempt and is drawn
  too.
- **Getting Started, "What's set up for you"** — a drive chip in the existing chip row
  (`GS_DRIVE_CHIP`, `Label · value` like every neighbour), only when `/me.user_drive` is non-null;
  a paused allocation shows `GS_DRIVE_CHIP_PAUSED`. One sentence in the **"Add your workspace"**
  card (`GS_DRIVE_BODY`) says a drive is not a workspace — placed there because that card is where
  the two get conflated (`DESIGN.md` §5.4).
- **Refusals** — the eight server messages in §7.7, rendered verbatim on the launch path.
  Backend-unavailable is **post-attempt only**: nothing client-side can know whether the cluster
  will let the runner create a claim.

### 2.6 States

- **Populated** — drives with allocations, the preview answered.
- **Empty (no drives yet)** — runs keep nothing between them; the empty state carries the
  action that fills it.
- **Drive with no allocation** — distinct words (`ALLOCATED_NONE`): a drive nobody mounts.
- **Empty (drives, no allocations)** — the allocations table's own empty state.
- **Fetch-failed** — distinct from empty. Allocations that already exist keep binding every
  run; this list just cannot show them.
- **Editor open** — the allocation form collapses to its disabled `ADD_TITLE` row (one teal at
  a time, the governance shape).
- **Write refusals** — the 400s post-attempt, the 409 pre-filled and post-attempt (§2.4).
- **Backend unavailable** — Docker with no roots: the option disabled with its reason.
  Kubernetes with a cluster that refuses claim creation: a **422 at first run**, nothing earlier
  (see Adjudication — the preview payload carries no warning field).
- **Preview answered / no allocation / couldn't preview.**
- **Member: granted writable · granted read-only · no allocation · paused · denied · refused at
  launch.**

### 2.7 Canon frozen here, shipped later

- **The run rail's drive line (§7.8).** The `IdentityWidget` is fed from the run row, and
  THREAT-MODEL §4.6 ("concurrent runs share one drive") puts drive persistence on the run row
  in 0.7.1. The line is frozen now so the rail does not get a second copy round; it renders
  when the run row carries a drive, and until then it does not exist.
- **The `enforcement` vocabulary.** `types.StorageEnforcement` — `filesystem`, `request`,
  `external`, `none` — is introduced by this feature and adopted by `docs/OPERATIONS.md`'s
  known-gaps section and, later, the two `DiskMiB` warn sites. The four glosses (§7.2) are
  frozen here for all four values even though no v1 drive backend yields `filesystem`: the chip
  map must be total, and the writable-layer cap will reuse the fourth.
- **The honesty sentence** (`HONESTY`, §7.2) is one string for docs and console. `OPERATIONS.md`
  quotes it; the console renders it. Two wordings of "we do not enforce this" is how one of them
  starts to overclaim.

## 3. Do not design (out of scope this round)

- **No setup step.** `steps.ts`'s ORDER test fails a drive: nothing later breaks without one.
  A card in the `workspaces` step body, and nothing more.
- **No nav item.** Entry is the Workspaces header button, the setup card, the Settings card
  (`DESIGN.md` §11 Q7, accepted).
- **No fourth `OptionCard`** in the Add-workspace dialog. A drive is allocated, not onboarded.
- **No Reset.** Member self-service reset is 0.7.1; a poisoned managed drive is reclaimed by
  the admin's command. No string exists for it.
- **No usage meter.** v1 has no cheap usage read. The chip says the allocation, not the fill.
- **No in-product reclaim.** `reclaim` is recorded intent; the command is documented. No
  "Reclaim now" button, no PVC delete verb.
- **No share credentials, no per-user uid, no Kerberos.** The operator mounts shares host-side;
  Wardyn never holds a share credential and never passes one as a volume option.
- **No policy field.** The flag is on the run request; `POLICIES.md` gains only the reserved
  target note.
- **No CLI.** `wardyn drive get|apply` is 0.7.1.
- **No collision warning** for one person's concurrent runs on one drive (THREAT-MODEL §4.6 ("concurrent runs share one drive")).
- **No change to `/governance` beyond the third row**, and no change to the Limits lead
  (Q4 — a copy change, owner-gated, not this round's).
- **No re-record** of the demo videos; 04d and the 12b beat arrive with the feature.

## 4. Design system

Same token block and CSS idioms as `docs/design/governance-mock/index.html` (`--background` /
`--card` / `--surface-2` / `--foreground` / `--muted-foreground` / `--border` /
`--border-strong` / `--primary` / `--success` / `--warning` / `--danger` / `--info`, light and
dark, Inter + JetBrains Mono, `chip`/`btn`/`sw`/`seg`/`dlg`/`note`/`card` idioms; the four
rungs, no ad-hoc sizes — `CONSOLE-RULES.md` §3).

**Teal is in budget on `/drives`** — a full screen, so exactly **one** `default` button at a
time: **Allocate** at rest; **New drive** when the drives table is empty (the empty state carries
the action that fills it, and there is nothing to allocate yet); **Save drive** while the editor
is open, during which the allocation form collapses to its disabled `ADD_TITLE` row. Everything
else is `outline` or `ghost`; Delete is `destructive` with a confirmation dialog; **Remove** (an
allocation) is `outline`, because unallocating is reversible and deletes nothing.

**Zero teal on the setup card and the Settings card.** The funnel's footer Next is the step's
one affirmative action, and Settings' cards delegate; the drives card is a two-line link button
in both places (§6), never a form.

**The Workspaces header** gains one `outline` button beside its teal `Add workspace`; the teal
stays where it is.

Colour, stated per rule:

- **Writable is amber, read-only is neutral.** `MODE_RW` is a widened blast radius — the
  drive's own hint says "what a compromised run could alter" — the same reason `/permissions`
  paints an allow amber. It is a fact-chip with a word, never colour alone.
- **Enforcement glosses are neutral.** Where a size binds is a fact about the backend, not a
  risk grade. The honesty note is `plain`.
- **Paused is neutral** — a fact about the row, with the word on it.
- **Amber and red carry genuine risk and error only:** the write refusals, the 409, the member
  refusals, fetch-failed.
- **Mono is for literals only:** the mount target, backend wire values, object names, directory
  names, host roots, storage classes, env var names, the `drive` request field. A **drive name
  is never mono**: it is a human-chosen label, rendered inside double quotes exactly as §7
  spells it — everywhere except inside a `Label · value` chip, where the `·` already delimits
  it. A `{size}` is plain text.
- **The backend column is a kind chip over a mono wire value** (`KIND_MANAGED` / `KIND_SHARE`
  above `host_path`), the WHO-over-WHAT two-glyph rule applied to an object instead of an actor.

## 5. Hard canon constraints

1. **The honesty sentence is one string, `HONESTY`, frozen here and quoted by
   `docs/OPERATIONS.md`** — never paraphrased on either side. Every other size string says
   *allocation*; the words *quota* and *enforces* appear only where they are attributed to a
   share, a storage class, or Wardyn's own absence of enforcement.
2. **No member-facing string names a host path, a storage object, or another person.** The
   object name renders to admins only (the preview). A member's request carries a flag, never a
   path — this is the Iris guardrail the feature is built on, and the copy must not leak around
   it.
3. **`/home/agent/drive` is one literal, mono, spelled once.** Every string that names the
   target uses it verbatim; the reserved-target refusal (§7.7) names it too.
4. **Every string a member is refused with is composed server-side** and rendered verbatim
   (`run-warnings.ts`'s rule). §7.7 is a table of *server* strings in the in-tree `writeError`
   shape, and it is **complete** for the doors this feature adds. `DENIED_STALE_GROUPS` is
   reused from governance-prompt §7.7, not re-frozen.
5. **The door's two strings live in `governance-prompt.md` §7.2 and `governance-copy.ts`** —
   appended there in this round, listed here in §7.1 as reused. The profile editor renders them
   from `GOV.*`; the drives module never carries them.
6. **The subject vocabulary is `permissions-copy.ts`'s, the priority copy and the preview's
   "not saved" are `governance-copy.ts`'s, the preview field is `people-access-copy.ts`'s** —
   referenced, never retyped (§7.1).
7. **No nav item; `MEMBER_NAV_PATHS` unchanged; every `/drives*` route is SUPER.** The security
   admin's console hides the screen and its entry points and shows only the door.
8. **Reset does not exist.** No string, no button, no disabled placeholder.
9. **Pluralisation is the inline ternary** `PERM.ENFORCE_ON_BODY` already uses, one alternation
   per key — `ALLOCATED_COUNT`, `DELETE_RESTRICT_BODY`, `CARD_DRIVES`, `CARD_ALLOCATIONS` — never
   a second helper.
10. **Sizes render through one helper** — `SIZE_MIB(n)` and, for whole multiples of 1024,
    `SIZE_GIB(n)`; `size_mib = 0` renders `SIZE_NONE`. `lib/format.ts`'s `fmtBytes` is **not**
    reused: it labels a binary quotient "MB" and stops at megabytes. `SIZE_MIB` / `SIZE_GIB` take
    a number, so `user-drives-copy.test.ts` checks them in their own `it` block (`SIZE_MIB(8)`
    against the doc cell with `{n}`→`8`) rather than through the placeholder-substitution path,
    alongside the four pluralised keys.
11. **The drives module exports no name `governance-copy.ts` already exports.** Its namespaces
    are `DRIVES`, `DRIVE_MEMBER` and `DRIVE_RUN`; `MEMBER.GS_CHIP` / `MEMBER.GS_BODY` are the
    governance round's and render on the same Getting Started screen as `DRIVE_MEMBER.GS_DRIVE_*`,
    so a same-named twin would be a silent shadow at the one import site (`member-getting-started.tsx`).

**Assertion sites this round's copy is already pinned to** — a change here that is not
reflected in these is a broken test, not a free edit:

- `ui/src/app/lib/governance-copy.test.ts` — the two appended §7.2 rows made `doc.size` **89**;
  `governance-copy.ts` carries `GOVERNANCE.LIMIT_DRIVE_LABEL` / `LIMIT_DRIVE_HINT` and the test's
  `rendered` map lists both, so the suite is green with this round (the door's keys landed with
  it, `DESIGN.md` §4.1 #4).
- `ui/src/app/components/screens/governance/profile-editor.tsx` — the Limits section's `LimitRow`
  count, and any e2e that counts switches on the editor (`ui/e2e/governance.spec.ts`).
- `ui/src/app/components/screens/new-run/new-run-screen.tsx` — the Workspace card block that
  moves to `new-run/workspace-card.tsx`; `wizard-spec.ts`'s `buildSpec` gains `run.drive`.
- `ui/src/app/components/screens/onboarding/member-getting-started.tsx` — the chip row and the
  Workspace card's body.
- `ui/src/app/components/screens/setup/step-bodies.tsx` (`WorkspacesStep`) and
  `screens/settings/settings-screen.tsx` — the two homes of the shared card.
- `ui/src/app/components/screens/workspaces.tsx` — the header's `actions` slot.
- `docs/OPERATIONS.md` known-gaps — quotes `HONESTY` verbatim; `docs/MEMBERS.md` "Your drive".

Implementation is not done when it builds and unit tests pass: the UI e2e suite is daemon-only
and is **not** part of `make ci` — run `scripts/run-ui-e2e.sh drives` before calling any of it
done.

## 6. Where the model lives on the page

**New screen `/drives`** (SUPER-only; no nav entry — reached from the Workspaces header's
`outline` button, the setup card, and the Settings card). Top to bottom:

1. **Header** — `TITLE`, `LEAD`, and `NEW_CTA` (`outline` at rest, teal only in the empty
   state).
2. **Drives** — the table (name · backend · size · mode · when a person leaves · allocated to)
   with Edit / Delete per row, `HONESTY` as one plain note under it; the editor opens **in
   place** under the table (name, backend, the backend's one extra field, directory name, size,
   writable, when a person leaves; `SAVE_CTA` teal, `PEOPLE.CANCEL` ghost).
3. **Allocations** — the table (who · drive · priority · overrides · added) with Remove per row,
   the precedence note, the two effect notes, and the add form (subject `Segmented` + the
   directory-aware subject field, drive, priority, size override, writable override, directory
   name, enabled; `ADD_CTA` teal). **While the editor above is open this form is collapsed to its
   disabled `ADD_TITLE` row.**
4. **Who gets what** — the preview: paste the claims a person's token would carry, get the one
   drive those claims resolve to, the directory and storage object names, the size with its
   enforcement gloss, and the mode. Field label and hint are `PREVIEW.FIELD_CLAIMS / _HINT`.

**The door**, on `/governance`: a third `LimitRow` in the profile editor's Limits section,
after "Deny interactive runs"; its chip in the profiles table's Limits column beside the other
two.

**The card**, in two places from one component (`setup/user-drives-card.tsx`): the `workspaces`
step body, under the workspace list; and Settings, as a fifth card. Title `TITLE`, one lead
line, a summary (`CARD_SUMMARY` or `CARD_EMPTY`), and a two-line link button `CARD_OPEN` into
`/drives`. Rendered only for SUPER — a security admin sees nothing, there being nothing for them
to act on.

**Member surfaces, inline, in place:**

- `new-run/workspace-card.tsx` — checkbox, hint, mode sentence, read-only toggle, and the
  reason line, all under the workspace `<Select>` (§7.6).
- `member-getting-started.tsx` — one chip in the summary card's chip row; one sentence in the
  "Add your workspace" card (§7.6).
- Launch refusals — the existing 403/422 render path (§7.7).

**Frozen but not placed this round:** the run rail's `Drive` row (§7.8).

## 7. Canonical strings — FROZEN

Throughout §7, a backticked substring inside a frozen string (the mount target, an env var, a
wire field or value, a directory or object name) renders `font-mono` in the console and in the
mock — apply the mono span uniformly everywhere that substring recurs. A **drive name** is never
mono: it is a human-chosen label and is rendered inside double quotes, exactly as the strings
below spell it. A `{size}` is plain text from the size helper (§5 #10); a `{mode}` inside a
sentence is `MODE_RO_INLINE` / `MODE_RW_INLINE`, and on a chip or in a table cell `MODE_RO` /
`MODE_RW`.

**Server-refusal shape (§7.7 only).** The member-facing refusals are `writeError` bodies and
follow the in-tree convention — a lowercase-opening clause naming the wire field or the thing
refused (`denyMemberRunQuota`, `denyMemberField`) — not a console-styled sentence. The console
renders them verbatim.

### 7.1 Reused canon — referenced, never re-frozen

These strings already exist and are **imported**, not retyped, by the drives surfaces. Listed so
that every product string rendered in the mock has a key somewhere.

| Key | Lives in | String |
|---|---|---|
| `PERM.COL_WHO` / `PERM.FIELD_WHO` | `permissions-copy.ts` | Who |
| `PERM.COL_ADDED` | `permissions-copy.ts` | Added |
| `PERM.SUBJECT_USER` / `SUBJECT_GROUP` / `SUBJECT_ALL` | `permissions-copy.ts` | User / Group / Everyone signed in |
| `PERM.HINT_USER` | `permissions-copy.ts` | An email address or the sign-in subject id. Either one matches the same person. |
| `PERM.HINT_GROUP` | `permissions-copy.ts` | A group or app-role name exactly as your identity provider sends it in the token. |
| `PERM.HINT_ALL` | `permissions-copy.ts` | Every signed-in member. Admins are exempt. |
| `PERM.REMOVE` | `permissions-copy.ts` | Remove |
| `PEOPLE.CANCEL` | `people-access-copy.ts` | Cancel |
| `PREVIEW.FIELD_CLAIMS` | `people-access-copy.ts` | Roles, groups, or email |
| `PREVIEW.FIELD_CLAIMS_HINT` | `people-access-copy.ts` | One value per line — an App Role, a group name, or an email address. |
| `ACCESS_STATE.FETCH_FAILED_RETRY` | `people-access-copy.ts` | Retry |
| `GOV.FIELD_PRIORITY` / `GOV.COL_PRIORITY` | `governance-copy.ts` | Priority |
| `GOV.PRIORITY_HINT` | `governance-copy.ts` | Breaks ties between groups a person is in. Higher wins. It is ignored for a person and for everyone. |
| `GOV.PRIORITY_NA` | `governance-copy.ts` | — |
| `GOV.PREVIEW_NOT_SAVED` | `governance-copy.ts` | Nothing here is saved. |
| `GOV.PREVIEW_RESULT_UNKNOWN` | `governance-copy.ts` | Couldn't resolve this — try again. |
| `GOV.LIMITS_TITLE` / `GOV.LIMITS_LEAD` | `governance-copy.ts` | Limits / Some of what a run can do routes around the ceiling entirely. Deny it here instead. |
| `GOV.LIMIT_EXEC_LABEL` / `GOV.LIMIT_INTERACTIVE_LABEL` | `governance-copy.ts` | Deny exec runs / Deny interactive runs |
| **`GOV.LIMIT_DRIVE_LABEL`** | `governance-prompt.md` §7.2 (appended this round) → `governance-copy.ts` | Deny mounting a user drive |
| **`GOV.LIMIT_DRIVE_HINT`** | `governance-prompt.md` §7.2 (appended this round) → `governance-copy.ts` | A run under this profile cannot mount the person's drive, even when one is allocated to them. |
| `DIRECTORY.*` | `governance-copy.ts` | The combobox's suggestion row, group chip, note and three lookup states (governance-prompt §7.9) — the allocation's subject field when a directory is configured |
| `MEMBER.DENIED_STALE_GROUPS` | `governance-copy.ts` | groups_snapshot_stale: … — the truncated-snapshot refusal, reused verbatim when group-tier allocations exist |
| `MEMBER_GETTING_STARTED.SETUP_SUMMARY_TITLE` / `_HELPER` | `wardyn/copy.ts` | What's set up for you / Your admin configured the barrier, network and shared credentials. Your runs inherit them. |
| `MEMBER_GETTING_STARTED.BARRIER_CHIP(label)` / `MODEL_ACCESS_PROVIDED_CHIP` / `SIGNIN_SSO_CHIP` | `wardyn/copy.ts` | Barrier · {label} / Model access · Provided by your admin / Sign-in · SSO |
| `MEMBER_GETTING_STARTED.WORKSPACE_TITLE` / `WORKSPACE_BODY` / `WORKSPACE_ACTION` | `wardyn/copy.ts` | Add your workspace / A repo or directory a run can attach. Runs can only attach what is listed here. / Add workspace |
| `MEMBER.GS_CHIP(name)` | `governance-copy.ts` | Governance · {name} |
| `MEMBER.GS_BODY(name)` | `governance-copy.ts` | Your runs are bounded by "{name}". What you can change is what it leaves open. |
| Workspaces page header + action | `workspaces.tsx` | Workspaces · A repo or directory a run can attach. Runs can only attach what's listed here. · Add workspace |
| New Run workspace section + Select | `new-run-screen.tsx` | Workspace · Ephemeral scratch — no repo · Add workspace |
| `DENIED.WORKSPACE_CHIP` | `permissions-copy.ts` | Not granted |
| Setup step heading (`STEP_HEADING.workspaces`) | `setup/steps.ts` | Onboard a workspace |
| Settings page title + Host card | `settings-screen.tsx` | Settings · Host · The barriers this machine can build, and what every run inherits by default. |
| `IdentityWidget` labels | `run-detail/widgets/identity.tsx` | Identity · Run · Image · Policy · Runner · Sandbox · Started |
| `relativeTime(t)` | `lib/format.ts` | "3 days ago" — the shipped helper |
| Launch-warning toast title | `run-warnings.ts` | Run launched with a warning |

**Composed by the server — rendered verbatim, never keyed (the `governance-copy.ts` "deliberately
absent" rule).** The drives module carries **no copy** of these. They are the wording the Go
side emits (P2 transcribes them beside the gates that raise them); the console renders them from
the wire under its own heading. They are listed so the mock can draw them and so an implementer
does not freeze a second wording.

**This table is DERIVED FROM THE CODE, and checked against it.** `Emitted by` names the Go
function that composes the string, `String` is that function's own literal with its format verbs
written as `{placeholders}` (or as the constant the verb is always given), and the status is the
one the write boundary answers with. `user-drives-copy.test.ts` parses these three columns back
out and matches every row against the real literal in `internal/api`, `internal/types` and
`internal/runner` — a row the code cannot emit fails there. Two things the rows leave out, both
for the same reason (the table freezes the refusal, not the envelope): the write boundary's own
`invalid drive: ` / `invalid allocation: ` prefix, and the ` (resolves to "{real}")` clause the
`host_root` rows gain when the path is a symlink that lands somewhere else.

| Source | Emitted by | String |
|---|---|---|
| Host root outside the roots (422) | `UserDriveHostRootCheck` | host_root "{path}" is not inside WARDYN_USER_DRIVE_HOST_ROOTS ({roots}) — a drive may bind only a subdirectory of a root this deployment allows |
| Host root under a denied prefix (422) | `UserDriveHostRootCheck` | host_root "{path}" is under a denied prefix ({prefix}) — the same deny list every host bind obeys |
| Host root inside another drive's (422) | `driveHostRootNesting` | host_root "{path}" is inside drive "{name}"'s host_root "{other}" — that tree holds directories the other drive's members can write from inside a run, so they could redirect this one; give the two drives separate trees |
| Host root containing another drive's (422) | `driveHostRootNesting` | host_root "{path}" contains drive "{name}"'s host_root "{other}" — this drive's members could redirect that one from inside a run; give the two drives separate trees |
| Backend / runner mismatch (400) | `ValidateUserDrive` | backend "{backend}" cannot be mounted by this deployment's runner ({runner}) |
| Template invalid for a share (400) | `ValidateUserDrive` | home_template "hash" is not allowed on a share backend — a share's directories are named by your directory, so pick sub or email_local |
| Template invalid for a managed backend (400) | `ValidateUserDrive` | home_template "{template}" is not allowed on a managed backend — Wardyn names the object after the directory, so the template lands in a {backend} object name that `docker volume ls` and `kubectl get pvc` show without inspecting anything; email_local also collides two people whose addresses share the part before the "@". Use hash, which is unique and reveals nothing; a share backend keeps every template |
| Size required for a managed claim (400) | `ValidateUserDrive` | size_mib must be above 0 for a k8s_pvc drive — it is the volume request |
| Delete while allocated (409) | `handleDeleteUserDrive` | this drive is still allocated — remove its allocations first (deleting it while allocated would leave those subjects with a mount that names nothing) |
| Identity-affecting PUT on an allocated drive (409) | `driveRehomeGuard` | this drive is allocated to {n subjects} and this change re-homes {them}: {backend "host_path" → "docker_volume", …}. Every allocated person's storage object is derived from these fields, so their next run mounts a different object and the one holding their work is left behind with nothing in Wardyn naming it. Confirming is an API action, not a console one: re-send as PUT /drives/{id}?confirm=rehome. |
| Home override on a non-user row (400) | `ValidateUserDriveGrant` | home_override is accepted on a user-tier allocation only — a group cannot share one directory |
| Home override already held on this drive (409) | `driveGrantConflictMsg` | another allocation on this drive already uses the directory name "{home}" — a directory name is one person's, which is why a group allocation may not carry one; pick a different name or remove the allocation that holds it |
| Home override unstated on an allocation that pins one (409) | `driveGrantConflictMsg` | this allocation pins a directory name and your request did not mention home_override — writing it would CLEAR that name, so this subject's next run would mount a different object and the one holding their work would be left behind with nothing in Wardyn naming it. Re-send with home_override set to the name you want kept, or to "" to drop it deliberately |
| Stricter home rule on a Kubernetes backend (**suffix**, 422) | `DriveHomeStricterRuleClause`, appended by `newResolvedDrive` after `REFUSED_HOME_INVALID` | (on a Kubernetes deployment the rule is stricter: no _, and it may not end in - or .) |

The last row is a **suffix, not a rewording**. `REFUSED_HOME_INVALID` (§7.7) is frozen and
describes `driveHomeSegmentRe`, the DOCKER rule; a `k8s_pvc`/`k8s_pvc_static` home must satisfy
`driveHomeSegmentK8sRe`, which also forbids `_` and a trailing `-`/`.` — and that gap is the
motivating case, since an Entra `sub` is base64url and routinely carries `_`. The frozen sentence
still ships byte-for-byte on every deployment; a Kubernetes one appends the clause its own regex
enforces, which is what this table's rule is for.

The 409 body **carries no count and must not grow one**: `DELETE_RESTRICT_BODY` (§7.4) is the
client-side pre-fill and names the count the list already shows; on the race path the client
believed the count was zero and renders the wire text.

The **re-home 409** is the second row that names a count, and it names its own: nothing on the
client can pre-fill it, because which of the four identity columns a PUT changes is only known
once the stored row and the submitted one are compared. It ends by naming an **API** action on
purpose. The console has no confirm affordance — `updateDrive` PUTs `/drives/{id}` with no query
and the editor renders any `HttpError` under `DRIVES.SAVE_REFUSED_TITLE` (§7.4) — so a remedy
phrased as "re-send with `?confirm=rehome`" reads, on the one screen that raises it, as a button
an admin cannot find. A confirm dialog is new UI and new copy: **FILED for a mock round**
(CONSOLE-RULES §12), not invented here. Until it exists the sentence must keep saying that
confirming happens through the API.

### 7.2 `DRIVES` — the drives block

| Key | String |
|---|---|
| `TITLE` | User drives |
| `LEAD` | Persistent storage a run can mount at `/home/agent/drive`. An admin registers a drive and allocates it to people or groups; each person gets their own directory in it, and chooses per run whether to mount it. |
| `DRIVES_TITLE` | Drives |
| `DRIVES_LEAD` | A drive is one place storage comes from: a share your platform already mounts, or a volume Wardyn creates per person. |
| `COL_NAME` | Name |
| `COL_BACKEND` | Backend |
| `COL_SIZE` | Size |
| `COL_MODE` | Mode |
| `COL_RECLAIM` | When a person leaves |
| `COL_ALLOCATED` | Allocated to |
| `NEW_CTA` | New drive |
| `EDIT` | Edit |
| `DELETE` | Delete |
| `KIND_MANAGED` | Managed |
| `KIND_SHARE` | Share |
| `ALLOCATED_NONE` | Allocated to nobody |
| `ALLOCATED_COUNT(n)` | {n} subject / {n} subjects |
| `MODE_RO` | Read-only |
| `MODE_RW` | Writable |
| `MODE_RO_INLINE` | read-only |
| `MODE_RW_INLINE` | writable |
| `SIZE_NONE` | No allocation shown |
| `SIZE_MIB(n)` | {n} MiB |
| `SIZE_GIB(n)` | {n} GiB |
| `EMPTY_TITLE` | No drives yet |
| `EMPTY_BODY` | Runs keep nothing between them today. Register a drive to give people a directory that persists. |
| `EDITOR_TITLE_NEW` | New drive |
| `EDITOR_TITLE_EDIT(name)` | Edit "{name}" |
| `FIELD_NAME` | Name |
| `NAME_HINT` | What this drive is called on the allocations below and in a member's run. Names are unique. |
| `FIELD_BACKEND` | Backend |
| `BACKEND_HINT` | Only the backends this deployment's runner can mount are offered. |
| `BACKEND_DOCKER_VOLUME` | Wardyn-managed volume (this Docker host) |
| `BACKEND_HOST_PATH` | Share mounted on this host |
| `BACKEND_K8S_PVC` | Wardyn-managed volume (a storage class) |
| `BACKEND_K8S_PVC_STATIC` | Share provisioned by your platform (a pre-created volume claim per person) |
| `BACKEND_UNAVAILABLE_DOCKER_ROOTS` | Not available: this deployment sets no `WARDYN_USER_DRIVE_HOST_ROOTS`, so no host path may back a drive. |
| `FIELD_HOST_ROOT` | Host root |
| `HOST_ROOT_HINT` | The mounted share's root on this host, under one of `WARDYN_USER_DRIVE_HOST_ROOTS`. Each person's directory is a subdirectory of it; only that subdirectory is ever mounted into a run. |
| `FIELD_STORAGE_CLASS` | Storage class |
| `STORAGE_CLASS_HINT` | Leave empty for the cluster default. A block storage class enforces the size; a network-share provisioner does not. |
| `FIELD_HOME` | Directory name |
| `HOME_HINT` | How each person's directory is named inside the drive. A share uses the name your directory already has; a managed drive can use a derived id. |
| `HOME_HASH` | Derived (stable id) |
| `HOME_HASH_HINT` | A short id derived from the drive and the person. It never collides and says nothing about who it is — Preview below finds a person's directory. |
| `HOME_SUB` | Sign-in subject |
| `HOME_SUB_HINT` | The subject id your identity provider sends, lowercased. Stable, but rarely what a share already calls a person. |
| `HOME_EMAIL_LOCAL` | Email, before the @ |
| `HOME_EMAIL_LOCAL_HINT` | The part before the @, lowercased — the usual shape of a corporate home directory. |
| `HOME_RULE` | A directory name is lowercase letters and digits, then any of `. _ -`, up to 63 characters. A person whose claim cannot name one is refused at their run, never guessed. |
| `FIELD_SIZE` | Size (MiB) |
| `SIZE_HINT` | The allocation shown to each person, and on Kubernetes the volume request. 0 shows no allocation. Override it per person below. |
| `SIZE_HINT_REQUIRED` | The allocation shown to each person, and the volume request Kubernetes makes. Required — a claim cannot request zero. Override it per person below. |
| `FIELD_WRITABLE` | Writable |
| `WRITABLE_HINT` | Off by default. A writable drive is where a run's changes persist — and what a compromised run could alter. |
| `FIELD_RECLAIM` | When a person leaves |
| `RECLAIM_RETAIN` | Keep their directory |
| `RECLAIM_DELETE` | Delete their directory |
| `RECLAIM_HINT` | Recorded here, carried out by you: removing an allocation below never deletes data. Preview prints the exact object to remove. |
| `SAVE_CTA` | Save drive |
| `SAVE_ERROR` | Couldn't save this drive. |
| `SAVE_REFUSED_TITLE` | This drive can't be saved as written |
| `ENFORCEMENT_FILESYSTEM` | Size enforced by the filesystem |
| `ENFORCEMENT_REQUEST` | Size requested; the storage class decides |
| `ENFORCEMENT_EXTERNAL` | Size bounded by the share's own quota |
| `ENFORCEMENT_NONE` | Size shown, not enforced |
| `HONESTY` | Wardyn never enforces a drive's size itself. On Kubernetes the size is the volume request and the storage class decides whether it binds — block disks do, network-share provisioners do not. On Docker a managed drive has no byte cap, the same gap `disk_mib` has. A share is bounded by its own quota. The size you see is the allocation, not a guarantee. |

`TITLE` is **one string for four places** — the screen heading, the Workspaces header's outline
button, the setup card's title and the Settings card's title — the way `GOVERNANCE.TITLE`
serves nav and heading. `ALLOCATED_COUNT` is the `PERM.ENFORCE_ON_BODY` inline ternary. The
backend column renders `KIND_MANAGED` / `KIND_SHARE` as a neutral chip over the backend's wire
value in mono; the four long `BACKEND_*` labels are the editor's select options only. A size
cell renders `SIZE_MIB` / `SIZE_GIB` (or `SIZE_NONE`) with the drive's `ENFORCEMENT_*` gloss as
its sub-line, so the honesty is on every number, not only in the note. `HONESTY` renders once,
as the plain note under the drives table (Q1). `ENFORCEMENT_FILESYSTEM` is frozen for a value no
v1 backend yields (§2.7). `HOME_RULE` renders under the directory-name field for every option.
`SIZE_HINT_REQUIRED` replaces `SIZE_HINT` under the Size field when the backend is `k8s_pvc`
(Q7); no required-marker glyph exists — the hint carries the word.

### 7.3 `DRIVES` — allocations and the preview

| Key | String |
|---|---|
| `ALLOC_TITLE` | Allocations |
| `ALLOC_LEAD` | Who gets a drive. A person's own row beats their group's; a group's beats everyone's. One drive per person. |
| `PRECEDENCE` | The most specific allocation wins: a person beats a group, and a group beats everyone. Within a person, the sign-in subject beats the email. Between groups, the higher priority wins, then the drive name. |
| `EFFECT_NOTE` | Takes effect on their next run. A run already dispatched keeps what it mounted. |
| `SIGNIN_NOTE` | A change to someone's groups in your identity provider reaches Wardyn only when they next sign in, so a group's allocation reaches them then — or, for an API token, when it is re-minted. |
| `ADD_TITLE` | Allocate a drive |
| `ADD_CTA` | Allocate |
| `FIELD_DRIVE` | Drive |
| `DRIVE_PLACEHOLDER` | Choose a drive |
| `FIELD_SIZE_OVERRIDE` | Size override (MiB) |
| `SIZE_OVERRIDE_HINT` | Empty or 0 keeps the drive's size. |
| `FIELD_WRITABLE_OVERRIDE` | Writable |
| `WRITABLE_OVERRIDE_INHERIT` | Same as the drive |
| `WRITABLE_OVERRIDE_HINT` | Same as the drive, or set it for this subject alone. A run can narrow the result to read-only, never widen it. |
| `FIELD_HOME_OVERRIDE` | Directory name |
| `HOME_OVERRIDE_HINT` | For one person only: the exact directory name inside the drive, when it differs from the drive's rule. |
| `HOME_OVERRIDE_NA` | Only a person's own allocation can name a directory. |
| `FIELD_ENABLED` | Enabled |
| `ENABLED_HINT` | Off pauses the drive for this subject without deleting anything. |
| `COL_DRIVE` | Drive |
| `COL_OVERRIDES` | Overrides |
| `OVERRIDES_NONE` | None |
| `OVERRIDE_SIZE(size)` | Size · {size} |
| `OVERRIDE_HOME(name)` | Directory · {name} |
| `PAUSED_CHIP` | Paused |
| `ALLOC_REPLACED` | This subject already had an allocation — it was replaced. |
| `REMOVE_CONFIRM(who, name)` | Remove "{name}" from {who}? Their next run mounts whatever else matches them, or nothing. Nothing on the drive is deleted — the directory stays until you reclaim it. |
| `EMPTY_ALLOC_TITLE` | No allocations yet |
| `EMPTY_ALLOC_BODY` | A drive with no allocation is mounted by nobody. Allocate one to a group to start. |
| `PREVIEW_TITLE` | Who gets what |
| `PREVIEW_LEAD` | Paste the roles, groups, or email a person's token would carry, and see which drive and directory they would mount. |
| `PREVIEW_CTA` | Preview |
| `PREVIEW_NONE` | No drive is allocated to these claims. |
| `PREVIEW_RESULT(drive, tier)` | "{drive}" via the {tier} allocation |
| `PREVIEW_TIER_USER` | user |
| `PREVIEW_TIER_GROUP` | group |
| `PREVIEW_TIER_ALL` | everyone |
| `PREVIEW_OBJECT_LABEL` | Storage object |
| `PREVIEW_OBJECT_HINT` | What the reclaim command names — copy it when someone leaves. |
| `PREVIEW_ENFORCEMENT_LABEL` | Enforcement |

**`EFFECT_NOTE` and `SIGNIN_NOTE` are two halves of one fact and render together**, the
governance round's lesson carried over with the object changed: an **allocation** is a row the
resolver re-reads every run, so it binds at the next *run*; a person's **group membership** is
read once at sign-in, so a group's allocation reaches them at their next *sign-in* — or, on an
API token, its next mint. `GOV.SIGNIN_NOTE` is not reused: it ends on "keep their current
ceiling", and what lags here is a mount.

**The allocations table renders one bounded PAGE, and says so.** `GET /drives` bounds the grants
read at the list cap and ships `grant_total` — how many allocations EXIST — beside the page it
sends (`internal/api/user_drives.go`), so when `grant_total` exceeds the rows delivered the block
renders the console's SHARED truncation note (`TruncatedNote`,
`ui/src/app/components/wardyn/states.tsx`: the same warning-toned row `runs`, `workspaces` and
`policies` already carry) above the table. **No string is frozen here** — the shared sentence is
used with no override, and the note is warning-toned because the Who search below it filters
CLIENT-SIDE over that window, so past the cap "no matches" would otherwise be a lie about someone
who holds a drive. Real `?offset=` paging for this block is not this round's (R4/F092-a); the note
is the honest statement until it lands.

**The preview has no field label or hint of its own** — it takes the claims a token would
carry, which is what the People step's and the governance preview take, so it renders
`PREVIEW.FIELD_CLAIMS / _HINT` (§7.1). Its footer is `GOV.PREVIEW_NOT_SAVED`; its failure arm is
`GOV.PREVIEW_RESULT_UNKNOWN`. It resolves the claims *as typed* against the allocations and is
not a person lookup; a truncated snapshot surfaces at the member's launch as
`MEMBER.DENIED_STALE_GROUPS`, not here. The answered preview is `PREVIEW_RESULT` over a
`<dl>`: `FIELD_HOME` → the directory name (mono), `PREVIEW_OBJECT_LABEL` → the object name
(mono, with `PREVIEW_OBJECT_HINT` — this is what the offboarding command needs),
`COL_SIZE` → the size, `COL_MODE` → `MODE_RO` / `MODE_RW`, `PREVIEW_ENFORCEMENT_LABEL` → the
`ENFORCEMENT_*` gloss. `{tier}` is one of the three `PREVIEW_TIER_*` words, frozen in the table
rather than in prose (the governance round's `MATCHED_*` addition, learned from).

**The `<dl>` above is FIVE ROWS AND THE ENDPOINT ANSWERS SEVEN FIELDS** — this section describes
what the console renders today, and the two it drops are named here so the gap is a recorded
decision rather than drift. `POST /drives/preview` also composes `home_subject` (WHICH of the
submitted claims the directory name was derived from) and a server-written `warning`
(`drivePreviewWarning`, `internal/api/user_drives_resolve.go:503-522`, whose one sentence is
*"the directory name keys on the sign-in subject; paste it first"*, raised for a `hash`/`sub`
drive previewed with an address pasted first). Both are typed in the TS mirror and rendered by nothing
(`ui/src/app/lib/api/drives.ts`), because a new row is a new surface and a surface arrives
through a mock round — the placement is `CONSOLE-RULES §12`'s to give, not this section's.

Until it does, **`PREVIEW_OBJECT_HINT` is qualified by that omission and not by its own words**:
for the one request shape the `warning` names, the object name shown is well-formed and names an
object no run will ever mount, so copying it into the reclaim command reclaims nothing. That is
also the promise `docs/OPERATIONS.md`'s offboarding step sends an operator here to collect, so
the caveat belongs on both ends of it. The remedy is the admin's and takes no new copy: paste
the sign-in subject first and preview again. `PREVIEW_LEAD`'s *"which drive and directory they
would mount"* is answered for the claims **as typed**, never for the person behind them.

**The Overrides column** renders zero or more chips: `OVERRIDE_SIZE`, `MODE_RO` / `MODE_RW`
(a writable override, in the mode's own chip vocabulary), `OVERRIDE_HOME`, and `PAUSED_CHIP` for
`enabled=false`; `OVERRIDES_NONE` when there are none and the row is enabled.
`HOME_OVERRIDE_NA` replaces `HOME_OVERRIDE_HINT` under a disabled directory-name field when the
subject type is not a user. `ALLOC_REPLACED` is an inline `plain` note after an upsert that
repointed an existing row — not a toast: it is worth reading twice.

### 7.4 Write refusals and states

| Key | String |
|---|---|
| `DELETE_CONFIRM(name)` | Delete "{name}"? It is allocated to nobody, so no run loses a mount. Anything already stored in it stays where it is. |
| `DELETE_RESTRICT_TITLE` | This drive is still allocated |
| `DELETE_RESTRICT_BODY(name, n)` | "{name}" is still allocated to {n} subject / {n} subjects. Remove those allocations first. Deleting the row never deletes data — their directories stay until you reclaim them. |
| `FETCH_FAILED_TITLE` | Couldn't load user drives |
| `FETCH_FAILED_BODY` | Something went wrong reaching the server. Allocations that already exist still bind every run — this list just can't show them right now. |

`DELETE_CONFIRM` is shown only when the drives table reports `ALLOCATED_COUNT` = 0 — otherwise
the dialog opens pre-filled with `DELETE_RESTRICT_TITLE / _BODY` and its confirm disabled
(§2.4); on the race path the console's heading sits over the server's 409 (§7.1), post-attempt,
confirm still enabled. `SAVE_REFUSED_TITLE` (§7.2) heads every 400 the editor can meet, over the
server's text; `SAVE_ERROR` is the unreachable-server arm, not a refusal. Retry on fetch-failed
is `ACCESS_STATE.FETCH_FAILED_RETRY`. There is no "drives not configured" state: with no drives
the feature is empty, not unconfigured, and `EMPTY_TITLE / EMPTY_BODY` say so.

### 7.5 `DRIVES` — the card (setup step and Settings) and the entry points

| Key | String |
|---|---|
| `CARD_LEAD` | Persistent storage people can mount into a run — separate from any workspace. |
| `CARD_EMPTY` | No drives yet. |
| `CARD_DRIVES(n)` | {n} drive / {n} drives |
| `CARD_ALLOCATIONS(n)` | {n} allocation / {n} allocations |
| `CARD_SUMMARY(drives, allocations)` | {drives} · {allocations} |
| `CARD_OPEN` | Manage drives |

One component in two homes (`setup/user-drives-card.tsx`; the Settings shared-component rule).
The card's title is `TITLE`; its body is `CARD_LEAD` over `CARD_SUMMARY(CARD_DRIVES(n),
CARD_ALLOCATIONS(m))` or `CARD_EMPTY`; its action is the two-line link button with `CARD_OPEN` as
its label and the summary as its meta line. It counts **allocations, never people**: a group
allocation is one row and Wardyn holds no directory read, so "14 people" would be a claim, not a
count. The Workspaces header's `outline` button is `TITLE`. The card and the button render for
SUPER only.

### 7.6 `DRIVE_MEMBER` — display moments

| Key | String |
|---|---|
| `NR_CHECKBOX` | Mount my drive |
| `NR_HINT(name, size, mode)` | "{name}" at `/home/agent/drive` — {size}, {mode}. |
| `NR_HINT_NOSIZE(name, mode)` | "{name}" at `/home/agent/drive` — {mode}. |
| `NR_RW_NOTE` | What a run writes there persists to your next run. |
| `NR_RO_NOTE` | A run can read it and never change it. |
| `NR_READONLY_TOGGLE` | Mount read-only for this run |
| `NR_PAUSED` | Your drive is paused by your admin. |
| `NR_DENIED(profile)` | Your governance profile "{profile}" does not allow mounting a drive. |
| `GS_DRIVE_CHIP(name, size, mode)` | Drive · {name}, {size}, {mode} |
| `GS_DRIVE_CHIP_NOSIZE(name, mode)` | Drive · {name}, {mode} |
| `GS_DRIVE_CHIP_PAUSED(name)` | Drive · {name} · Paused |
| `GS_DRIVE_BODY` | Your drive is not a workspace: mount it from New run alongside whatever you attach. It is yours alone — a run sees only your directory. |

`NR_HINT`'s `{mode}` and `GS_DRIVE_CHIP`'s `{mode}` are `MODE_RO_INLINE` / `MODE_RW_INLINE`; the
mode sentence after the hint is `NR_RW_NOTE` when the mount will be writable and `NR_RO_NOTE`
otherwise — including when a writable allocation is narrowed by `NR_READONLY_TOGGLE` for this
run, so the sentence never promises persistence a read-only mount cannot give. `NR_HINT`'s
`{mode}` and the mode sentence both describe the mount this run will get, so both flip when
`NR_READONLY_TOGGLE` is on. `size_mib = 0`
selects the `_NOSIZE` twin rather than rendering `SIZE_NONE` inside a member's sentence.
`NR_READONLY_TOGGLE` renders only when the allocation is writable and defaults **off** (Q5).
The two reason lines render **in place of** the checkbox: an unmountable drive is not a
disabled checkbox with a tooltip, it is one sentence where the checkbox would be. There is no
third: no-allocation-and-no-door is the absent row (§2.5), and the launch-path answer to that
same condition is a SERVER string (`REFUSED_NO_GRANT`, §7.7) — a reply to an attempt, not a
caption on an offer nobody was made. `GS_DRIVE_CHIP`
follows `BARRIER_CHIP`'s `Label · value` shape (Q2) and sits beside `MEMBER.GS_CHIP`, the
governance chip, in the same row. `GS_DRIVE_BODY` renders in the "Add your workspace" card,
after `WORKSPACE_BODY`, only when `/me.user_drive` is non-null.

### 7.7 `DRIVE_MEMBER` — refusals (server-composed)

| Key | String |
|---|---|
| `DENIED_DRIVE(name)` | mounting a user drive is not allowed by your governance profile "{name}". Launch without `drive`. |
| `REFUSED_NO_GRANT` | drive: no user drive is allocated to you — ask an admin for an allocation |
| `REFUSED_PAUSED` | drive: your allocation is paused by an admin |
| `REFUSED_HOME_INVALID(claim)` | drive: your {claim} cannot name a directory (lowercase letters and digits, then `. _ -`, up to 63 characters) — ask an admin to set your directory name |
| `REFUSED_HOME_MISSING(name)` | drive: directory `{name}` does not exist on the share — ask an admin to create it |
| `REFUSED_WRITABLE` | drive: your allocation is read-only; `read_only:false` cannot widen it |
| `REFUSED_BACKEND(reason)` | drive: this deployment cannot mount your drive ({reason}) |
| `REFUSED_TARGET_RESERVED` | workspace_mounts[0]: target `/home/agent/drive` is reserved for the user drive |

**This table is COMPLETE** (§5 #4): every string a member can be refused with at a door this
feature adds is here. It is complete about **doors**, not about the whole path — a drive that
passes every door can still be refused by the RUNNER, and those strings are §7.9. `DENIED_DRIVE` is the one **403** (audited `authz.denied`,
reason `governance_profile`, target `runs.drive` — `denyMemberDrive` beside
`denyMemberRunQuota`); the six `REFUSED_*` are **422s with no audit** (`seedRequestDrive`, run
create and preflight both). **Nothing here names an unprovisioned `k8s_pvc_static` claim**: no
door this feature adds can see that condition — the row is valid and the allocation resolves;
the claim's absence is discovered by the k8s driver at *dispatch* — so a frozen sentence for it
would be a string no code path can emit. `REFUSED_TARGET_RESERVED` is the **400** `validatePolicySpec`'s
unique-target arm raises when a policy or workspace source names the reserved target — it is
met by whoever writes the policy, member or admin, and it belongs here because it is a door
this feature adds. Its `[0]` is the **mount's position**, not a literal: every mount error is
prefixed `workspace_mounts[i]` (`workspace_repos[i]` for a repo), and the frozen string spells
the first mount's, so the canon equals the server's bytes. `{reason}` in `REFUSED_BACKEND` is **`driveMountFor`'s own prose**
(`internal/api/user_drives_run.go`): the backend/runner mismatch — *it is a "{backend}" drive and
this deployment dispatches to "{target}"* — or, for a share, `driveShareIsBindable`'s host-root
arm, *drive "{name}" is on a share this deployment does not allow — ask an admin*. That arm is
**path-free by rule**: `UserDriveHostRootCheck`'s own error spells the drive's `host_root` and the
whole `WARDYN_USER_DRIVE_HOST_ROOTS` list, and this body is read by a MEMBER, so the diagnosis
goes to `slog` for the operator and the member gets the drive's name and who to ask — the same
line `REFUSED_HOME_MISSING`, `applyUserDriveEnv` and the `run.drive.mount` target already hold.
It is **not** an apiserver refusal; the console never asks the cluster and nothing on this
path relays one. `{claim}` in `REFUSED_HOME_INVALID` is
the template's claim name (`sub`, `email_local`). `MEMBER.DENIED_STALE_GROUPS` (§7.1)
is reused verbatim for the truncated-snapshot case and is not re-frozen.

### 7.8 `DRIVE_RUN` — the rail line (frozen here, shipped with run-row persistence)

| Key | String |
|---|---|
| `RAIL_LABEL` | Drive |
| `RAIL_VALUE(name, mode)` | "{name}" · {mode} |

One `<dt>`/`<dd>` pair in `IdentityWidget`'s `<dl>`, between Sandbox and Started, rendered
**only when the run row carries a drive** — which it does not in v1 (§2.7). `{mode}` is `MODE_RO`
/ `MODE_RW`. The name is quoted, never mono; the `dd`'s `title` carries the object name for the
admin who hovers, and nothing else on the run page names it.

### 7.9 `DRIVE_DISPATCH` — refusals and log lines the RUNNER composes

The four refusals in §7.7 are the **doors** — `422`/`403` bodies this feature's own handlers
write. These are the **dispatch** half: a drive that passed every door and was refused by the
runner, at the last moment before the sandbox is created. They reach a human as the run's
`failure_hint`, **verbatim**, so the canon owns them the same way it owns a `writeError` body.
Same audience rule as §7.7 and for the same reason — the reader is whoever launched the run —
so **not one of them names a path on the operator's filesystem**: the drive, the directory and
the reason, and the paths go to the log rows below instead.

| Key | String |
|---|---|
| `DISPATCH_SOURCE_REFUSED(name, home)` | user drive: drive "{name}", directory "{home}" cannot be bound on this host: the directory this deployment resolved for it is not one a drive may bind here — wardynd's log names the rule that refused it, and an operator can fix it |
| `DISPATCH_SIBLING_NAME(name, home)` | user drive: drive "{name}", directory "{home}" resolves to a directory named after somebody else — a home replaced by a link to a sibling would bind another person's directory |
| `DISPATCH_TARGET_RESERVED(name, home, target)` | docker: denied user drive (drive "{name}", directory "{home}") -> "{target}": a user drive may bind only at the reserved drive path (`/home/agent/drive`) |
| `DISPATCH_BACKEND_UNSUPPORTED(name, home, backend)` | docker: user drive (drive "{name}", directory "{home}") has backend "{backend}", which this runner cannot mount (…) |

The first two are wrapped by the driver at the call site in the frozen shape `docker: denied
user drive mount -> "{target}": …`, which is **kept** — the target is the member's own sandbox
path and is theirs to read. `drive "{name}", directory "{home}"` is one subject rendered by a
single helper; a mount that carries no drive name degrades to `directory "{home}"` and one that
carries no drive at all to `this drive` — the fallback for "I cannot name the drive" is never
"then name the path". The `(…)` on `DISPATCH_BACKEND_UNSUPPORTED` is the runner's own list of
the backends it does mount. These are **wire** strings, not console copy: nothing here is a
`{mode}`, a `{size}` or a mono span the console applies, and the drive name is quoted here
because the string ships with the quotes in it.

**Operator log lines — the other half of the same split.** Frozen here because they are what an
admin is told to go and read whenever a member-facing string above declines to say more; a
member never sees one.

| Key | Line |
|---|---|
| `LOG_BIND_REFUSED` | wardyn: user drive: this share mount was refused at bind time — attrs `source`, `real_path`, `host_root`, `drive`, `home` |
| `LOG_NO_RRO` | wardyn: user drive: this runtime does not support recursively read-only binds, so a submount under the share's home could be writable inside the sandbox — attrs `drive`, `home`, `mount_option` = `rro` |
| `LOG_CEILINGS_OVERLAP` | boot WARN, prefix `wardynd: mount ceilings overlap — ` over three variants: *…both name "{p}": a member can onboard that directory as a workspace and bind the WHOLE share, every other person's home included, without a drive allocation…*; *…contains "{m}", which holds the WARDYN_USER_DRIVE_HOST_ROOTS entry "{d}"…*; *…contains "{m}", which is INSIDE the WARDYN_USER_DRIVE_HOST_ROOTS entry "{d}": member workspaces would be authored inside a share whose directories Wardyn hands out one person at a time…* |

`LOG_BIND_REFUSED` is the operator half of `DISPATCH_SOURCE_REFUSED` and `DISPATCH_SIBLING_NAME`
— one refusal, two audiences, written in one place so the two cannot drift, and the direction
they would drift in is disclosure. `LOG_CEILINGS_OVERLAP` is a **warning and not a refusal**:
the pair it describes may be a posture the operator chose, and a boot refusal would take a
running deployment down on upgrade. [OPERATIONS.md](../OPERATIONS.md) "User drives on Docker"
carries all three with the operator recipe.

## 8. Where to apply (once implemented, out of scope this round)

- **`/drives`** — the whole screen: drives block, allocations block, preview (§7.2–§7.4), one
  file per seam under `screens/drives/`; `lib/api/drives.ts`; `lib/user-drives-copy.ts` + test.
- **`/governance` profile editor** — the third `LimitRow` (`GOV.LIMIT_DRIVE_*`, §7.1) and its
  chip in the profiles table; `governance-copy.ts` gains the two keys, its test count 87 → 89.
- **Workspaces header** — the `outline` `TITLE` button in `PageHeader`'s `actions` (SUPER).
- **Setup `workspaces` step + Settings** — `setup/user-drives-card.tsx` (§7.5), rendered in
  `WorkspacesStep` under the list and as Settings' fifth card.
- **New Run Workspace card** — `new-run/workspace-card.tsx` (extracted), the checkbox, hint,
  mode sentence, toggle and reason line (§7.6); `wizard-spec.ts` emits `run.drive`.
- **Member Getting Started** — the chip and the sentence (§7.6); `lib/api/health.ts`'s `/me`
  type gains `user_drive`.
- **Run create + preflight** — the eight server strings (§7.7) beside the gates that raise them;
  `/me.user_drive.denied_by_profile` (Q6).
- **Run rail** — `RAIL_*` (§7.8), with run-row persistence in 0.7.1.
- **`docs/OPERATIONS.md` known gaps, `docs/MEMBERS.md` "Your drive"** — `HONESTY` verbatim; the
  member section says where it mounts, read-only default, yours alone, and that the size shown
  is an allocation.

## 9. Owner question list

**Decided by the rules above, drawn for the record (no answer needed):** Q1 — `HONESTY` renders
once under the drives table (§5 #1; `STORAGE_CLASS_HINT` carries the typing-time clause). Q2 —
the chip is `Drive · {name}, {size}, {mode}` (the `Label · value` shape every neighbouring chip
has). Q3 — the editor offers only this runner's two backends (a fifth string for an avoidable
state would be filler). **Open for the owner: Q4–Q7.**

**Q1.** Where `HONESTY` renders on `/drives`: **(a) once, as the plain note under the drives
table**, with the per-size `ENFORCEMENT_*` gloss carrying it onto every number; **(b) under the
Size field in the editor**, at the moment the admin types the number; **(c) both.**
**Recommend (a).** Five sentences under a numeric input is the "no filler" rule's target, and the
editor's Size field already carries `SIZE_HINT` plus, for Kubernetes, `STORAGE_CLASS_HINT`'s
"a block storage class enforces the size; a network-share provisioner does not" — the one clause
that matters while typing. (b) is drawn as the losing variant.

**Q2.** The member's Getting Started chip: **(a) `Drive · {name}, {size}, {mode}`** — the
`Label · value` shape every chip in that row already has (`BARRIER_CHIP`, `GS_CHIP`); **(b)
`Your drive — "{name}", {size}`**, `DESIGN.md` §4.3's draft, which quotes the name and drops the
mode. **Recommend (a).** A chip row with one em-dash chip reads as a different component, and
the mode is the fact a member most needs before their first run — it is what decides whether
anything they write survives. (b) is drawn as the losing variant.

**Q3.** The editor's Backend field: **(a) offer only this runner's two backends**, with the
Docker `host_path` option disabled and explained when no roots are set; **(b) offer all four,
the other runner's two disabled** with a "not this runner" reason. **Recommend (a).** Progressive
disclosure — an admin on Docker never needs to read about storage classes — and the
backend/runner 400 stays on the API path, where `wardyn drive apply` (0.7.1) will meet it. (b)
would need a fifth frozen string for a state the console can always avoid.

**Q4.** The profile editor's Limits lead — "Two launch modes route around tool approvals
entirely, so the ceiling above cannot reach them. Deny them here instead." — now heads three
rows, and the third is not a launch mode. **(a) Leave it this round**; the door's own hint carries
the distinction. **(b) Reword it** in an owner-gated copy change ("Some of what a run can do
routes around the ceiling entirely. Deny it here instead."). **Recommend (b), as a separate
copy change** — this round appends rows and rewords nothing (the brief's boundary); the reword
touches a string `governance.spec.ts` may assert on and belongs to the door-only 0.7 slice.

**Q5.** `NR_READONLY_TOGGLE`'s default on a writable allocation: **(a) off** — the admin decided
writable, and the member narrows only when they choose to; **(b) on** — every run mounts
read-only unless the member opts into writing. **Recommend (a).** Read-only-by-default is
already the *allocation's* default (`WRITABLE_HINT`); making the toggle default on would have a
writable allocation behave read-only until a second click on every run, and the member would
learn to click through it. The mode sentence flips honestly either way.

**Q6.** The door's client-side bit: **(a) `/me.user_drive.denied_by_profile`** — inside the
drive object; **(a′) a sibling `/me.user_drive_denied_by_profile`** (the profile name, or empty)
— keeping `user_drive` strictly nil-means-no-allocation, since the door and the allocation are
independent and a denied member with no allocation still needs the line; **(b) `GET
/policies/default.governance_limits`** — the whole limits struct beside
`governance_profile_name`. **Recommend (a′).** The New Run card already fetches `/me` for the
drive and needs one more field, not a second struct on a policy endpoint; and (a) says exactly
what the console renders — the name to interpolate — while (b) would leak two limits the card has
no line for. Either way the 403 stays authoritative post-attempt and is drawn.

**Q7.** A `k8s_pvc` drive with `size_mib = 0`: **(a) a new 400** — "size_mib must be above 0
for a k8s_pvc drive — it is the volume request" — and the editor shows `SIZE_HINT_REQUIRED`;
**(b) clamp 0 to the storage class's minimum** and keep 0 legal everywhere as `DESIGN.md` §2.1
says. **Recommend (a).** A claim that requests zero is refused by the apiserver anyway; refusing
at the write boundary says so in the admin's own screen instead of at a member's run. This is a
fifth refusal beyond `DESIGN.md` §2.6's four and needs your word.

## Adjudication

### Owner answers

Owner, 2026-09-01 (this session): **mock approved as drawn.** Q4 → **(b) reword**, applied in this
round as an owner-gated copy change (`GOV.LIMITS_LEAD` in `governance-prompt.md` §7.2 and
`governance-copy.ts`; `governance.spec.ts` updated if it asserts the lead). Q5 → (a) off. Q6 →
(a′) sibling `/me.user_drive_denied_by_profile`. Q7 → **(a) the new 400**; `SIZE_HINT_REQUIRED`
stands. Q1–Q3 decided by rule.

### Round notes (author, 2026-09-01)

0. **Coordinator, after the Opus copy review:** the `email` home template is gone (a full
   address can never pass the segment rule, and a PVC name must be DNS-1123) — `hash`, `sub`,
   `email_local` remain; `email_local` plus a per-user directory override cover a corporate home
   named by username. Namespaces renamed `DRIVE_MEMBER` / `DRIVE_RUN` and the Getting Started
   keys `GS_DRIVE_*` so nothing shadows `governance-copy.ts`'s `MEMBER` (§5 #11). Q7 added.

Departures from `DESIGN.md` §4.3's draft tables, each with the reason; the model is unchanged.

1. **Two drives on one runner.** The brief asked state 2 to show a `host_path` share beside a
   `k8s_pvc` managed drive. A backend must match the deployment's runner (`DESIGN.md` §2.3), so
   those two cannot coexist. State 2 draws a Kubernetes deployment (`k8s_pvc_static` share,
   read-only, beside `k8s_pvc` managed, writable) and a scaffold-labelled Docker variant of the
   same table (`host_path` + `docker_volume`) so all four backend chips are reviewable.
2. **The run rail line has no data in v1.** `IdentityWidget` reads the run row, and
   THREAT-MODEL §4.6 ("concurrent runs share one drive") puts drive persistence on the run
   row in 0.7.1. The line is frozen (§7.8) under "canon frozen here, shipped later", the
   governance round's own device, and drawn as such.
3. **`NR_DENIED` needs a wire bit `DESIGN.md` §5.1 does not carry.** `GET /policies/default`
   ships the profile name only; the door is knowable client-side only with a `/me` field — a
   sibling `user_drive_denied_by_profile` so `user_drive` stays nil-means-no-allocation (Q6 a′). Drawn pre-filled on that assumption, and post-attempt
   from the 403 regardless.
4. **Backend-unavailable on Kubernetes is post-attempt only.** `DESIGN.md` §4.2 says "previewed
   as a warning", but `POST /drives/preview`'s payload (§2.5) has no warning field and the
   console cannot ask the apiserver. Not drawn on the preview; drawn as the member's
   `REFUSED_BACKEND` 422 and named here so P2 either adds the field or drops the sentence.
5. **`k8s_pvc` with `size_mib = 0` has no request to make.** `DESIGN.md` allows 0 everywhere;
   a zero PVC request is invalid. Listed as a server 400 in §7.1's second table and raised as
   **Q7** (a new refusal the owner did not accept in `DESIGN.md` §2.6); the editor renders
   `SIZE_HINT_REQUIRED` for that backend until the owner decides.
6. **The chip shape** (Q2) and **`NR_HINT`'s split** — the draft's "What a run writes there
   persists to your next run" is false for a read-only mount, so it became `NR_RW_NOTE` with an
   `NR_RO_NOTE` twin, and the `_NOSIZE` twins keep `SIZE_NONE` out of a member's sentence.
7. **`GS_DRIVE_BODY` reworded** to say what it is placed in the Workspace card to say: a drive is not
   a workspace.
8. **`DELETE_RESTRICT_BODY` reworded** — the draft said "never widens", governance vocabulary
   with no meaning for a drive; it now says what deleting the row does not do (delete data).
9. **`SIGNIN_NOTE` and `EFFECT_NOTE` frozen rather than reused** — the governance strings end on
   a ceiling; these end on a mount. **`PRECEDENCE` is duplicated deliberately**: it differs from
   governance-prompt §7.3's only by "allocation"/"drive name", and the two must be edited
   together (a parameterised canon would touch the shipped module mid-hardening).
10. **The card counts allocations, not people.** The draft's "2 drives · 14 people" cannot be
    computed under a group allocation without a directory read Wardyn does not hold.
11. **Door keys are `LIMIT_DRIVE_LABEL` / `LIMIT_DRIVE_HINT`** (the draft's names, and the
    existing `LIMIT_EXEC_* / LIMIT_INTERACTIVE_*` pattern), label "Deny mounting a user drive" as
    drafted — the brief's paraphrase "Deny user drives" would have been a third naming shape.
12. **The door's two keys landed with this round** — `governance-copy.ts` carries
    `GOVERNANCE.LIMIT_DRIVE_LABEL` / `LIMIT_DRIVE_HINT` and `governance-copy.test.ts` asserts
    `doc.size` 89; the suite is green.
13. **The Limits lead is left as shipped** (Q4) — this round rewords no existing string.

Not drawn, deliberately: a Reset control, a usage meter, a reclaim button, a nav item, a
fourth `OptionCard`, a collision warning, a share-credential field (§3).
