# People — the acting surface for who can sign in

This is the mock round for the People acting surface — the design gate before any console
code (owner law: the mock is UI source of truth; canon strings are app strings). The model
below is decided and converged; nothing here is open for re-design, only for drawing. Eight
open calls remain and are listed as Q1–Q8 in §9, with an empty Adjudication section at the
end for owner answers.

Static mock: `docs/design/people-access-mock/index.html` (open it in a browser).
Frozen strings: §7 below. No TS copy module exists yet — this is a mock round, same as
permissioning's A-A stage was before `ui/src/app/lib/permissions-copy.ts` existed. The
implementation stage creates `ui/src/app/lib/people-access-copy.ts` from §7 verbatim; it does
not retype copy from this document, and every string in the mock HTML matches §7 byte-for-byte.

---

## 1. What exists today (the thing being grown)

The People step (`ui/src/app/components/screens/setup/step-bodies.tsx`'s `DeploymentStep`,
canon strings `PEOPLE_STEP` in `ui/src/app/components/wardyn/copy.ts:615-640`) is a pure
explainer, done on arrival (`steps.ts`'s `stepDone.people`). Single-user mode explains the one
shared admin credential. Multi-user mode explains that people sign in with SSO and get a role
from `WARDYN_OIDC_ROLE_MAP` or the operator allowlist, states the Admins/Members split, and
links out to `/permissions` for capability grants. There is nothing to configure on this
screen today — it only describes what env vars already decided at boot.

Role derivation itself (`internal/auth/oidc`'s `deriveRole`, documented in full in
`docs/OPERATIONS.md` → "Multi-user: who can change what" and `docs/ENV.md`'s
`WARDYN_OIDC_ROLE_MAP` / `WARDYN_OIDC_DEFAULT_ROLE` / `WARDYN_OIDC_OPERATOR_EMAILS` rows) is
env-only: a CSV of `value=role` pairs matched against the `roles` claim (Entra App Roles, the
priority path), the `groups` claim, or email, case-insensitively, admin-wins-on-conflict,
unmatched falls to `WARDYN_OIDC_DEFAULT_ROLE` or denies the login, derived once at
`/auth/callback` and stamped into the session cookie. Editing it today means editing a chart
value and doing a helm upgrade.

## 2. The model this round mocks

The People step becomes the acting surface for that role map. A **role-mappings table** merges
two sources:

- **Chart rows** — read-only, sourced from `WARDYN_OIDC_ROLE_MAP`, source-labeled, with a hint
  pointing back at the chart.
- **Console rows** — editable, server-persisted via the admin API.

A row is `(value, role)` where role is `admin` or `member` and value is an Entra App Role
value, a `groups`-claim entry, or an email, matched case-insensitively at next sign-in. Every
existing semantic holds: any `admin` match wins; an unmatched sign-in falls to
`WARDYN_OIDC_DEFAULT_ROLE`, else is denied; a role is derived at sign-in only, never
retroactively.

A **console row that collides with a chart key, or with an operator-allowlist email, is
refused at write time (400)** — it would only ever be shadowed, so the write never happens. A
**late helm upgrade** can still introduce a chart key that collides with an already-saved
console row, or add an email to `WARDYN_OIDC_OPERATOR_EMAILS` that collides with one — two
distinct shadow causes, same outcome: the chart/allowlist wins (same precedence as boot) and
the console row renders with a badge instead of disappearing, so the admin can see and clean
it up — "shadowed by your chart" (`SHADOWED_BADGE`, §7.2) for a role-map key collision,
"shadowed by your operator allowlist" (`SHADOWED_OPERATOR_BADGE`, §7.2) for an allowlist-email
collision.

`WARDYN_OIDC_DEFAULT_ROLE` and `WARDYN_OIDC_OPERATOR_EMAILS` display **read-only**, source
"chart" — this round does not make either editable from the console.

Capability grants stay exactly where they are, at `/permissions`. This step keeps its "Open
Permissions" outline button and hint (`PEOPLE_STEP.MULTI_USER_PERMISSIONS_ACTION` /
`_HINT`, unchanged — hard canon, see §6).

A **preview panel** does a dry run: paste the claims a token would carry (role/group values,
email) — or use one's own live session — and see the role that would derive, which entries
matched, or that nothing matched (would-be-denied), or that the check itself failed
(couldn't-check). Nothing is saved by a preview.

IdP-side duties — creating people and groups, assigning Entra App Roles, the app
registration's "assignment required" setting — are explained as the identity provider's job.
None of it is editable here; the screen only says what Wardyn does with a value the IdP
already sends.

### 2.1 Guards on the two posture-flipping writes

Two writes can change what happens to everyone who **doesn't** match anything and **isn't** on
the operator allowlist, and both guards fire **only when the chart map is empty** — a
non-empty chart already provides real gating, so these are the two moments a console-only
deployment can flip its whole default posture with one row: **first console row added**
(chart empty, no console rows yet) and **last console row deleted** (chart empty, this is the
only remaining row).

**The predicate is not "the map went from empty to non-empty" — it's "the outcome for that
unmatched, non-allowlisted person changed."** Compute the outcome on each side of the flip
using `HasOperatorEmails()` and `DefaultRole()` (both already exported,
`internal/auth/oidc/derive.go`):

- **Before the flip** (combined map empty — `deriveRole`'s arm 1): `member` when
  `HasOperatorEmails()` is true, `admin` when it's false.
- **After the flip** (combined map non-empty — `deriveRole`'s arm 2/3 fallthrough): the
  default role when `DefaultRole()` is set, denied when it's unset.

The guard fires **iff before ≠ after** — first-row-added compares (empty-map outcome) →
(non-empty-map outcome); last-row-deleted compares the same two outcomes in the opposite
direction. This is stricter than "chart map is empty ⇒ always fires": with the operator
allowlist set and `DEFAULT_ROLE=member`, before=member and after=member — the write is a
no-op for the person the guard is about, so it does **not** fire. But with the allowlist set
and `DEFAULT_ROLE=admin`, before=member and after=admin — a **silent widening** the split-only
version of this guard (chart-empty ⇒ always fire, worded only in terms of the default-role
value) would have caught only by accident, if at all. `ALLOWLIST_NOTE` (§7.3) is appended
whenever `HasOperatorEmails()` is true, since the guard's whole subject is the person that
note doesn't cover.

Both guard bodies are parameterized by the actual before/after pair in §7.3, with worked
examples for both the no-default-role and default-role-set cases.

### 2.2 Lockout guard

A write that would leave the acting admin no longer admin is refused, independent of the
posture guards above. The copy says the check is against the admin's **last sign-in** (its
role is a stamped cookie, not live), and says plainly that the **admin token is never bound**
by this check — it is the recovery path if an admin locks themselves out some other way.

The refusal is **server-authoritative, not a client pre-check**: there is nothing in the
value/role a form can inspect client-side to know a delete would lock the acting admin out
(that requires knowing whose session this is server-side), so the Delete control stays
enabled and the confirm dialog goes through the normal flow. The 400 comes back only after the
attempt; `LOCKOUT_ERROR` renders in the dialog at that point, in place of (or alongside) the
plain `DELETE_CONFIRM` body. §7.4's inset is drawn post-attempt for exactly this reason — see
the note there.

### 2.3 States

- **SSO not configured (503)** — the role-mappings block's own fetch can 503 if SSO isn't
  configured server-side even though the client briefly renders the multi-user branch (a
  stale `status` on first load). This is distinct from single-user mode itself: **the
  single-user branch of the step is unchanged** (§6) — a genuinely single-user install never
  reaches this block at all.
- **Fetch-failed** — the mappings list failed to load, distinct from SSO-not-configured: the
  chart's mappings still apply, this panel just can't confirm them right now.
- **Empty console list** — the chart may or may not have rows; no console rows have been added
  here yet. Not a guard state (nothing narrows on its own from an empty-console view when
  guard preconditions aren't met).

## 3. Do not design (out of scope this round)

- No editing of `WARDYN_OIDC_DEFAULT_ROLE` or `WARDYN_OIDC_OPERATOR_EMAILS` from the console —
  read-only display only.
- No capability-grant UI here — that is `/permissions`, unchanged, linked via the existing
  button.
- No IdP-side actions (creating people/groups, App Role assignment) — explained, never
  editable.
- No structural change to setup routing, the phase rail, or the step order — see §6.

## 4. Design system

Same token block and CSS idioms as `docs/design/permissioning-mock/index.html` (`--background`
/ `--card` / `--surface-2` / `--foreground` / `--muted-foreground` / `--border` /
`--border-strong` / `--primary` / `--success` / `--warning` / `--danger` / `--info`, light and
dark, Inter + JetBrains Mono, `chip`/`btn`/`sw`/`seg`/`dlg`/`note`/`card` idioms). One
deliberate departure: **zero teal on this step.** `DeploymentStep`'s existing rule is that the
footer's Next is the surface's one affirmative action, so every button in the step body is
`outline` — this round keeps that rule for every new control (`.btn`, never `.btn.primary`).
Correspondingly, **the "grants are amber" convention from `/permissions` does not apply here**
— `admin`/`member` are roles, not capability grants, so role chips are neutral, not amber.
Amber/red/blue are still used for genuine risk and error states (guard bodies, lockout,
collision, shadowed, SSO-unavailable, fetch-failed) per `CONSOLE-RULES.md` §2's general
state-color convention — that convention is unrelated to and unaffected by the
grants-specific amber rule. One exception to "zero teal, everything else neutral": the
existing `MULTI_USER_SSO_CHIP` keeps its **success tone** (green dot) — that is
`DeploymentStep`'s status-of-the-connection, unrelated to the role/grant coloring rule above,
already filmed that way in 02c/04c, and this round doesn't touch it.

## 5. Hard canon constraints (episode 04c already filmed against this step)

1. The step heading stays exactly **"Who can sign in"**.
2. `/setup?step=people` keeps working — no structural change to routing is implied.
3. The phase rail's `people` badge (`steps.ts`'s `stepBadges.people`, text `"Multi-user"` /
   `"Single-user"`, unchanged) is the **first** "Multi-user" text node on the page — it renders
   before the step body. The step body's own `PEOPLE_STEP.MULTI_USER_CHIP` chip (also literally
   "Multi-user") is fine because it lands after the rail badge in the DOM, not because it is
   worded differently. The mock includes a small rail-preview strip above the step frame so
   this ordering is visible, not just asserted.
4. The "Open Permissions" button and its hint stay, unchanged — including its **element**:
   `<Button asChild variant="outline" size="sm"><Link to="/permissions">{PT.MULTI_USER_PERMISSIONS_ACTION}</Link></Button>`
   (`step-bodies.tsx:347-349`), which renders `role=link`, not a button. This is not a styling
   detail — see the assertion sites below. The mock's
   `<a class="btn sm" href="#">Open Permissions</a>` stands in for that real anchor-semantics
   element, not a `<button>`.

Every `PEOPLE_STEP` string is preserved as-is **except one flagged change** — see §7.1.

**Assertion sites this step's copy and structure are already pinned to** — a change here that
isn't reflected in these four is a broken test, not a free edit:

- `ui/e2e/demo/04c-who-may-do-what.spec.ts:58` — `getByRole("heading", { name: "Who can sign in" })`.
- `ui/e2e/demo/04c-who-may-do-what.spec.ts:59` — `getByText("Multi-user").first()` (canon
  constraint #3's DOM-order requirement, above).
- `ui/e2e/demo/04c-who-may-do-what.spec.ts:63` — `getByRole("link", { name: "Open Permissions" })`
  (canon constraint #4/F-1, above — this is the spec that breaks if the button ever stops
  rendering `role=link`).
- `ui/e2e/demo/02c-one-command-to-a-cluster.spec.ts:100-102` — same heading + `"Multi-user"`
  text assertions, on the same step, from a different episode.
- `ui/e2e/setup-gate.spec.ts:60` — `openPermissionsFromPeople`'s
  `getByRole("heading", { name: "Who can sign in" })`, immediately followed at `:61` by its
  `getByRole("link", { name: "Open Permissions" })` click — the pair that gets this helper onto
  `/permissions` from People in the first place.
- `ui/src/app/components/screens/setup/setup-screen.test.tsx:408` — asserts exactly **one**
  `"Who can sign in"` heading (`STEP_HEADING.people` plus `DeploymentStep`'s deliberately
  title-less card must never both render the string as a heading).

Implementation is not done when it builds and unit-tests pass: run `scripts/run-ui-e2e.sh` and
the `setup-screen` unit test before calling it done — the UI e2e suite is daemon-only and is
**not** part of `make ci`.

## 6. Where the model lives on the page

Inside the existing multi-user branch, after the existing Admins/Members card (unchanged),
in this order:

1. Role mappings table (chart rows + console rows, source column, shadowed badges, defaults
   panel) + add form.
2. Preview panel.
3. IdP-duties note.

Single-user branch: **unchanged**, byte-for-byte (§7.1).

## 7. Canonical strings — FROZEN

Throughout §7, a backticked substring inside a frozen string (an env var, a claim name, a
const) renders `font-mono` in the console and in the mock — apply the mono span uniformly
everywhere that substring recurs, not only on its first appearance in a given string.

### 7.1 Retained from `PEOPLE_STEP` — unchanged unless flagged

All of `ui/src/app/components/wardyn/copy.ts:615-640` is preserved verbatim, **with one
flagged change**:

| Key | Status | String |
|---|---|---|
| `SINGLE_USER_CHIP` | unchanged | Single-user |
| `SINGLE_USER_LEDE_LOCAL` | unchanged | One admin credential — no sign-in at all — anyone who reaches this console on this machine is the admin. No per-person identity. |
| `SINGLE_USER_LEDE_TOKEN` | unchanged | One admin credential — the token the installer printed. No per-person identity. |
| `SINGLE_USER_BODY` | unchanged | Just you. Whoever holds the admin token (or reaches a local-mode console) is the admin; runs, policies, secrets and approvals are all yours. There is no member role until people sign in as themselves. |
| `SINGLE_USER_SSO_NOTE_PREFIX` / `_DOC` / `_SUFFIX` | unchanged | To add people, configure SSO: each person gets their own identity and an admin or member role, and members get their own Getting Started. The recipe is in `docs/OPERATIONS.md`, "Second user, same host". |
| `MULTI_USER_CHIP` | unchanged | Multi-user |
| `MULTI_USER_LEDE` | unchanged | People sign in with SSO; each is an admin or a member, per your role map. |
| **`MULTI_USER_ROLES_PREFIX`** | **CHANGED** | `"Roles come from the mappings below — your chart's "` (note: ends with one space — see below) |
| `MULTI_USER_ROLES_VAR` | unchanged (still mono-rendered) | `WARDYN_OIDC_ROLE_MAP` |
| **`MULTI_USER_ROLES_SUFFIX`** | **CHANGED** | , console rows added here, or the operator allowlist. |
| `MULTI_USER_SSO_CHIP` | unchanged | SSO |
| `MULTI_USER_ADMINS_LABEL` / `_BODY` | unchanged | **Admins** set the ceiling — policies, secrets, workspaces, site configuration. |
| `MULTI_USER_MEMBERS_LABEL` / `_BODY` | unchanged | **Members** run inside it — their own workspaces, runs, approvals and SSH keys. They land on their own Getting Started the first time they sign in. |
| `MULTI_USER_PERMISSIONS_ACTION` | unchanged (hard canon) | Open Permissions |
| `MULTI_USER_PERMISSIONS_HINT` | unchanged (hard canon) | Capability grants, per person or group |
| `STEP_HEADING.people` | unchanged (`steps.ts`, real code — not part of `PEOPLE_STEP`, listed here per §5 #1) | Who can sign in |

**Why the one change:** `MULTI_USER_ROLES_PREFIX`/`_SUFFIX` today read as one sentence —
"Roles come from `WARDYN_OIDC_ROLE_MAP`, or the operator allowlist" — which was true when the
role map had exactly one source. It is now factually incomplete: a role can also come from a
console row. Leaving it unedited would have the step's own lede paragraph contradict the table
directly beneath it. `MULTI_USER_ROLES_VAR` (the mono-rendered env var name) is untouched.
`MULTI_USER_ROLES_PREFIX`'s new value ends with one trailing space (before
`MULTI_USER_ROLES_VAR` is concatenated in) — backticked above so the table itself doesn't eat
it; the implementation copies the literal `" "` at the end, not a trimmed string.

Phase-rail badge (`steps.ts`, unchanged, not part of `PEOPLE_STEP`): `"Multi-user"` /
`"Single-user"`, tone neutral — this is the text hard canon constraint #3 (§5) protects.

### 7.2 `PEOPLE` — the role-mappings editor

| Key | String |
|---|---|
| `TABLE_TITLE` | Role mappings |
| `TABLE_LEAD` | A value — an Entra App Role, a groups-claim entry, or an email — mapped to admin or member. |
| `EFFECT_NOTE` | Takes effect at next sign-in. A person already signed in keeps the role they were given until then. |
| `COL_VALUE` | Value |
| `COL_ROLE` | Role |
| `COL_SOURCE` | Source |
| `COL_ADDED` | Added |
| `ROLE_ADMIN` | Admin |
| `ROLE_MEMBER` | Member |
| `SOURCE_CHART` | From your chart |
| `SOURCE_CONSOLE` | Console |
| `CHART_HINT` | Edit in your chart values. |
| `ADDED_CHART_NA` | — |
| `SHADOWED_BADGE` | Shadowed by your chart |
| `SHADOWED_BODY` | Your chart now maps this value too, and the chart always wins. This row is stored but has no effect until you remove the chart entry or delete this row. |
| `SHADOWED_OPERATOR_BADGE` | Shadowed by your operator allowlist |
| `SHADOWED_OPERATOR_BODY` | This value is on your chart's `WARDYN_OIDC_OPERATOR_EMAILS` and always resolves to admin. The row is stored but has no effect until the allowlist entry or this row is removed. |
| `EMAIL_KEY_BADGE` | Unverified claim |
| `EMAIL_KEY_BODY` | This is an email-keyed mapping riding an unverified IdP claim (`WARDYN_OIDC_EMAIL_DOMAINS` is unset, so `email_verified` isn't enforced). Prefer an App Role or group instead. |
| `ADD_TITLE` | Add a mapping |
| `ADD_CTA` | Add mapping |
| `FIELD_VALUE` | Value |
| `VALUE_HINT` | An Entra App Role or groups-claim value exactly as your identity provider sends it in the token, or an email address. Matched case-insensitively. |
| `FIELD_ROLE` | Role |
| `DELETE` | Delete |
| `DELETE_CONFIRM(value)` | Delete the mapping for "{value}"? At their next sign-in, they fall through to whatever the rest of your map resolves to. |
| `CANCEL` | Cancel |
| `EMPTY_TITLE` | No mappings added here |
| `EMPTY_BODY` | Add one for a person or group. If your chart already maps someone, they're listed above and don't need a row here. |
| `DEFAULTS_TITLE` | Defaults |
| `DEFAULT_ROLE_LABEL` | Default role |
| `DEFAULT_ROLE_UNSET` | Unset — anyone matching no mapping is denied at sign-in. |
| `DEFAULT_ROLE_HINT` | Applies to anyone who matches no mapping above. Set in your chart's `WARDYN_OIDC_DEFAULT_ROLE`. |
| `OPERATOR_EMAILS_LABEL` | Operator allowlist |
| `OPERATOR_EMAILS_HINT` | Always admin, on top of any mapping above. Set in your chart's `WARDYN_OIDC_OPERATOR_EMAILS`. |
| `OPERATOR_EMAILS_EMPTY` | None set. |
| `IDP_NOTE` | Creating people and groups, and assigning Entra App Roles, happens in your identity provider — mapping a role here only tells Wardyn what to do with a value your IdP already sends. "Assignment required" on the app registration is Entra's gate, not this one's. |

Value shown for `DEFAULT_ROLE_LABEL` when set: `ROLE_ADMIN` or `ROLE_MEMBER`, reused as-is (no
separate string) — see Q2 (§9) on the display treatment.

**Casing rule:** `{role}`/`{defaultRole}` interpolations inside a sentence (§7.3, §7.5, §7.7)
are lowercase — `admin`/`member` — the way prose names a role mid-sentence. `ROLE_ADMIN` /
`ROLE_MEMBER` above are the chip/label forms only (`Admin`/`Member`, title case) and are never
interpolated into a sentence as-is.

`COL_ADDED`'s chart-row cells show `ADDED_CHART_NA` (chart rows have no console-tracked add
time). Console-row cells show a relative timestamp — reuse the existing
`relativeTime` helper (`ui/src/app/lib/format.ts:6`, already used by `runs.tsx`,
`approvals.tsx`, `policies.tsx` and others for the same "3 days ago" idiom) rather than adding
a second one.

### 7.3 Posture-flip guards — fires iff the unmatched/non-allowlisted outcome changes (§2.1)

`before`/`after` below are each one of the outcome phrases below, computed as §2.1 describes
(`HasOperatorEmails()`, `DefaultRole()`). The guard is never shown for a `before`/`after` pair
that come out equal — that pair is a no-op for the person this guard is about.

| Key | String |
|---|---|
| `FIRST_ROW_TITLE` | This first mapping changes who gets in |
| `FIRST_ROW_BODY(before, after)` | Today, anyone matching no mapping signs in as {before}. After this mapping, they will {after} at their next sign-in. |
| `LAST_ROW_TITLE` | Removing the last mapping changes who gets in |
| `LAST_ROW_BODY(before, after)` | Today, anyone matching no mapping {before}. After this removal, they will {after} at their next sign-in. |
| `ALLOWLIST_NOTE` | People on your operator allowlist stay admins either way. |
| `GUARD_ACK_LABEL` | I understand this changes who can sign in. |
| `FIRST_ROW_CONFIRM` | Add mapping |
| `LAST_ROW_CONFIRM` | Delete mapping |

`FIRST_ROW_BODY`'s `{before}` is the empty-map outcome (§2.1) written as a noun phrase — `a
member` (allowlist non-empty) or `an admin` (allowlist empty) — and its `{after}` is the
non-empty-map outcome written as a verb phrase — `be denied` (default role unset), `sign in as
a member` (default role = member), or `sign in as an admin` (default role = admin).
`LAST_ROW_BODY` uses the same two outcome-phrase sets in the opposite roles and tense: its
`{before}` is the (still-current, non-empty-map) outcome as a present-tense clause — `is denied
at sign-in`, `signs in as a member`, or `signs in as an admin` — and its `{after}` is the
(post-deletion, empty-map) outcome as a verb phrase — `sign in as a member` or `sign in as an
admin` (never `be denied`: an empty map always resolves via §2.1's arm 1, which never denies).
`ALLOWLIST_NOTE` is appended (on its own line, after the body) whenever `HasOperatorEmails()`
is true — both `before`'s `a member` value and `after`'s `sign in as a member` value only ever
occur when the allowlist is set, so this is exactly the condition under which the note is
relevant.

Worked examples, spelled out in full (the four combinations that actually fire — the other
two, admin→admin and member→member, are the no-op pairs the guard suppresses and are never
shown). The two **set** rows are the ones drawn in the mock's four posture-flip insets,
byte-for-byte:

- **allowlist empty, `DEFAULT_ROLE` unset:**
  `FIRST_ROW_BODY`: "Today, anyone matching no mapping signs in as an admin. After this
  mapping, they will be denied at their next sign-in."
  `LAST_ROW_BODY`: "Today, anyone matching no mapping is denied at sign-in. After this removal,
  they will sign in as an admin at their next sign-in."
- **allowlist empty, `DEFAULT_ROLE=member`:**
  `FIRST_ROW_BODY`: "Today, anyone matching no mapping signs in as an admin. After this
  mapping, they will sign in as a member at their next sign-in."
  `LAST_ROW_BODY`: "Today, anyone matching no mapping signs in as a member. After this removal,
  they will sign in as an admin at their next sign-in."
- **allowlist set, `DEFAULT_ROLE` unset:**
  `FIRST_ROW_BODY`: "Today, anyone matching no mapping signs in as a member. After this
  mapping, they will be denied at their next sign-in." + `ALLOWLIST_NOTE`
  `LAST_ROW_BODY`: "Today, anyone matching no mapping is denied at sign-in. After this removal,
  they will sign in as a member at their next sign-in." + `ALLOWLIST_NOTE`
- **allowlist set, `DEFAULT_ROLE=admin`** (the **silent-widening** case F-16 exists to catch):
  `FIRST_ROW_BODY`: "Today, anyone matching no mapping signs in as a member. After this
  mapping, they will sign in as an admin at their next sign-in." + `ALLOWLIST_NOTE`
  `LAST_ROW_BODY`: "Today, anyone matching no mapping signs in as an admin. After this removal,
  they will sign in as a member at their next sign-in." + `ALLOWLIST_NOTE`

(Suppressed, no guard shown: allowlist empty + `DEFAULT_ROLE=admin` — admin→admin; allowlist
set + `DEFAULT_ROLE=member` — member→member.)

This supersedes the two flat `defaultRole`-only bodies and the prior "assumption flagged for
adjudication" note from an earlier draft of this table: `LAST_ROW_BODY`'s empty-map fallback
(`a member`/`an admin`, driven by `HasOperatorEmails()`) is §2.1's arm 1, already the
documented `deriveRole` behavior — nothing here still needs owner adjudication on that point.
What the prior note actually flagged — whether `DEFAULT_ROLE` should apply retroactively at a
combined map of exactly zero rows — is a separate, real open question, promoted to **Q8** in
§9 rather than buried in a table footnote.

### 7.4 Errors — collision and lockout

| Key | String |
|---|---|
| `COLLISION_ERROR_CHART(value)` | "{value}" is already mapped in your chart's `WARDYN_OIDC_ROLE_MAP` — the chart always wins, so a console row here would only ever be shadowed. Edit your chart values instead. |
| `COLLISION_ERROR_OPERATOR(value)` | "{value}" is already on your chart's operator allowlist (`WARDYN_OIDC_OPERATOR_EMAILS`) and always resolves to admin — a console row here would have no effect. Edit your chart values instead. |
| `LOCKOUT_ERROR` | This change would leave you without admin access, checked against your last sign-in — refused. The admin token is never bound by this check, so it stays your recovery path if you lock out any other way. |

`LOCKOUT_ERROR` is drawn **post-attempt** (§2.2): the Delete control is never disabled
pre-emptively (the client has nothing to check client-side), so the mock's lockout inset shows
the confirm dialog with Delete still enabled and `LOCKOUT_ERROR` rendered below the body as the
result of a completed, refused attempt — not a pre-check state.

### 7.5 `PREVIEW` — dry-run panel

| Key | String |
|---|---|
| `TITLE` | Preview a sign-in |
| `LEAD_PREFIX` | Paste the roles, groups, or email a person's token would carry, or |
| `OWN_SESSION_CTA` | test with my own session |
| `LEAD_SUFFIX` | , to see the role they'd derive at their next sign-in. Nothing here is saved. |
| `FIELD_CLAIMS` | Roles, groups, or email |
| `FIELD_CLAIMS_HINT` | One value per line — an App Role, a group name, or an email address. |
| `RUN_CTA` | Preview |
| `RESULT_MATCHED(role, matched)` | Would sign in as {role} — matched by {matched}. |
| `RESULT_DEFAULT(role)` | Would sign in as {role} — nothing matched, so your default role applies. |
| `RESULT_DENIED` | Would be denied at sign-in — nothing matched, and no default role is set. |
| `RESULT_LEGACY(role)` | Would sign in as {role} — no mappings are configured; the operator allowlist decides. |
| `RESULT_UNKNOWN` | Couldn't check this against your mappings — try again. |

`RESULT_MATCHED`/`RESULT_DEFAULT`/`RESULT_DENIED` cover a non-empty combined map (`ok=true`
via a `MatchSourceMapRow`/`MatchSourceOperatorAllowlist` match, `ok=true` via
`MatchSourceDefaultRole`, and `ok=false`, respectively — `PreviewRole`, `derive.go`).
`RESULT_LEGACY` is the distinct empty-map arm (§2.1's arm 1): with no chart rows and no console
rows, `PreviewRole` never reaches `DefaultRole()` at all — the role comes from
`HasOperatorEmails()`/`emailInList` alone, so this result explicitly says the map is unused
rather than implying a `DEFAULT_ROLE` decided it.

### 7.6 States

| Key | String |
|---|---|
| `SSO_UNAVAILABLE_TITLE` | SSO isn't configured on this deployment |
| `SSO_UNAVAILABLE_BODY` | Role mappings need SSO to mean anything — there's no per-person identity to map without it. This is likely a stale view; reload to see the single-user explainer instead. |
| `FETCH_FAILED_TITLE` | Couldn't load role mappings |
| `FETCH_FAILED_BODY` | Something went wrong reaching the server. Your chart's mappings still apply even though this list can't confirm them right now. |
| `FETCH_FAILED_RETRY` | Retry |

### 7.7 `SIGNIN` — re-worded `auth_error` arms (`sign-in.tsx`'s `authErrorMessage`)

Only the two arms named in this round, plus one new arm. Every other `auth_error` case
(`email_unverified`, `email_domain`, `oidc_transient`, `oidc_config`, default) is unchanged and
out of scope.

| Key (auth_error code) | Today | Reworded |
|---|---|---|
| `no_role` | Your account has no Wardyn role assigned. Ask an operator to map your role (`WARDYN_OIDC_ROLE_MAP`) or add your email to `WARDYN_OIDC_OPERATOR_EMAILS`. | Your account has no Wardyn role assigned. Ask your Wardyn admin to add you to a role mapping (`WARDYN_OIDC_ROLE_MAP` or `WARDYN_OIDC_OPERATOR_EMAILS`, or the equivalent on the People step). |
| `email_verified_absent` | Your identity provider doesn't send an `email_verified` claim at all (common on Entra ID), so this console can't confirm the email on its own. Ask an operator to map your role via `WARDYN_OIDC_ROLE_MAP` (an App Role or group, not email domains) instead. | Your identity provider doesn't send an `email_verified` claim at all (common on Entra ID), so this console can't confirm the email on its own. Ask your Wardyn admin to map your role by App Role or group instead (`WARDYN_OIDC_ROLE_MAP`, or the People step). |
| `role_check_unavailable` (new) | — | Couldn't check your access — try again, or contact your admin. |

The rule both reworded arms now follow: name **"your Wardyn admin"** as who to ask, with the
env var(s) demoted to a parenthetical for the operator who actually has to act on it — a
signed-in human should never be told to go set an env var themselves.

### 7.8 `sso_rbac` setup-check (`internal/api/setup_checks.go`'s `ssoRBACCheck`) — reworded

| State | Field | Today | Reworded |
|---|---|---|---|
| ok | `Detail` | `WARDYN_OIDC_ROLE_MAP` is set: signed-in humans are assigned admin/member from their IdP roles/groups/email. | Role mapping is configured — from `WARDYN_OIDC_ROLE_MAP`, the People step, or both — so signed-in humans are assigned admin/member from their IdP roles/groups/email. |
| warn | `Detail` | `WARDYN_OIDC_ROLE_MAP` is not set: every SSO user is an admin. | No role mapping is configured — neither `WARDYN_OIDC_ROLE_MAP` in your chart nor a mapping added on the People step — so every SSO user is an admin, unless your operator allowlist already splits admins from members. |
| warn | `Fix` | Set `WARDYN_OIDC_ROLE_MAP` (helm: `env.WARDYN_OIDC_ROLE_MAP`) to map IdP roles/groups/emails to "admin" or "member". | Set `WARDYN_OIDC_ROLE_MAP` (helm: `env.WARDYN_OIDC_ROLE_MAP`), or add a mapping on the People step (Setup → People → Role mappings), to map IdP roles/groups/emails to "admin" or "member". |

**Plumbing note (not a copy question):** `ssoRBACCheck(oidcConfigured, roleMapConfigured
bool)` is a pure function today — it has no way to know whether any console rows exist, only
whether the chart's `WARDYN_OIDC_ROLE_MAP` is set. Reflecting "or a mapping added on the People
step" in the warn/ok split above requires widening its signature (e.g. an added
`consoleRowCount int`, `roleMapConfigured` becoming true when either the chart map or that
count is non-empty) — the pure check itself cannot reach the console's row store, so the
setup-status assembly point that calls it (wherever `SetupCheck`s are collected) is what plumbs
the count in. The reworded `Detail`/`Fix` above describe the check's OUTPUT once that wiring
exists; they do not by themselves imply the wiring already does.

## 8. Where to apply (once implemented, out of scope this round)

- People step, multi-user branch — the whole editor (§6).
- Sign-in screen — `SIGNIN.*` (§7.7).
- `sso_rbac` setup check — `internal/api/setup_checks.go` (§7.8).
- `/permissions` — unchanged, only the outbound link from People stays.

## 9. Owner question list

**Q1.** Step order: keep `environment → people → corp_network → integrations` (already
matches the target flow) vs swap `people ↔ corp_network`. **Recommend keep** — the mock
changes nothing about routing or order (hard canon #2), and nothing in this model depends on
`corp_network` running first or last relative to People.

**Q2.** Default-role display treatment: plain read-only line vs posture callout. The mock
shows the plain line (§7.2 `DEFAULT_ROLE_LABEL` + value). **Recommendation:** plain line when
the default is `member` or unset (both are the safe end), escalate to a warning-toned callout
only when the default is `admin` — that is the one setting that silently admins every
unmatched sign-in. Not mocked as a separate inset (it is a conditional treatment of the same
row, not a new state); described here instead.

**Q3.** Chart rows inline in one table (with a source column) vs a separate "From your chart"
block. **Mocked both, now at full string parity** — both variants draw every §7.2 string a
row can carry (added-time, shadowed badge + body, unverified-claim badge + body), not just the
value/role pair; only the layout differs. **Recommendation: the merged single table** — it
matches `/permissions`' own precedent (one grants table with a subject-type column, not a
split-by-subject layout) and gives the admin one place to scan for a given value instead of
two.

**Q4.** 04c re-take stance: footage shows the old (pre-round) explainer visuals; narration
stays true. **Recommendation:** no re-take yet. This is a mock round — nothing shipped
changes today, so 04c's recorded visuals are still exactly what the console shows. Re-take
once the A-E implementation stage actually lands the editor, not before.

**Q5.** Include the F-12 cockpit fix (member egress decides on own-run inline approvals) in
this campaign. **Recommend yes** — it's an existing, already-decided fix orthogonal to this
model (approvals authorization, not role mapping), and bundling it avoids a second setup pass
through the same review loop.

**Q6.** People done-ness: recommend stays done-on-arrival (orchestrator-OR precedent is
monotone-true; subtractive override = new pattern + flicker risk) — named alternative if
owner wants real done-ness. **Alternative, if wanted:** gate `stepDone.people` on multi-user
mode having at least one working role resolution (a chart row, a console row, or a default
role set) — i.e. real done-ness would require a fetch this pure function doesn't currently
take. That is a genuinely new pattern for this codebase (every other `stepDone` entry is
monotone — once true, an unrelated later change can't flip it back false) and risks a
done-checkmark flicker false→true→false as the mappings fetch resolves. Staying
done-on-arrival avoids both costs.

**Q7.** Email-keyed console rows: warn-badge (mirroring the boot warning for env email keys
when `WARDYN_OIDC_EMAIL_DOMAINS` is unset) vs refuse email keys outright. **Recommend
warn-badge** (`EMAIL_KEY_BADGE` / `EMAIL_KEY_BODY`, §7.2) — refusing outright would make email
a second-class value type inconsistent with `WARDYN_OIDC_ROLE_MAP` itself, which has always
accepted email keys with the same caveat stated as a warning, never a refusal
(`cmd/wardynd/boot_deps.go`'s existing boot warning is the precedent this badge mirrors).

**Q8.** Should `WARDYN_OIDC_DEFAULT_ROLE` govern a combined role map that is currently exactly
zero rows (chart empty, console emptied back to nothing), the same way it governs a non-empty
map with no match? Today's actual boot-time rule (`docs/ENV.md`) is unconditional: an unset
`WARDYN_OIDC_ROLE_MAP` makes every SSO user admin, full stop, with `WARDYN_OIDC_DEFAULT_ROLE`
explicitly *ignored* in that case ("Ignored when `WARDYN_OIDC_ROLE_MAP` is itself empty") — §2.1
and §7.3's `LAST_ROW_BODY` state exactly this current behavior, not a new one.
**Recommendation: keep current behavior** — `DEFAULT_ROLE` stays ignored at an empty combined
map. Honoring it there would silently change the outcome for every already-running arm-1
install (no role map, no console rows, `DEFAULT_ROLE` set from an earlier, now-inert config)
the moment this feature ships, with no write and no acknowledgement from that install's admin —
exactly the kind of unannounced posture flip §2.1's guards exist to prevent, not create. Cross-
reference: §7.3's worked-examples table already assumes this answer (its empty-map `{before}`/
post-deletion `{after}` values come from `HasOperatorEmails()` alone, never `DefaultRole()`).

## Adjudication

<!-- Owner answers go here. -->
