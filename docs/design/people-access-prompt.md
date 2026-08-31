# People — the acting surface for who can sign in

This is the mock round for the People acting surface — the design gate before any console
code (owner law: the mock is UI source of truth; canon strings are app strings). The model
below is decided and converged; nothing here is open for re-design, only for drawing. Seven
open calls remain and are listed as Q1–Q7 in §9, with an empty Adjudication section at the
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
console row; when that happens the chart wins (same precedence as boot) and the console row
renders with a "shadowed by your chart" badge instead of disappearing, so the admin can see
and clean it up.

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

Two writes change what happens to everyone who **doesn't** match anything, and both guards
fire **only when the chart map is empty** — a non-empty chart already provides real gating, so
these are the two moments a console-only deployment can flip its whole default posture with
one row:

- **First console row added** (chart empty, no console rows yet): adding it turns role
  derivation on for the first time. Before, with the whole map empty, Wardyn's legacy
  boot-time rule applies (`WARDYN_OIDC_ROLE_MAP` unset → every SSO user is admin, per
  `docs/ENV.md`). After, everyone who signs in and matches nothing falls to
  `WARDYN_OIDC_DEFAULT_ROLE`, or is denied if that's unset too — a real narrowing the write
  must say out loud before it happens, with an explicit acknowledgement control.
- **Last console row deleted** (chart empty, this is the only remaining row): deleting it
  empties the map back to nothing, same shape in reverse, same acknowledgement control.

Both guard bodies are written in §7.3 with an explicit assumption flagged inline — see the
note under that table.

### 2.2 Lockout guard

A write that would leave the acting admin no longer admin is refused, independent of the
posture guards above. The copy says the check is against the admin's **last sign-in** (its
role is a stamped cookie, not live), and says plainly that the **admin token is never bound**
by this check — it is the recovery path if an admin locks themselves out some other way.

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
grants-specific amber rule.

## 5. Hard canon constraints (episode 04c already filmed against this step)

1. The step heading stays exactly **"Who can sign in."**
2. `/setup?step=people` keeps working — no structural change to routing is implied.
3. The phase rail's `people` badge (`steps.ts`'s `stepBadges.people`, text `"Multi-user"` /
   `"Single-user"`, unchanged) is the **first** "Multi-user" text node on the page — it renders
   before the step body. The step body's own `PEOPLE_STEP.MULTI_USER_CHIP` chip (also literally
   "Multi-user") is fine because it lands after the rail badge in the DOM, not because it is
   worded differently. The mock includes a small rail-preview strip above the step frame so
   this ordering is visible, not just asserted.
4. The "Open Permissions" button and its hint stay, unchanged.

Every `PEOPLE_STEP` string is preserved as-is **except one flagged change** — see §7.1.

## 6. Where the model lives on the page

Inside the existing multi-user branch, after the existing Admins/Members card (unchanged),
in this order:

1. Role mappings table (chart rows + console rows, source column, shadowed badges, defaults
   panel) + add form.
2. Preview panel.
3. IdP-duties note.

Single-user branch: **unchanged**, byte-for-byte (§7.1).

## 7. Canonical strings — FROZEN

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
| **`MULTI_USER_ROLES_PREFIX`** | **CHANGED** | Roles come from the mappings below — your chart's |
| `MULTI_USER_ROLES_VAR` | unchanged (still mono-rendered) | `WARDYN_OIDC_ROLE_MAP` |
| **`MULTI_USER_ROLES_SUFFIX`** | **CHANGED** | , console rows added here, or the operator allowlist. |
| `MULTI_USER_SSO_CHIP` | unchanged | SSO |
| `MULTI_USER_ADMINS_LABEL` / `_BODY` | unchanged | **Admins** set the ceiling — policies, secrets, workspaces, site configuration. |
| `MULTI_USER_MEMBERS_LABEL` / `_BODY` | unchanged | **Members** run inside it — their own workspaces, runs, approvals and SSH keys. They land on their own Getting Started the first time they sign in. |
| `MULTI_USER_PERMISSIONS_ACTION` | unchanged (hard canon) | Open Permissions |
| `MULTI_USER_PERMISSIONS_HINT` | unchanged (hard canon) | Capability grants, per person or group |

**Why the one change:** `MULTI_USER_ROLES_PREFIX`/`_SUFFIX` today read as one sentence —
"Roles come from `WARDYN_OIDC_ROLE_MAP`, or the operator allowlist" — which was true when the
role map had exactly one source. It is now factually incomplete: a role can also come from a
console row. Leaving it unedited would have the step's own lede paragraph contradict the table
directly beneath it. `MULTI_USER_ROLES_VAR` (the mono-rendered env var name) is untouched.

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
| `SHADOWED_BADGE` | Shadowed by your chart |
| `SHADOWED_BODY` | Your chart now maps this value too, and the chart always wins. This row is stored but has no effect until you remove the chart entry or delete this row. |
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

### 7.3 Posture-flip guards — only when the chart map is empty

| Key | String |
|---|---|
| `FIRST_ROW_TITLE` | Add the first mapping? |
| `FIRST_ROW_BODY(defaultRole)` | Anyone matching no mapping will be made {defaultRole} at next sign-in. |
| `FIRST_ROW_BODY` — no default role set | Anyone matching no mapping will be denied at next sign-in. |
| `LAST_ROW_TITLE` | Delete the last mapping? |
| `LAST_ROW_BODY(defaultRole)` | Everyone who signs in becomes {defaultRole} at next sign-in. |
| `LAST_ROW_BODY` — no default role set | Everyone who signs in becomes admin at next sign-in — the same legacy behavior as an unconfigured role map. |
| `GUARD_ACK_LABEL` | I understand this changes who can sign in. |
| `FIRST_ROW_CONFIRM` | Add mapping |
| `LAST_ROW_CONFIRM` | Delete mapping |

**Assumption flagged for adjudication:** `LAST_ROW_BODY`'s no-default-role branch assumes
`WARDYN_OIDC_DEFAULT_ROLE` governs even a fully-emptied combined map. Today's actual boot-time
rule (`docs/ENV.md`) is unconditional: an unset `WARDYN_OIDC_ROLE_MAP` makes every SSO user
admin, full stop, with `WARDYN_OIDC_DEFAULT_ROLE` explicitly *ignored* in that case ("Ignored
when `WARDYN_OIDC_ROLE_MAP` is itself empty"). Whether the backend can and should distinguish
"never configured" from "console-managed and currently empty" — so the console honors
`DEFAULT_ROLE` even at zero rows — is a real implementation question, not just a copy one. The
guard body above states the actual current-code outcome (admin, unconditionally); it does not
invent a new backend behavior. Flagged, not silently resolved.

### 7.4 Errors — collision and lockout

| Key | String |
|---|---|
| `COLLISION_ERROR_CHART(value)` | "{value}" is already mapped in your chart's `WARDYN_OIDC_ROLE_MAP` — the chart always wins, so a console row here would only ever be shadowed. Edit your chart values instead. |
| `COLLISION_ERROR_OPERATOR(value)` | "{value}" is already on your chart's operator allowlist (`WARDYN_OIDC_OPERATOR_EMAILS`) and always resolves to admin — a console row here would have no effect. Edit your chart values instead. |
| `LOCKOUT_ERROR` | This change would leave you without admin access, checked against your last sign-in — refused. The admin token is never bound by this check, so it stays your recovery path if you lock out any other way. |

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
| `RESULT_DENIED` | Would be denied at sign-in — nothing matched, and no default role is set. |
| `RESULT_UNKNOWN` | Couldn't check this against your mappings — try again. |

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
| `role_lookup_failed` (new) | — | Couldn't check your access — try again, or contact your admin. |

The rule both reworded arms now follow: name **"your Wardyn admin"** as who to ask, with the
env var(s) demoted to a parenthetical for the operator who actually has to act on it — a
signed-in human should never be told to go set an env var themselves.

### 7.8 `sso_rbac` setup-check (`internal/api/setup_checks.go`'s `ssoRBACCheck`) — reworded

| State | Field | Today | Reworded |
|---|---|---|---|
| ok | `Detail` | `WARDYN_OIDC_ROLE_MAP` is set: signed-in humans are assigned admin/member from their IdP roles/groups/email. | Role mapping is configured — from `WARDYN_OIDC_ROLE_MAP`, the People step, or both — so signed-in humans are assigned admin/member from their IdP roles/groups/email. |
| warn | `Detail` | `WARDYN_OIDC_ROLE_MAP` is not set: every SSO user is an admin. | No role mapping is configured — neither `WARDYN_OIDC_ROLE_MAP` in your chart nor a mapping added on the People step — so every SSO user is an admin. |
| warn | `Fix` | Set `WARDYN_OIDC_ROLE_MAP` (helm: `env.WARDYN_OIDC_ROLE_MAP`) to map IdP roles/groups/emails to "admin" or "member". | Set `WARDYN_OIDC_ROLE_MAP` (helm: `env.WARDYN_OIDC_ROLE_MAP`), or add a mapping on the People step (Setup → People → Role mappings), to map IdP roles/groups/emails to "admin" or "member". |

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
block. **Mocked both** — cheap, since both variants reuse the identical §7.2 strings and only
change layout. **Recommendation: the merged single table** — it matches `/permissions`' own
precedent (one grants table with a subject-type column, not a split-by-subject layout) and
gives the admin one place to scan for a given value instead of two.

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

## Adjudication

<!-- Owner answers go here. -->
