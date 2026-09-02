# Governance profiles — the acting surface for who runs under which ceiling

This is the mock round for the 0.7 governance surfaces — the design gate before any console
code (owner law: the mock is UI source of truth; canon strings are app strings). The model
below is decided and converged in the active plan; nothing here is open for re-design, only
for drawing. **Seven** open calls remain and are listed as Q1–Q7 in §9, with an empty
Adjudication section at the end for owner answers.

One round covers all three viewer surfaces at once (the precedent: one prompt + one static
mock, frozen strings byte-exact, Q-numbered owner calls, empty Adjudication):

1. the **security admin's** Governance console (a new screen),
2. the **member's** inline moments (no new screen — three existing surfaces gain a line), and
3. **canon frozen here, shipped later** — the positioning slogan (§H) and the directory
   combobox (§I), both of which land in phases after this one but whose strings must exist
   before the code that renders them.

Static mock: `docs/design/governance-mock/index.html` (open it in a browser).
Frozen strings: §7 below. No TS copy module exists yet — this is a mock round, the same way
permissioning's A-A stage preceded `ui/src/app/lib/permissions-copy.ts` and the People round
preceded `ui/src/app/lib/people-access-copy.ts`. The implementation stage creates
`ui/src/app/lib/governance-copy.ts` **from §7 verbatim**; it does not retype copy from this
document, and every string in the mock HTML matches §7 byte-for-byte.

---

## 1. What exists today (the thing being grown)

This section describes the base this round grows from. **The symbols are the durable half; the
line numbers are not.** They were checked once against this worktree, which is shared with the
enforcement lanes actively editing these same files, so a given number may already have drifted
by the time you read it — resolve the symbol, re-locate the line at implementation. Where a
site has no stable line to point at, only the symbol is given.

**One ceiling, deployment-wide.** `Config.DefaultPolicy` (env-borne, `WARDYN_DEFAULT_POLICY`)
is the single member ceiling. A member's inline policy is clamped to it
(`internal/api/inline_policy.go:95`), their eligible grants are filtered against it
(`filterMemberGrants`, same file), a run created with no policy at all gets a clone of it
(`resolvePolicy`'s no-policy branch, `internal/api/runs_policy.go`), Recording-Mode synthesis is clamped to it
(`internal/api/profile.go:122`), and `handleGetDefaultPolicy`
(`internal/api/policies.go:97-98`) serves what its own comment calls "the same ceiling a
member's inline policy" is bounded against (`:93`). There is no per-user or per-group ceiling
anywhere: differentiation today is only capability grants layered on that one floor.

**Saved policies are readable and selectable by everyone.** `GET /policies`,
`GET /policies/default` and `GET /policies/{id}` are registered on plain `r`, not
`operatorOnly` (`internal/api/routes.go:230,233,234` — only POST/PUT/DELETE at
`:229,235,236` are operator-gated), and the stored branch of `resolveRunPolicy` never clamps.
The console makes it one click: PolicyPanel's "Reuse a saved policy" card
(`ui/src/app/components/wardyn/policy-panel.tsx:700`) over an unfiltered picker
(`ui/src/app/components/screens/new-run/new-run-screen.tsx:736-765`). This is the escape the
enforcement spine closes; it is stated here because it is the reason the profiles feature is
not merely additive.

**Sessions are stateless, and this bounds what any preview can know.** The session is a
signed HttpOnly cookie carrying sub, email, role and expiry — "Sessions live entirely in a
signed HttpOnly SameSite=Lax cookie" (`internal/auth/oidc/oidc.go:22-24`) — with **no
server-side session table at all**: revoking a human stamps a cutoff rather than deleting a
row, precisely because "there is no session row to delete"
(`internal/api/sessions.go:23-25`, `oidc.go:135-137`). A member's group snapshot therefore
rides *their* cookie and is unreadable by anyone else; the only group snapshot Wardyn
persists is `api_tokens.groups`, stamped at mint. Nothing server-side can answer "what groups
does Alice have right now", which is what makes the resolved preview a **claims** dry run
(§2.5, Q2) rather than a person lookup.

**`/permissions` — the org allow/denylist, already built.** `PermissionsScreen`
(`ui/src/app/components/screens/permissions.tsx:159`) renders capability grants over a
subject-type model this round reuses wholesale: the exported `Segmented` picker
(`permissions.tsx:123` — buttons with `aria-pressed`, already reused by the People
step's role picker), a mono value input, and the frozen subject vocabulary in
`ui/src/app/lib/permissions-copy.ts` (`SUBJECT_USER`/`SUBJECT_GROUP`/`SUBJECT_ALL` at
`:151-153`, `HINT_USER`/`HINT_GROUP`/`HINT_ALL` at `:167-169`, `COL_WHO`/`FIELD_WHO`,
`PRECEDENCE`). Subjects resolve through the same `capabilitySubjects` the new resolver will
read.

**`/approvals` — escalation, already built.** `ApprovalsScreen`
(`ui/src/app/components/screens/approvals.tsx`) is where a member asks for a host and an
operator decides. Nothing in this round adds an approvals surface.

**`/policies` — the ceiling, read-only for members.** `PoliciesScreen`
(`ui/src/app/components/screens/policies.tsx:130-142`) already discloses the default ceiling
in a `<details>` labelled "Default (ceiling) policy — applied to runs with no policy_id, and
to every member's inline policy" (`:134`). `PolicyPanel` (`policy-panel.tsx:6-27`) is ONE
spec-JSON authoring surface with two instances — `instance="policies"` (the admin editor body)
and `instance="run"` (the New Run card) — carrying the template row, the JSON editor, the
field help list, and the `SafetyMeter`'s debounced `POST /policies/grade` (`:796`).

**The New Run policy step.** `SectionCard title="Policy"`
(`new-run-screen.tsx:726`) holds the panel. It has **no line naming the ceiling today** — the
only ceiling-adjacent text is the preflight hint, "Checks the spec server-side and shows what
would be clamped — before you launch." (`policy-panel.tsx:870`). The ceiling-context line
this round freezes (§7.6) is therefore a NEW line, not an edit of an existing one.

**Member Getting Started.** `MemberGettingStarted`
(`ui/src/app/components/screens/onboarding/member-getting-started.tsx:156`) renders a
"What's set up for you" `SectionCard` whose body is a row of chips — `BARRIER_CHIP`,
`MODEL_ACCESS_OWN_CHIP` / `MODEL_ACCESS_PROVIDED_CHIP`, `SIGNIN_SSO_CHIP` — over
`SETUP_SUMMARY_HELPER`, all frozen in `ui/src/app/components/wardyn/copy.ts:555-586`. The
page's subtitle already says "You're a member of this Wardyn. Your admin set the ceiling; you
run inside it." (`copy.ts:557`).

**The `/access` surface, merged from the SSO campaign.** `AccessPanel`
(`ui/src/app/components/screens/setup/access-panel.tsx`) is the People step's role-mappings
editor: a merged chart+console table, an add form whose role picker is that same `Segmented`
with exactly two options today (`:522-528`, `PEOPLE.ROLE_ADMIN` / `PEOPLE.ROLE_MEMBER`), and
a **preview panel** (`:574` onward) that dry-runs a sign-in — paste claims, or "test with my
own session" — via `POST /access/preview`, saving nothing. Its wire type is
`AccessRole = "admin" | "member"` (`ui/src/app/lib/types/access.ts:10`) and its copy is
`ui/src/app/lib/people-access-copy.ts`. Every "who" field on it is free text: Wardyn holds
zero directory read today.

**Navigation.** `NAV_ITEMS` (`ui/src/app/components/screens/app-shell.tsx:190`) is Runs ·
Approvals · Workspaces · **Policies · Permissions** · Secrets · Audit · Recordings, with the
comment at `:200-201` recording that Permissioning "sits beside Policies: both answer "what is
allowed here", one for runs and one for the humans launching them."
`MEMBER_NAV_PATHS` (`:225`) narrows a member to Runs · Approvals · Workspaces, and
`navItemsForRole` (`:226`) keys on `role !== "member"`.

**What a member is refused today, and how.** Request-level refusals are 403s composed
server-side by `denyMemberRequest` (`internal/api/runs_create_validate.go:298`, three
fields: `devcontainer_repo`, `image`, `workspace_id`); the two closed-enum refusals this round
extends are the codex-cli hold refusal (`:153`) and `interactiveToolApprovalsError`
(`:252`). Clamp/capability warnings ride the 201 as `warnings[]`
(`internal/api/runs.go:205`) and surface as one sonner toast per message titled "Run launched
with a warning" (`ui/src/app/components/screens/new-run/run-warnings.ts`) — **the server
composes that text and the console never rewords it**, which is why §7.7's member strings are
written as server messages, not console labels.

### 1.1 What this round REUSES rather than builds

Stated so the scope is honest, per the plan's §C.2:

- **The per-group allow/denylist is `/permissions`, unchanged** — capability grants with
  `all`/group deny rows already are that primitive. The tier work hands that screen to the
  security admin; this round adds no second grant surface.
- **Escalation approvals is `/approvals`, unchanged** — after the tier work's two predicate
  swaps a security admin sees and decides everyone's. No new approvals surface.
- **The ceiling editor is `PolicyPanel instance="policies"`, unchanged** — the profile editor
  embeds the shipped spec editor rather than growing a second one.
- **The subject picker is `Segmented` from `permissions.tsx`, unchanged** — including the
  red-deny styling branch, which keys on an option literally valued `deny`. The subject-type
  picker has no such option, so it renders neutral: the display law is inherited by reuse, not
  re-implemented (see §4).

## 2. The model this round mocks

### 2.1 A governance profile

A **profile** is a named ceiling: `{name, ceiling, limits}` where `ceiling` is a
`RunPolicySpec` (the same document `PolicyPanel` already edits and `POST /policies/grade`
already grades) and `limits` is two booleans. It is stored in its own table, never as a saved
`run_policies` row — saved policies are selectable *content*, and making ceilings selectable
would reproduce the conflation this campaign removes.

An assigned profile **replaces** the deployment ceiling for its subjects; it is never composed
with it. No assignment ⇒ `DefaultPolicy`, byte-for-byte today's behavior. That is the whole
composition rule, and the console says it in `LEAD` (§7.2).

### 2.2 Assignment and precedence

An **assignment** binds one subject to one profile: `{subject_type, subject, profile_id,
priority}`, unique per subject, with the same three subject types capability grants already
use (`user` / `group` / `all`). Exactly one profile applies to a principal.

The precedence rule, rendered on the page as `PRECEDENCE` (§7.3) and answered concretely by
the resolved preview (which resolves **typed claims**, never a live person — §1's
stateless-session fact is what forces that):

- **user beats group beats everyone**;
- within the user tier, a **sign-in subject** match beats an **email** match (a person can
  present as both, and the subject is the stable identifier — the email is reassignable);
- between groups, **higher priority wins**, then the profile **name**, so the order is total
  and the same query always returns the same answer.

`priority` is meaningful only inside the group tier — a user-tier or everyone-tier row shows
`PRIORITY_NA`.

### 2.3 The two limits

Two launch modes route *around* tool approvals entirely, so a ceiling cannot reach them:

- **`task_mode=exec`** — a command with no agent and therefore no tool gate at all.
- **interactive** — supervised by a human at the attach pane, not by rules; an interactive run
  refuses `tool_approvals=hold` by design, and a request with **no task coerces to
  interactive**, which is why the profile's interactive limit is evaluated *after* that
  coercion and its refusal string names the no-task case out loud (§7.7).

`limits` therefore carries exactly these two booleans, drawn as two switches, and nothing else.

### 2.4 What the console refuses, and what it only warns about

Three write outcomes are drawn, because each is a different kind of "no":

- **Grants that go past the deployment ceiling — REFUSED (400).** A profile's eligible grants
  must be a per-grant subset of `DefaultPolicy`'s: the pairing must exist there, approval may
  be forced on but never stripped, the lifetime may be shortened but never raised, and a
  `github_token` grant's repositories and permissions must be covered. Grants are material
  whoever deployed Wardyn provisioned; a profile may withhold one, never mint one. Four
  distinct causes, one body — §7.4.
- **Deleting a profile that is still assigned — REFUSED (409).** Deleting it would silently
  widen its members back to the deployment ceiling, so the assignments come off first. The
  assignment count is **client-visible** in the profiles list, so the console does not make
  the operator discover this by failing: at `ASSIGNED_COUNT > 0` the delete dialog opens
  **pre-filled** with the refusal and its confirm disabled. The 409 stays authoritative for
  the race the count cannot see (another admin assigning it a second ago), and that path is
  drawn too — post-attempt, confirm still enabled, exactly the People-step lockout shape.
- **A profile that omits something the deployment ceiling carries — WARNED, never refused.**
  Narrowing by omission is exactly what a profile is for; the console says what was dropped
  rather than blocking the save. Presentation is **Q6**.

### 2.5 The member's three inline moments

The 0.6 doctrine holds: **no member governance screen, inline moments only.** Member nav is
unchanged.

- **New Run, Policy card** — a ceiling-context line naming the profile that binds this run
  (`CEILING_PROFILE`, §7.6). Rendered **only when a profile is assigned**; with no assignment
  the card is byte-for-byte today's, matching the absent-row doctrine everywhere else in this
  campaign.
- **Getting Started, "What's set up for you"** — a governance row beside the existing barrier
  / model-access / sign-in chips, **naming the profile** (decided in the plan, not an open
  call: the member already reads the name on every New Run and in every refusal, so a
  "managed" placeholder here would only make two screens disagree).
- **Refusals and warnings** — the seven server messages in §7.7. They are the only place a
  member meets a limit, and each names the profile so "why can't I" has an answer on the first
  read.

Both display moments are fed by one additive field on `GET /policies/default` — the endpoint
that already resolves through the ceiling — so no new member endpoint exists.

### 2.6 States

- **Populated** — profiles with assignments, the resolved preview answered.
- **Empty (no profiles yet)** — the deployment ceiling still binds everyone; the empty state
  carries the action that fills it.
- **Empty (profiles, no assignments)** — distinct and worth its own words: a profile with no
  assignment bounds nobody.
- **Fetch-failed** — distinct from empty. The profiles already assigned keep binding every run;
  this list just cannot show them.
- **Editor open** — the add-assignment form collapses (below).
- **Write refusals** — the two 4xx states in §2.4. The grant-bound refusal is drawn
  post-attempt (nothing client-side can compare a profile's grants to an env-borne ceiling);
  the delete refusal is drawn both pre-filled and post-attempt, per §2.4.
- **Omission warning** — the non-blocking third outcome.

**One teal button at a time.** The profile editor opens in place, and its Save profile is the
surface's `default` button while it is open — so the add-assignment form below it **collapses
to its disabled `ADD_TITLE` summary row** for exactly that span, taking its teal Assign with
it. This is a disabled-state change on a form that is already on the page, not a new dialog or
a new surface: `CONSOLE-RULES.md` §6's one-default rule is kept without inventing a modal, and
§7's "a busy control is disabled, not removed" is the shape it borrows.

### 2.7 Canon frozen here, shipped later

Two bodies of copy are frozen in this round although their code lands in later phases. They
are here because both replace or extend strings the console already renders, and a copy change
is called out as one (`CONSOLE-RULES.md` §10):

- **The positioning slogan (§7.8).** "Run anything. Keep your keys." is replaced by
  **"Sandboxed. Governed. Self-hosted. Free."**, and the setup layout's long-form variant
  becomes **"Governed sandboxes for anything you run — on your own infrastructure, free."**
  Both are decided; this round only freezes and draws them. The released demo videos narrate
  the old line and **lag** the string change until the owner's next re-record cycle — that is
  recorded, and it does not block the swap.
- **The directory combobox and the third role (§7.9).** The assignment subject field and the
  People step's mapping value become a `DirectoryCombobox` when a directory connector is
  configured, and stay a plain free-text input when it is not. Three behaviors are frozen: a
  suggestion row is `DisplayName — detail`; picking a **group** shows its NAME in a chip while
  the stored value is the group's **object id**; a failing lookup shows a "couldn't check"
  note and typing still works. **Absent mode is silence** — no error, no banner, no disabled
  state: the combobox IS the plain input, and no string exists for it. The access-panel role
  picker's third option is labelled **"Security admin"**.

## 3. Do not design (out of scope this round)

- **No member governance screen.** Inline moments only; `MEMBER_NAV_PATHS` is unchanged.
- **No second grant surface.** Per-group allow/denylists are `/permissions`, unchanged (§1.1).
- **No approvals surface.** Escalation is `/approvals`, unchanged.
- **No second spec editor.** The ceiling is `PolicyPanel instance="policies"` embedded; do not
  redraw its fields, its templates, its help list, or its meter.
- **No change to `/policies`.** The saved-policy list and the default-ceiling disclosure stay
  exactly as they are; the clamp that binds a selected saved policy is enforcement, not UI.
- **No role-map editing.** The People step's table is the SSO campaign's surface; this round
  touches exactly one string on it — the role picker's third option label.
- **No directory connector configuration UI.** The connector is env-configured and opt-in;
  this round draws only the field's two modes.
- **No re-record of the demo videos.** Owner-gated, and it does not block the string swap.

## 4. Design system

Same token block and CSS idioms as `docs/design/people-access-mock/index.html` (`--background`
/ `--card` / `--surface-2` / `--foreground` / `--muted-foreground` / `--border` /
`--border-strong` / `--primary` / `--success` / `--warning` / `--danger` / `--info`, light and
dark, Inter + JetBrains Mono, `chip`/`btn`/`sw`/`seg`/`dlg`/`note`/`card` idioms).

Unlike the People round, **teal is in budget here** — the Governance console is a full screen,
not a setup step, so `CONSOLE-RULES.md` §2's normal rule applies: exactly **one** `default`
(teal) button per surface, and the screen enforces it over time rather than by counting forms.
**Assign** is the screen's teal button at rest. When the profile editor opens in place its
**Save profile** takes that role, and the add-assignment form collapses to its disabled
`ADD_TITLE` summary row for exactly that span — so the two teal buttons can never co-occur
(§2.6; drawn in the mock's editor state). Everything else is `outline` or `ghost`; Delete is
`destructive` with a confirmation dialog; **Remove** (an assignment) is `outline`, because
unassigning is reversible.

Colour, stated per rule:

- **Assignments are not grants.** The amber-allow / red-deny law belongs to `/permissions`,
  where an effect column exists. An assignment has no effect column, and the reused `Segmented`
  colours red only an option literally valued `deny` — the subject-type picker has none, so it
  renders neutral. Profile names, subject types and priorities are all neutral.
- **Amber and red carry genuine risk and error only**: the two write refusals, the omission
  warning, the member refusals, the fetch-failed state — `CONSOLE-RULES.md` §2's general
  state-colour convention, unrelated to and unaffected by the grants-specific amber rule.
- **The grade is semantic, not metal** — the embedded `SafetyMeter` keeps its own
  success/warning/danger scale; nothing in this round recolours it.
- **Mono is for literals only**: subject values, group object ids, env var names, wire values
  like `task_mode=exec`. Never for a profile name in prose — a profile name is quoted, not
  mono, because it is a human-chosen label (see the §7 header rule).

## 5. Hard canon constraints

1. **The two §H strings are frozen here, and only here** — "Sandboxed. Governed. Self-hosted.
   Free." and "Governed sandboxes for anything you run — on your own infrastructure, free."
   (§7.8). They are a copy change under `CONSOLE-RULES.md` §10 and are called out as one; they
   are not slipped in as a docs edit.
2. **The §I `DirectoryCombobox` strings and the "Security admin" label are frozen here** even
   though their code lands in later phases (§7.9). The canon exists before the code that
   renders it — that is the whole point of the mock-first law.
3. **`MEMBER_NAV_PATHS` is unchanged.** Runs · Approvals · Workspaces. A member never sees a
   Governance item, and there is no member governance route.
4. **`navItemsForRole` needs no code change.** It already keys on `role !== "member"`, so a
   security admin gets the full nav for free; the Governance item is gated client-side by the
   security-operator predicate and server-side by the security-ops route group.
5. **Every string a member is refused with is composed server-side** and rendered verbatim
   (`run-warnings.ts`'s rule). §7.7 is therefore a table of *server* strings, in the in-tree
   refusal shape, not console labels.
6. **The subject vocabulary is `permissions-copy.ts`'s**, not a second copy of it (§7.1). A
   governance assignment and a capability grant must never disagree about what "Everyone signed
   in" means.

**Assertion sites this round's copy is already pinned to** — a change here that is not
reflected in these is a broken test, not a free edit:

- `ui/src/app/components/screens/onboarding/onboarding-screen.tsx:211` — the hero string, plus
  its three assertions at `onboarding-screen.test.tsx:46,141,146`.
- `ui/e2e/demo/walkthrough.spec.ts:89` (`getByRole("heading", …, level: 1)`) and `:647`
  (caption), `ui/e2e/demo/02-set-up-the-host.spec.ts:187,200` (same pair) — the hero as both a
  heading and a narrated caption.
- `ui/e2e/demo/12-audit-and-attach.spec.ts:467` — `caption(page, "Keep your keys.")`, a
  **fragment** of the old slogan; it moves with the string even though it never spelled the
  whole line.
- Comment-level references that must not be left describing a string that no longer exists:
  `ui/e2e/demos.spec.ts:38`, `ui/e2e/demo/11-ci-and-headless.spec.ts:258`,
  `ui/e2e/screenshots/docs.spec.ts:123`.
- `ui/src/app/components/screens/setup/setup-layout.tsx:112` — the long-form variant.
- `docs/DEMO-SCRIPT.md:536,616` — the script opens and closes on the line.
- `ui/src/app/components/screens/setup/access-panel.tsx:522-528` — the role picker whose option
  list gains a third entry, and `ui/src/app/lib/types/access.ts:10`, the union it renders from.

Implementation is not done when it builds and unit tests pass: the UI e2e suite is daemon-only
and is **not** part of `make ci` — run `scripts/run-ui-e2e.sh` before calling any of it done.

## 6. Where the model lives on the page

**New screen `/governance`**, a new `NAV_ITEMS` entry between Policies and Permissions (slot
confirmed by **Q1**), gated client-side by the security-operator predicate and server-side by
the security-ops route group. Top to bottom:

1. **Profiles** — the list (name, assigned-to, limits, grade, updated) with the New profile
   action; the editor opens in place, embedding `PolicyPanel instance="policies"` for the
   ceiling, the two limit switches, and the grade meter.
2. **Assignments** — the table (who, profile, priority, added), the add form (subject-type
   `Segmented` + subject field + profile + priority), and the precedence line. **While the
   editor above is open this form is collapsed to its disabled `ADD_TITLE` summary row**
   (§2.6/§4) — one teal button on the screen at any moment, no new dialog.
3. **Resolved profile** — the preview: paste the claims a person's token would carry, get the
   one profile those claims resolve to and what matched. Input shape is **Q2**, and it is a
   claims dry run rather than a person lookup for the reason §1 records — there is no
   server-side session row to read a live person's groups from.

**Member surfaces, inline, in place:**

- `new-run-screen.tsx`'s `SectionCard title="Policy"` — one line above the panel (§7.6).
- `member-getting-started.tsx`'s "What's set up for you" card — one chip in the existing chip
  row plus one line, naming the profile (§7.6).
- Launch refusals and warnings — the existing 403 body and `warnings[]` toast paths (§7.7).

**Surfaces frozen but not moved this round:** the onboarding hero and the setup layout
subtitle (§7.8); the People step's role picker options and the two "who" fields that become
comboboxes (§7.9).

## 7. Canonical strings — FROZEN

Throughout §7, a backticked substring inside a frozen string (an env var, a wire field, a
literal value) renders `font-mono` in the console and in the mock — apply the mono span
uniformly everywhere that substring recurs, not only on its first appearance in a given
string. A **profile name** is never mono: it is a human-chosen label and is rendered inside
double quotes, exactly as the strings below spell it.

**Server-refusal shape (§7.7 only).** The member-facing refusals are `writeError` bodies and
follow the in-tree convention those already use — a lowercase-opening clause naming the wire
field or the thing refused (`internal/api/runs_create_validate.go:153`,
`runs_create_validate.go:298`), not a console-styled sentence. The console renders them
verbatim.

### 7.1 Reused canon — referenced, never re-frozen

These strings already exist and are **imported**, not retyped, by the governance surfaces.
Listed so that every product string rendered in the mock has a key somewhere.

| Key | Lives in | String |
|---|---|---|
| `PERM.COL_WHO` / `PERM.FIELD_WHO` | `permissions-copy.ts` | Who |
| `PERM.COL_ADDED` | `permissions-copy.ts` | Added |
| `PERM.SUBJECT_USER` | `permissions-copy.ts` | User |
| `PERM.SUBJECT_GROUP` | `permissions-copy.ts` | Group |
| `PERM.SUBJECT_ALL` | `permissions-copy.ts` | Everyone signed in |
| `PERM.HINT_USER` | `permissions-copy.ts` | An email address or the sign-in subject id. Either one matches the same person. |
| `PERM.HINT_GROUP` | `permissions-copy.ts` | A group or app-role name exactly as your identity provider sends it in the token. |
| `PERM.HINT_ALL` | `permissions-copy.ts` | Every signed-in member. Admins are exempt. |
| `PERM.REMOVE` | `permissions-copy.ts` | Remove |
| `PEOPLE.CANCEL` | `people-access-copy.ts` | Cancel |
| `PEOPLE.ROLE_ADMIN` / `PEOPLE.ROLE_MEMBER` | `people-access-copy.ts` | Admin / Member |
| `PEOPLE.FIELD_VALUE` / `PEOPLE.ADD_CTA` | `people-access-copy.ts` | Value / Add mapping |
| `PREVIEW.FIELD_CLAIMS` | `people-access-copy.ts` | Roles, groups, or email |
| `PREVIEW.FIELD_CLAIMS_HINT` | `people-access-copy.ts` | One value per line — an App Role, a group name, or an email address. |
| `ACCESS_STATE.FETCH_FAILED_RETRY` | `people-access-copy.ts` | Retry |
| Setup layout heading | `setup-layout.tsx:111` | Getting started |
| `MEMBER_GETTING_STARTED.SETUP_SUMMARY_TITLE` | `wardyn/copy.ts` | What's set up for you |
| `MEMBER_GETTING_STARTED.SETUP_SUMMARY_HELPER` | `wardyn/copy.ts` | Your admin configured the barrier, network and shared credentials. Your runs inherit them. |
| `MEMBER_GETTING_STARTED.BARRIER_CHIP(label)` | `wardyn/copy.ts` | Barrier · {label} |
| `MEMBER_GETTING_STARTED.MODEL_ACCESS_PROVIDED_CHIP` | `wardyn/copy.ts` | Model access · Provided by your admin |
| `MEMBER_GETTING_STARTED.SIGNIN_SSO_CHIP` | `wardyn/copy.ts` | Sign-in · SSO |
| `PEOPLE.FIELD_ROLE` | `people-access-copy.ts` | Role |
| `NAV_ITEMS[…].label` | `app-shell.tsx` | Runs · Approvals · Workspaces · Policies · Permissions · Secrets · Audit · Recordings |
| New Run policy section title | `new-run-screen.tsx` | Policy |
| PolicyPanel saved-policy cards | `policy-panel.tsx` | Reuse a saved policy / One your operators already wrote and named. / Custom policy / Start from a template and edit the spec for this run. |
| PolicyPanel preflight button + hint | `policy-panel.tsx` | Preflight · Checks the spec server-side and shows what would be clamped — before you launch. |
| `SafetyMeter` eyebrow + grades | `safety-meter.tsx` | Safety · Safest / Guarded / Elevated / Weakest |
| `relativeTime(t)` | `lib/format.ts` | "3 days ago" — the shipped helper, not a second one |
| `CC_META[…].label` | `wardyn/cc-meta.ts` | Fence / Wall / Vault |
| Launch-warning toast title | `run-warnings.ts` | Run launched with a warning |
| codex-cli explicit-hold refusal | `runs_create_validate.go` | tool_approvals=hold is not supported for codex-cli (no external tool-approval contract) |

**Shipped by the enforcement lane — rendered verbatim, never re-worded (§7.4).** These landed
while this round was drawn; canon adopts them rather than freezing a second wording that the
server would then have to be changed to emit.

| Source | Lives in | String |
|---|---|---|
| Delete refusal (409) | `handleDeleteGovernanceProfile` | this governance profile is still assigned — delete its assignments first (deleting it while assigned would silently widen everyone it bounds back to the deployment ceiling) |
| Grant-bound 400 prefix | `handleCreateGovernanceProfile` | invalid ceiling: |
| …pairing leg | `governanceGrantWithinCeiling` | eligible grant %q pairing secret %q with host %q is not in the deployment ceiling (a profile may narrow the deployment's eligible grants, never add one) |
| …approval leg | `governanceGrantWithinCeiling` | eligible grant %q strips requires_approval, which the deployment ceiling sets (a profile may force approval on, never off — without it the credential auto-mints at proxy boot) |
| …TTL leg | `governanceGrantWithinCeiling` | eligible grant %q ttl_seconds resolves to %ds, above the deployment ceiling's %ds (0 means the %ds default, so it is not a narrowing) |
| …GitHub-scope leg | `governanceGrantWithinCeiling` | eligible grant %q: «`composer.GitHubScopeWithin`'s own error» |
| …no same-kind grant | `governanceGrantWithinCeiling` | eligible grant %q is not in the deployment ceiling's eligible grants (a profile may narrow the deployment's credential eligibility, never mint new eligibility) |
| Omission warning 1 of 5 | `governanceOmissionWarnings` | this profile omits %d denied domain(s) the deployment default denies (%s) — members under it are NOT walled from them |
| Omission warning 2 of 5 | `governanceOmissionWarnings` | this profile omits %d allowed domain(s) the deployment default allows (%s) — members under it lose access to them |
| Omission warning 3 of 5 | `governanceOmissionWarnings` | this profile omits %d eligible grant kind(s) the deployment default carries (%s) — members under it cannot request those credentials |
| Omission warning 4 of 5 | `governanceOmissionWarnings` | this profile's min_confinement_class %q is WEAKER than the deployment default's %q |
| Omission warning 5 of 5 | `governanceOmissionWarnings` | this profile sets allow_all_egress while the deployment default does not — members under it reach any non-denied public host |

All eleven are Go format strings; `%q` renders its value in double quotes and `%s`/`%d` bare,
which is what the mock draws. The five omission warnings are **independent list items**, never
joined into a sentence.

`PRIORITY_NA` (§7.3) is the same em dash `PEOPLE.ADDED_CHART_NA` renders, kept as its own key
because the two mean different things (no priority in this tier vs no console-tracked add
time) and one must be able to change without the other.

### 7.2 `GOVERNANCE` — the profiles block

| Key | String |
|---|---|
| `TITLE` | Governance |
| `LEAD` | Named ceilings, assigned to people and groups. An assigned profile replaces the deployment ceiling for its subjects; anyone with no assignment keeps the deployment ceiling. |
| `PROFILES_TITLE` | Profiles |
| `PROFILES_LEAD` | A profile is one ceiling: the policy every run under it is bounded by, plus the launch modes its subjects may not use at all. |
| `COL_NAME` | Name |
| `COL_ASSIGNED` | Assigned to |
| `COL_LIMITS` | Limits |
| `COL_GRADE` | Grade |
| `COL_UPDATED` | Updated |
| `NEW_CTA` | New profile |
| `EDIT` | Edit |
| `DELETE` | Delete |
| `ASSIGNED_NONE` | Not assigned |
| `ASSIGNED_COUNT(n)` | {n} subject / {n} subjects |
| `LIMITS_NONE` | None |
| `EMPTY_TITLE` | No profiles yet |
| `EMPTY_BODY` | Everyone runs under the deployment ceiling from `WARDYN_DEFAULT_POLICY`. Add a profile to give a group its own. |
| `EDITOR_TITLE_NEW` | New profile |
| `EDITOR_TITLE_EDIT(name)` | Edit "{name}" |
| `FIELD_NAME` | Name |
| `NAME_HINT` | What this profile is called on the assignments below, and in the run of anyone assigned to it. Names are unique. |
| `CEILING_TITLE` | Ceiling |
| `CEILING_LEAD` | Every run under this profile is bounded by this spec. A member's own policy is clamped to it, and so is a saved policy they pick. |
| `LIMITS_TITLE` | Limits |
| `LIMITS_LEAD` | Some of what a run can do routes around the ceiling entirely. Deny it here instead. |
| `LIMIT_EXEC_LABEL` | Deny exec runs |
| `LIMIT_EXEC_HINT` | `task_mode=exec` runs a command with no agent, so no tool rule is ever consulted. |
| `LIMIT_INTERACTIVE_LABEL` | Deny interactive runs |
| `LIMIT_INTERACTIVE_HINT` | An interactive run is supervised at the attach pane rather than by rules. A run with no task comes up interactive too, and is refused the same way. |
| `LIMIT_DRIVE_LABEL` | Deny mounting a user drive |
| `LIMIT_DRIVE_HINT` | A run under this profile cannot mount the person's drive, even when one is allocated to them. |
| `GRADE_NOTE` | Grades this ceiling as written — advisory, the same meter the policy editor shows. |
| `SAVE` | Save profile |
| `SAVE_ERROR` | Couldn't save this profile. |

`TITLE` is **one string for two places** — the `NAV_ITEMS` label and the screen heading — the
way every other nav entry already works; there is no second "Governance profiles" label.
`ASSIGNED_COUNT` follows the inline-pluralisation shape `PERM.ENFORCE_ON_BODY` already uses
(`${n} subject${n === 1 ? "" : "s"}`), not a second pluralisation helper. `COL_GRADE`'s cell
renders the embedded `SafetyMeter`'s own vocabulary (Safety · Safest / Guarded / Elevated /
Weakest), and `COL_UPDATED` the shipped `relativeTime` helper — both existing canon, neither
re-frozen here.

### 7.3 `GOVERNANCE` — assignments and the resolved preview

| Key | String |
|---|---|
| `ASSIGN_TITLE` | Assignments |
| `ASSIGN_LEAD` | Who runs under which profile. One profile applies to a person — never two merged together. |
| `PRECEDENCE` | The most specific assignment wins: a person beats a group, and a group beats everyone. Within a person, the sign-in subject beats the email. Between groups, the higher priority wins, then the profile name. |
| `EFFECT_NOTE` | Takes effect on their next run. A run already dispatched keeps the ceiling it started with. |
| `SIGNIN_NOTE` | A change to someone's groups in your identity provider reaches Wardyn only when they next sign in, so they keep their current ceiling until then — or, for an API token, until it is re-minted. |
| `COL_PROFILE` | Profile |
| `COL_PRIORITY` | Priority |
| `PRIORITY_NA` | — |
| `PRIORITY_HINT` | Breaks ties between groups a person is in. Higher wins. It is ignored for a person and for everyone. |
| `ADD_TITLE` | Assign a profile |
| `ADD_CTA` | Assign |
| `FIELD_PROFILE` | Profile |
| `PROFILE_PLACEHOLDER` | Pick a profile |
| `FIELD_PRIORITY` | Priority |
| `UNASSIGN_CONFIRM(who, name)` | Remove "{name}" from {who}? Their next run is bounded by whatever else matches them, or by the deployment ceiling if nothing does. |
| `EMPTY_ASSIGN_TITLE` | No assignments yet |
| `EMPTY_ASSIGN_BODY` | A profile with no assignment bounds nobody. Assign one to a group to start. |
| `PREVIEW_TITLE` | Resolved profile |
| `PREVIEW_LEAD` | Paste the roles, groups, or email a person's token would carry, and see which profile would bind their runs. |
| `PREVIEW_RUN_CTA` | Resolve |
| `PREVIEW_RESULT(name, matched)` | These claims resolve to "{name}" — matched by {matched}. |
| `PREVIEW_RESULT_DEFAULT` | These claims resolve to the deployment ceiling — no assignment matches them. |
| `PREVIEW_RESULT_UNKNOWN` | Couldn't resolve this — try again. |
| `PREVIEW_NOT_SAVED` | Nothing here is saved. |

**`EFFECT_NOTE` and `SIGNIN_NOTE` are two halves of one fact and render together.** The two
subjects are different, and getting them backwards is the trap: **an assignment** is a row the
resolver re-reads on every run, so a new one binds at the member's next *run* (`EFFECT_NOTE`);
**a person's group membership** is read once, at sign-in, so moving someone between groups
binds only at their next *sign-in* (`SIGNIN_NOTE`) — and for an API token, only at its next
mint, because a token carries the groups stamped when it was minted. Both halves belong where
the admin assigns: without the second, assigning a profile to a group and watching nothing
happen for an already-signed-in member looks like a broken write. This is deliberately **not**
`PERM.SNAPSHOT_BODY` reused — that string ends on "Grants themselves take effect on the next
request", which is the grants rule and is false here: what lags for governance is a
**ceiling**, not a permission, and it lags a sign-in rather than a request.

**The preview has no field label or hint of its own.** It takes the claims a token would
carry, which is precisely what the People step's preview already takes, so it renders
`PREVIEW.FIELD_CLAIMS` / `PREVIEW.FIELD_CLAIMS_HINT` verbatim (§7.1) — two labels for one
accepted shape is how they drift apart. Q2's losing variant (name a principal) borrows
`PERM.FIELD_WHO` / `PERM.HINT_USER` for the same reason; no copy is frozen here for a variant
that may not ship.

**What the preview can and cannot claim.** It resolves the claims *as typed* against the
assignments — a deterministic answer the server can actually compute. It is not a person
lookup and never says a live person's groups: sessions are stateless cookies with no
server-side row (§1), so nothing here can read them. `PREVIEW_RESULT`'s `{matched}` names the
matching row in the table's own vocabulary ("a group assignment", "a user assignment", "the
everyone assignment"). There is deliberately **no stale-snapshot arm**: a truncated or missing
snapshot is a fact about a member's own cookie or API token and surfaces where it actually
bites — at their launch, as `DENIED_STALE_GROUPS` (§7.7).

### 7.4 Write refusals and the omission warning

| Key | String |
|---|---|
| `GRANT_BOUND_TITLE` | These grants go past the deployment ceiling |
| `DELETE_CONFIRM(name)` | Delete "{name}"? It isn't assigned to anyone, so nobody's ceiling changes. |
| `DELETE_RESTRICT_TITLE` | This profile is still assigned |
| `DELETE_RESTRICT_BODY(name, n)` | "{name}" still has {n} assignment / {n} assignments. Deleting it would widen those subjects back to the deployment ceiling without anyone deciding that — remove the assignments first. |
| `OMISSION_TITLE` | This profile narrows by omission |
| `OMISSION_ACK` | I understand this profile takes these away. |

**Two of these are headings over server text, not replacements for it.** The enforcement lane
ships both bodies already, each more specific than a frozen sentence could be, so canon yields
and the console renders the server's message under the console's own heading:

- **`GRANT_BOUND_TITLE`** heads the 400's message, which `handleCreateGovernanceProfile`
  composes as `"invalid ceiling: "` + the comparator's own error — free-form prose that already
  names the failing leg, the grant kind, the secret, the host, and the two TTLs
  (`governanceGrantWithinCeiling`, `internal/api/governance_grantbound.go`). Freezing four
  `CAUSE_*` clauses here would have meant specifying a structured cause on the wire for copy
  polish; the shipped strings are listed in §7.1 as reused canon instead.
- **`OMISSION_TITLE`** heads the **list** the write response returns:
  `governanceOmissionWarnings` yields up to five INDEPENDENT prose warnings on
  `Warnings []string` (`internal/api/governance.go`), one per field where an omission changes
  what a member can reach. They render as a list, verbatim, in the response's own order — not
  composed into one sentence, which is why no `OMISSION_BODY` exists. Those five are §7.1 too.

`DELETE_CONFIRM` is shown only when the profiles list reports `ASSIGNED_COUNT` = 0 — otherwise
the dialog opens pre-filled with `DELETE_RESTRICT_TITLE` / `_BODY` and its confirm disabled
(§2.4), and `DELETE_RESTRICT_BODY` uses the same inline pluralisation as `ASSIGNED_COUNT`.
**`DELETE_RESTRICT_BODY` is the client-side pre-fill only**: it names a count, and the count is
knowable only there. On the race path the client believed the count was zero, so it renders the
shipped 409 — which carries no count and must not grow one (§7.1). `OMISSION_ACK` exists only
for Q6's acknowledge-before-save variant; the recommended variant renders `OMISSION_TITLE` over
the warning list after a successful save and never blocks it.

### 7.5 States

| Key | String |
|---|---|
| `FETCH_FAILED_TITLE` | Couldn't load governance profiles |
| `FETCH_FAILED_BODY` | Something went wrong reaching the server. Profiles that are already assigned still bound every run — this list just can't show them right now. |

Retry is `ACCESS_STATE.FETCH_FAILED_RETRY` (§7.1). There is no "governance not configured"
state: with no profiles the feature is not unconfigured, it is empty, and `EMPTY_TITLE` /
`EMPTY_BODY` (§7.2) say so honestly.

### 7.6 `MEMBER` — the two display moments

| Key | String |
|---|---|
| `CEILING_PROFILE(name)` | Bounded by "{name}", the governance profile your admin assigned you. Your policy is clamped to it. |
| `GS_CHIP(name)` | Governance · {name} |
| `GS_BODY(name)` | Your runs are bounded by "{name}". What you can change is what it leaves open. |

Both moments render **only when a profile is assigned**. With no assignment there is no chip,
no line, and no placeholder — today's screens byte-for-byte, which is the same absent-row
doctrine the resolver follows. `GS_CHIP` follows `BARRIER_CHIP`'s `Label · value` shape
exactly. Both **name the profile**: that is the plan's decision, not an open call, and it is
the only wording consistent with the New Run line and every refusal in §7.7, which name it
too.

### 7.7 `MEMBER` — refusals and warnings (server-composed)

| Key | String |
|---|---|
| `DENIED_TASK_MODE_EXEC(name)` | `task_mode=exec` is not allowed by your governance profile "{name}" — an exec run carries no agent and no tool approvals, so nothing supervises it. Launch with an agent instead. |
| `DENIED_INTERACTIVE(name)` | interactive runs are not allowed by your governance profile "{name}", and a request with no task comes up interactive too. Launch with a task, and without `--interactive`. |
| `DENIED_SEED_AUTO_TOOLS(name)` | `seed_auto_tools` is not allowed by your governance profile "{name}": its tool rules hold or deny, and the pre-attach seed runs before any human is at the pane. Launch without it. |
| `DENIED_CODEX_HOLD(name)` | codex-cli is not supported under your governance profile "{name}": its tool rules hold or deny, and codex-cli has no external tool-approval contract. Launch a different agent. |
| `WARN_STORED_CLAMPED(policy, name)` | saved policy "{policy}" was clamped to your governance profile "{name}" |
| `WARN_WORKSPACE_DENIED(host, name)` | workspace host "{host}" is denied by your governance profile "{name}" — the run launches, but that host is refused at the proxy |
| `WARN_GRANT_DROPPED(name, kind, reason)` | governance profile "{name}": dropped {kind} grant no longer within the deployment's eligible grants ({reason}) |
| `DENIED_STALE_GROUPS` | groups_snapshot_stale: your group membership snapshot is missing or was truncated at sign-in, and this deployment assigns governance profiles by group — sign in again (or re-mint your API token) so your ceiling can be resolved |
| `DENIED_SEEDED_IMAGE(image)` | image {image} comes from your own workspace's base image and is not granted to you — ask an admin to grant the exact image ref, or launch with the agent's convention image |
| `DENIED_WORKSPACE_LLM_CRED` | llm_cred is operator-only — an admin binds a workspace's model/harness credential (PUT /workspaces/{id}/llm-cred); create your workspace without it and ask for the binding |

**This table is COMPLETE** (§5 #5): every string a member can be refused or warned with at a
door this campaign touches is here. Four of them are the enforcement lane's, adopted byte-exact
rather than re-worded, because the shipped wording is the better wording:

- **`DENIED_STALE_GROUPS`** is `groupsSnapshotStaleMsg` (`internal/api/governance.go`) verbatim.
  It beats the console-voiced draft it replaces on the one thing that matters: it names **both**
  remedies. "Sign out and back in" is useless to an API-token holder, whose groups were stamped
  at mint — the shipped string says "or re-mint your API token" and so cannot mislead them. It
  keeps its `groups_snapshot_stale:` wire prefix, the same shape every other coded refusal uses.
- **`WARN_GRANT_DROPPED`** is `reintersectGovernanceGrants`' warning
  (`internal/api/governance.go`), which fires when a redeploy removes a pairing from
  `WARDYN_DEFAULT_POLICY` that a stored profile still names: the grant is dropped rather than
  the run failed, and the member is told. It rides the same `warnings[]` list as the two above.
- **`DENIED_SEEDED_IMAGE`** is `denyMemberSeededImage`'s refusal
  (`internal/api/runs_create_validate.go`) verbatim — the **seeded-image door**, closing the gap
  §1 names: a workspace's own `base_image` sets `req.Image` *after* `denyMemberRequest` has
  already run, so the explicit `--image` branch could not catch it. Same target and reason
  (`runs.image` / `byoi_member`) as that branch, because it is the same capability answered
  about the same value; only the door differs, and the message says which one.
- **`DENIED_WORKSPACE_LLM_CRED`** is `handleCreateWorkspace`'s refusal
  (`internal/api/workspaces.go`) verbatim — a member naming an `llm_cred` binding on workspace
  create is refused rather than silently dropped, on the same "a field accepted and thrown away
  is worse than one refused" rule `interactiveToolApprovalsError` states. It names the operator
  route to ask for, which is what makes it actionable.

**Two of these name no profile, deliberately.** `DENIED_SEEDED_IMAGE` and
`DENIED_WORKSPACE_LLM_CRED` are **capability** refusals, not governance-profile ones: they fire
whether or not the caller has a profile, and there is no profile name to interpolate. They are
in this table because §5 #5 makes it the complete set of what a member is refused with at these
doors — an implementer must not "fix" them by adding a profile name they do not have.

`DENIED_CODEX_HOLD` sits beside, and never replaces, the existing explicit-hold refusal
(§7.1) — one refuses a hold the caller asked for, the other refuses a hold their profile
derived. `WARN_STORED_CLAMPED` is the header warning only: `composer.Clamp`'s own per-field
warnings already ride the same `warnings[]` list and are unchanged canon, so this string says
*that* the clamp happened and lets the existing warnings say *what* it changed.
`PERM.STALE_GROUPS` (in `permissions-copy.ts`) stays the advisory line about a *missing*
permission and is not reworded.

### 7.8 `POSITIONING` — the slogan (§H)

| Key | Today | Frozen |
|---|---|---|
| `HERO_SLOGAN` | Run anything. Keep your keys. | Sandboxed. Governed. Self-hosted. Free. |
| `SETUP_SUBTITLE` | Governed sandboxes for anything you run — keep your keys. | Governed sandboxes for anything you run — on your own infrastructure, free. |

Four words carrying the combination the product actually has: sandbox depth, org-level
governance, the org's own infrastructure, and no paywall. The long-form variant keeps its
sentence shape and drops the keys-only frame. Neither names a competitor, and neither claims
a maturity Wardyn has not reached — "governed" is a mechanism the console demonstrates, not a
compliance claim. The released videos narrate the old line and lag this change (§2.7); the
live repository description is an owner-run command, never an agent's.

### 7.9 `DIRECTORY` — the combobox and the third role (§I)

| Key | String |
|---|---|
| `ROLE_SECURITY_ADMIN` | Security admin |
| `SUGGEST_ROW(displayName, detail)` | {displayName} — {detail} |
| `GROUP_PICKED_CHIP(displayName)` | Group · {displayName} |
| `GROUP_VALUE_NOTE` | Stored as the group's object id — the name is what your directory calls it today. |
| `SEARCHING` | Searching your directory… |
| `MIN_CHARS_HINT` | Type at least 2 characters to search your directory. |
| `NO_MATCHES` | No matches in your directory. Type the value yourself if you know it. |
| `LOOKUP_FAILED` | Couldn't check your directory — type the value yourself. |

`SUGGEST_ROW`'s `{displayName}` is plain text and `{detail}` is mono (an email, an object id,
an app-role value) — the row is the one place both appear together, and the mono half is what
gets stored. `GROUP_PICKED_CHIP` shows the NAME while the field's value is the object id;
`GROUP_VALUE_NOTE` is what stops that from being a lie, and is why the group case gets a chip
at all while a user's email needs none. **Absent mode has no string**: with no directory
configured the endpoint answers with its unconfigured code and the control IS the plain input
— no note, no banner, no disabled state, nothing for a reader to act on. `SEARCHING` and
`MIN_CHARS_HINT` are frozen here but their *display* behaviour is **Q7**.

`ROLE_SECURITY_ADMIN` is the picker option, the table chip, and the mapped-role label —
one string for all three, next to `PEOPLE.ROLE_ADMIN` / `PEOPLE.ROLE_MEMBER`, which is why it
is title case and never interpolated into a sentence (that casing rule is
`people-access-copy.ts`'s and carries over unchanged).

## 8. Where to apply (once implemented, out of scope this round)

- **`/governance`** — the whole screen: profiles block, assignments block, resolved preview
  (§7.2–§7.5), plus a `NAV_ITEMS` entry and its client-side gate.
- **New Run, Policy card** — `CEILING_PROFILE` (§7.6).
- **Member Getting Started** — the governance chip and line (§7.6).
- **Run create** — the five refusals and two warnings (§7.7), composed server-side beside the
  gates that raise them.
- **Onboarding hero + setup layout** — `HERO_SLOGAN`, `SETUP_SUBTITLE` (§7.8), with every
  assertion site in §5 moved in the same change.
- **Governance assignment subject field + the People step's mapping value** — the combobox
  (§7.9), in the phase that builds the connector.
- **People step role picker + the access role union** — `ROLE_SECURITY_ADMIN` (§7.9).
- **`/permissions`, `/approvals`, `/policies`** — unchanged; only the reuse stated in §1.1.

## 9. Owner question list

**Q1.** Governance's nav slot: **Policies → Governance → Permissions** vs **Policies →
Permissions → Governance**. (That Governance belongs in that group at all is decided — Settings
is "the home for connections", the wrong semantic.) **Recommend Policies → Governance →
Permissions** — the three read as a narrowing sequence: the deployment ceiling, the ceilings
assigned over it, then the grants layered inside one. Governance also reads the same objects
Policies does, so the two sit adjacent; Permissions is the odd one out and goes last.

**Q2.** The resolved preview's input: **(a) name a principal** — one field taking an email or
a sign-in subject id; **(b) paste claims** — the roles/groups/email a token would carry, the
`POST /access/preview` shape the People step already ships (`access-panel.tsx:574` onward);
**(c) both**, as People does with its "test with my own session" link. **Recommend (b).**
(a) reads as the friendlier surface, and it cannot be built honestly: **Wardyn keeps no
server-side session row.** The session is a signed cookie (`internal/auth/oidc/oidc.go:22-24`)
and revoking a human stamps a cutoff precisely because "there is no session row to delete"
(`internal/api/sessions.go:23-25`), so a member's group snapshot lives in *their* cookie and
no admin request can read it. The only persisted snapshot is `api_tokens.groups`, stamped at
mint — a stale artefact of one token, not the person. So (a) could answer only for a
user-tier assignment and would silently mis-answer, or blankly refuse, for the group tier —
which is the tier the whole feature is for. (b) resolves what the admin types, deterministically,
against the same ordered query the resolver runs, and mirrors a preview this console already
ships and members already understand. (c) is (a)'s problem plus a second control. The one
thing (b) genuinely cannot see — a member whose own snapshot is truncated — surfaces where it
bites, at that member's launch (`DENIED_STALE_GROUPS`, §7.7), and the preview says nothing
about it rather than guessing. (a) is drawn in the mock as the losing variant, borrowing
`PERM.FIELD_WHO` / `PERM.HINT_USER` rather than minting copy for a variant that may not ship.

**Q3.** The two limit switches: **(a) inside the ceiling editor**, below the spec, as one
"Limits" section of the profile form; **(b) their own card** beside the ceiling card.
**Recommend (a).** They are part of one saved object and are meaningless apart from it — a
second card implies a second write. `CONSOLE-RULES.md` §9's "never nest a card in a card"
makes the section, not the card, the right container for a subordinate group.

**Q4.** Priority editing: **(a) a number input** on the assignment row and in the add form;
**(b) drag-to-order** within each group tier, with the number derived. **Recommend (a).** The
stored value is a number the API takes and the audit row records; a drag surface would have to
invent and rewrite numbers for rows the admin did not touch, and it cannot express "these two
deliberately tie" (which the profile name then breaks, per `PRECEDENCE`). A number input is
also the only one of the two that works when the list is long enough to scroll.

**Q5.** Profile-name rules: **(a) free text, unique**, as the schema says; **(b) constrained**
to a slug shape (lowercase, dashes) the way a policy id is. **Recommend (a).** The name is
rendered to members inside quotes in five strings (§7.6, §7.7) — "Greenfield contractors"
reads correctly there and `greenfield-contractors` does not. Uniqueness is already enforced,
and the name is never a path segment or a wire key, so nothing downstream needs the constraint.

**Q6.** The omission warning's presentation: **(a) an inline note after a successful save**
(`OMISSION_TITLE` + `OMISSION_BODY`, non-blocking); **(b) an acknowledge-before-save step**
(`OMISSION_ACK` on a confirm, the posture-guard shape the People step uses); **(c) a persistent
badge** on the profile row for as long as the omission stands. **Recommend (a).** Narrowing by
omission is the *feature*, not an accident — a profile exists to take things away, and gating
every save behind an acknowledgement would train admins to click through the one warning that
matters. (b) is drawn as the losing variant. (c) is rejected outright: the deployment ceiling
is env-borne and changes under the profile without any write here, so a persistent badge would
be stale the moment a redeploy drops a grant.

**Q7.** Combobox feedback while typing: **(a) silent** — no `SEARCHING`, no `MIN_CHARS_HINT`;
suggestions simply appear; **(b) show `MIN_CHARS_HINT` under the field until the second
character, then `SEARCHING` while a request is in flight**; **(c) `SEARCHING` only**.
**Recommend (c).** `CONSOLE-RULES.md` §7 puts a sub-second action at "nothing" and a 1–3s one
at a label — a debounced directory call sits at the boundary and is the one part of this a
reader can be left wondering about, so it gets a label and nothing else does. A minimum-length
hint under a field the admin can also just type into is the "no filler" rule's exact target:
it explains a threshold they will cross before finishing the first word. Both strings stay
frozen either way; (a) and (b) are one-line changes at implementation if the owner disagrees.

## Adjudication
