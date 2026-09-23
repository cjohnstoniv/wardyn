/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Governance copy canon (0.7, governance profiles + the three-tier admin
// model) — the frozen canonical-strings tables from
// docs/design/governance-prompt.md §7.2-§7.9, transcribed verbatim. The
// /governance screen (profiles, assignments, resolved preview), the member's
// two display moments, the positioning slogan and the directory combobox read
// these instead of retyping the copy, so the shipped wording can't drift from
// the reviewed mock.
//
// Pure TS — no React, no fetch, no DOM. Same discipline as permissions-copy.ts
// (permissions.tsx:11-13) and people-access-copy.ts: the components that
// consume this add NO copy of their own.
//
// governance-copy.test.ts PARSES §7.2-§7.9's tables back out of the prompt doc
// and compares all 85 keys below against them, so a swapped hyphen, a dropped
// ellipsis or a new doc row fails a gate instead of shipping.
//
// Backtick-mono rule (§7 header note): a backticked substring inside a frozen
// string (an env var, a wire field, a literal value) is PLAIN TEXT here — the
// mono span is a DISPLAY concern the consuming component applies (the
// access-panel.tsx withMono precedent), uniformly at EVERY recurrence of that
// substring, never baked into the string. A profile NAME is never mono: it is
// a human-chosen label, and the strings below already spell the double quotes
// it is rendered inside.

import { PEOPLE } from "./people-access-copy";
import {
  AUTONOMY_LEVEL_ORDER,
  AUTONOMY_RUBRIC_ROW_KEYS,
  type AutonomyLevel,
  type AutonomyRubric,
  type AutonomyRubricRowKey,
} from "./api/governance";
import { GOVERNANCE_NAV_TITLE } from "./nav-copy";

// §7.1 — reused canon, referenced never re-frozen
//
// These already exist and are IMPORTED, not retyped. Re-exported from here so
// a governance surface has one import site and cannot accidentally grow a
// second home for a string that already has one:
//
//   PERM.COL_WHO / FIELD_WHO / COL_ADDED / SUBJECT_USER / SUBJECT_GROUP /
//   SUBJECT_ALL / HINT_USER / HINT_GROUP / HINT_ALL / REMOVE — the assignments
//     table's "Who" column, its three subject kinds, and their hints.
//   PEOPLE.CANCEL / ROLE_ADMIN / ROLE_MEMBER / FIELD_VALUE / ADD_CTA /
//     FIELD_ROLE.
//   PREVIEW.FIELD_CLAIMS / FIELD_CLAIMS_HINT — the resolved preview takes the
//     claims a token would carry, which is exactly what the People step's
//     preview takes, so it renders those two verbatim (§7.3). Two labels for
//     one accepted shape is how they drift apart.
//   ACCESS_STATE.FETCH_FAILED_RETRY — the Retry beside GOVERNANCE's own
//     FETCH_FAILED_TITLE / _BODY (§7.5).
export { PERM } from "./permissions-copy";
export { ACCESS_STATE, PEOPLE, PREVIEW } from "./people-access-copy";

// The rest of §7.1 is reused AT ITS OWN HOME, not through here — each already
// lives beside the component that draws it, and a re-export would only add an
// import hop: MEMBER_GETTING_STARTED.* and the nav labels (wardyn/copy.ts,
// app-shell.tsx), the setup-layout heading, New Run's "Policy" section title,
// PolicyPanel's saved-policy cards and preflight hint, SafetyMeter's eyebrow +
// grades, relativeTime (lib/format.ts), CC_META labels, the launch-warning
// toast title, and runs_create_validate.go's codex-cli explicit-hold refusal.
//
// DELIBERATELY ABSENT — the strings the SERVER emits. §7.1's second table
// freezes eleven Go format strings that this module does NOT carry, because
// the console renders them FROM THE WIRE, verbatim, and a second copy here
// would be a claim rather than canon:
//   - the still-assigned delete refusal (409, handleDeleteGovernanceProfile)
//     — GOVERNANCE.DELETE_RESTRICT_BODY below is the CLIENT-SIDE PRE-FILL
//       only; it names a count, which is knowable only client-side. On the
//       race path the client believed the count was zero, so it renders the
//       shipped 409, which carries no count and must not grow one (§7.4);
//   - "invalid ceiling: " + governanceGrantWithinCeiling's five cause legs
//     (internal/api/governance_grantbound.go) — free-form prose already naming
//     the failing leg, the grant kind, the secret, the host and both TTLs.
//     GOVERNANCE.GRANT_BOUND_TITLE below is a HEADING over that message, not a
//     replacement for it; freezing CAUSE_* clauses here would have meant
//     specifying a structured cause on the wire for copy polish;
//   - governanceOmissionWarnings' five omission warnings (internal/api/
//     governance.go), returned on the write response's `Warnings []string` as
//     INDEPENDENT list items in the response's own order — which is why
//     GOVERNANCE.OMISSION_TITLE has no OMISSION_BODY twin.
//
// MEMBER below is the one place that shape is inverted: §7.7's eight strings
// are server-composed too, but §7 froze them as canon keys, so they are
// transcribed here as the wording the Go side must emit — the same
// belt-and-braces ACCESS_ERROR.EMAIL_KEY_REFUSED already uses against
// access.go's accessEmailKeyRefused const. See MEMBER's own note.

// §7.2-§7.5 — GOVERNANCE, the admin screen

export const GOVERNANCE = {
  // ---- §7.2 the profiles block ----
  // TITLE is ONE string for two places — the NAV_ITEMS label and the screen
  // heading — the way every other nav entry already works. There is no second
  // "Governance profiles" label. Lives in nav-copy.ts (see its own comment)
  // so app-shell's eager sidebar isn't the reason this whole table ships early.
  TITLE: GOVERNANCE_NAV_TITLE,
  LEAD: "Named ceilings, assigned to people and groups. An assigned profile replaces the deployment ceiling for its subjects; anyone with no assignment keeps the deployment ceiling.",
  PROFILES_TITLE: "Profiles",
  PROFILES_LEAD:
    "A profile is one ceiling: the policy every run under it is bounded by, plus the launch modes its subjects may not use at all.",
  COL_NAME: "Name",
  COL_ASSIGNED: "Assigned to",
  COL_LIMITS: "Limits",
  // COL_GRADE's cell renders the embedded SafetyMeter's own vocabulary
  // (Safety · Safest / Guarded / Elevated / Weakest) and COL_UPDATED the
  // shipped relativeTime helper — both existing canon (§7.1), neither
  // re-frozen here.
  COL_GRADE: "Grade",
  COL_UPDATED: "Updated",
  NEW_CTA: "New profile",
  EDIT: "Edit",
  DELETE: "Delete",
  ASSIGNED_NONE: "Not assigned",
  // Inline pluralisation, the shape PERM.ENFORCE_ON_BODY already uses — not a
  // second pluralisation helper (§7.2).
  ASSIGNED_COUNT: (n: number) => `${n} subject${n === 1 ? "" : "s"}`,
  // ADDITION to §7.2 (R4/F032): the Limits cell must also account for a run
  // quota, not just the three BOOLEAN doors — otherwise a profile whose one
  // limit is a run quota reads "None" while denyMemberRunQuota (internal/api/
  // runs_create_validate.go) still refuses that member's fourth run with a
  // 422. Same inline pluralisation as ASSIGNED_COUNT; the wording tracks the
  // server's own "too many runs at once".
  LIMIT_QUOTA_LABEL: (n: number) => `Max ${n} run${n === 1 ? "" : "s"} at once`,
  LIMITS_NONE: "None",
  EMPTY_TITLE: "No profiles yet",
  EMPTY_BODY: "Everyone runs under the deployment ceiling from WARDYN_DEFAULT_POLICY. Add a profile to give a group its own.",
  EDITOR_TITLE_NEW: "New profile",
  EDITOR_TITLE_EDIT: (name: string) => `Edit "${name}"`,
  FIELD_NAME: "Name",
  NAME_HINT:
    "What this profile is called on the assignments below, and in the run of anyone assigned to it. Names are unique.",
  CEILING_TITLE: "Ceiling",
  CEILING_LEAD:
    "Every run under this profile is bounded by this spec. A member's own policy is clamped to it, and so is a saved policy they pick.",
  LIMITS_TITLE: "Limits",
  LIMITS_LEAD:
    "Some of what a run can do routes around the ceiling entirely. Deny it here instead.",
  LIMIT_EXEC_LABEL: "Deny exec runs",
  LIMIT_EXEC_HINT: "task_mode=exec runs a command with no agent, so no tool rule is ever consulted.",
  LIMIT_INTERACTIVE_LABEL: "Deny interactive runs",
  LIMIT_INTERACTIVE_HINT:
    "An interactive run is supervised at the attach pane rather than by rules. A run with no task comes up interactive too, and is refused the same way.",
  GRADE_NOTE: "Grades this ceiling as written — advisory, the same meter the policy editor shows.",
  SAVE: "Save profile",
  LIMIT_DRIVE_LABEL: "Deny mounting a user drive",
  LIMIT_DRIVE_HINT:
    "A run under this profile cannot mount the person's drive, even when one is allocated to them.",
  // ADDITION to §7.2: the three integer limits LimitRow (a Switch) cannot
  // carry — max_concurrent_runs (R4/F032), MaxEphemeralDiskMiB and
  // MaxDriveSizeMiB (§5.6/§6.3-§6.4). One zero rule across all three: 0
  // means no limit under this profile.
  LIMIT_CONCURRENT_LABEL: "Concurrent runs",
  // F4-F8: LimitNumberRow's numberField renders 0 as blank — 0 IS "no limit"
  // on the wire, but "0 means..." named a value the field can't display.
  // "Leave blank" is what the operator can do.
  LIMIT_CONCURRENT_HINT: "How many runs a person under this profile may have going at once. Leave blank for no limit.",
  LIMIT_EPHEMERAL_LABEL: "Largest ephemeral scratch (MiB)",
  LIMIT_EPHEMERAL_HINT:
    "Binds a run's requested scratch size, not a run that requests none. Leave blank for no limit under this profile.",
  LIMIT_DRIVE_SIZE_LABEL: "Largest drive (MiB)",
  LIMIT_DRIVE_SIZE_HINT:
    "Clamps the drive size a person under this profile resolves to. On a share it bounds the number shown, not the share. Leave blank for no limit.",
  SAVE_ERROR: "Couldn't save this profile.",

  // ---- §7.3 assignments and the resolved preview ----
  ASSIGN_TITLE: "Assignments",
  ASSIGN_LEAD: "Who runs under which profile. One profile applies to a person — never two merged together.",
  PRECEDENCE:
    "The most specific assignment wins: a person beats a group, and a group beats everyone. Within a person, the sign-in subject beats the email. Between groups, the higher priority wins, then the profile name.",
  // EFFECT_NOTE and SIGNIN_NOTE are two halves of ONE fact and render
  // together; their subjects differ and getting them backwards is the trap.
  // An ASSIGNMENT is a row the resolver re-reads on every run, so a new one
  // binds at the member's next RUN. A person's GROUP MEMBERSHIP is read once,
  // at sign-in, so moving someone between groups binds only at their next
  // SIGN-IN — and for an API token, only at its next mint. Deliberately NOT
  // PERM.SNAPSHOT_BODY reused: that string ends on the grants rule, which is
  // false here (what lags is a CEILING, and it lags a sign-in, not a request).
  EFFECT_NOTE: "Takes effect on their next run. A run already dispatched keeps the ceiling it started with.",
  SIGNIN_NOTE:
    "A change to someone's groups in your identity provider reaches Wardyn only when they next sign in, so they keep their current ceiling until then — or, for an API token, until it is re-minted.",
  COL_PROFILE: "Profile",
  COL_PRIORITY: "Priority",
  // The same em dash PEOPLE.ADDED_CHART_NA renders, kept as its OWN key
  // because the two mean different things (no priority in this tier vs no
  // console-tracked add time) and one must be able to change without the
  // other (§7.1).
  PRIORITY_NA: "—",
  PRIORITY_HINT: "Breaks ties between groups a person is in. Higher wins. It is ignored for a person and for everyone.",
  ADD_TITLE: "Assign a profile",
  ADD_CTA: "Assign",
  FIELD_PROFILE: "Profile",
  PROFILE_PLACEHOLDER: "Pick a profile",
  FIELD_PRIORITY: "Priority",
  UNASSIGN_CONFIRM: (who: string, name: string) =>
    `Remove "${name}" from ${who}? Their next run is bounded by whatever else matches them, or by the deployment ceiling if nothing does.`,
  EMPTY_ASSIGN_TITLE: "No assignments yet",
  EMPTY_ASSIGN_BODY: "A profile with no assignment bounds nobody. Assign one to a group to start.",
  PREVIEW_TITLE: "Resolved profile",
  PREVIEW_LEAD:
    "Paste the roles, groups, or email a person's token would carry, and see which profile would bind their runs.",
  PREVIEW_RUN_CTA: "Resolve",
  // {matched} names the matching row in the table's own vocabulary ("a group
  // assignment", "a user assignment", "the everyone assignment"). The preview
  // resolves the claims AS TYPED — it is not a person lookup and can never say
  // a live person's groups (sessions are stateless cookies with no
  // server-side row), which is why there is no stale-snapshot arm here: that
  // surfaces where it actually bites, at the member's own launch, as
  // MEMBER.DENIED_STALE_GROUPS.
  PREVIEW_RESULT: (name: string, matched: string) => `These claims resolve to "${name}" — matched by ${matched}.`,
  // ADDITION to §7.3 (0.7 implementation): §7.3's prose names PREVIEW_RESULT's
  // three {matched} values verbatim — "a group assignment", "a user
  // assignment", "the everyone assignment" — but its TABLE freezes only the
  // template. They are the assignments table's own vocabulary and there is
  // exactly one correct set, so they are keyed here rather than retyped at the
  // one call site (governance/assignments.tsx's MATCHED_LABEL). Noted as an
  // addition in the campaign report, the same way permissions-copy.ts's
  // ENFORCE_OFF_TITLE was.
  MATCHED_USER: "a user assignment",
  MATCHED_GROUP: "a group assignment",
  MATCHED_ALL: "the everyone assignment",
  PREVIEW_RESULT_DEFAULT: "These claims resolve to the deployment ceiling — no assignment matches them.",
  PREVIEW_RESULT_UNKNOWN: "Couldn't resolve this — try again.",
  PREVIEW_NOT_SAVED: "Nothing here is saved.",

  // ---- §7.4 write refusals and the omission warning ----
  // GRANT_BOUND_TITLE and OMISSION_TITLE are HEADINGS OVER SERVER TEXT, not
  // replacements for it — see the §7.1 note at the top of this file.
  GRANT_BOUND_TITLE: "These grants go past the deployment ceiling",
  // Shown only when the profiles list reports ASSIGNED_COUNT = 0; otherwise
  // the dialog opens pre-filled with DELETE_RESTRICT_* and its confirm
  // disabled (§2.4).
  DELETE_CONFIRM: (name: string) => `Delete "${name}"? It isn't assigned to anyone, so nobody's ceiling changes.`,
  DELETE_RESTRICT_TITLE: "This profile is still assigned",
  DELETE_RESTRICT_BODY: (name: string, n: number) =>
    `"${name}" still has ${n} assignment${n === 1 ? "" : "s"}. Deleting it would widen those subjects back to the deployment ceiling without anyone deciding that — remove the assignments first.`,
  OMISSION_TITLE: "This profile narrows by omission",
  // Q6's acknowledge-before-save variant only. The recommended variant renders
  // OMISSION_TITLE over the warning list after a SUCCESSFUL save and never
  // blocks it.
  OMISSION_ACK: "I understand this profile takes these away.",

  // ---- §7.5 states ----
  // Retry is ACCESS_STATE.FETCH_FAILED_RETRY (§7.1, re-exported above). There
  // is no "governance not configured" state: with no profiles the feature is
  // not unconfigured, it is empty, and EMPTY_TITLE / EMPTY_BODY say so.
  FETCH_FAILED_TITLE: "Couldn't load governance profiles",
  FETCH_FAILED_BODY:
    "Something went wrong reaching the server. Profiles that are already assigned still bound every run — this list just can't show them right now.",
} as const;

// §7.6-§7.7 — MEMBER

// §7.6's two moments render ONLY when a profile is assigned. With no
// assignment there is no chip, no line and no placeholder — today's screens
// byte-for-byte, the same absent-row doctrine the resolver follows. GS_CHIP
// follows MEMBER_GETTING_STARTED.BARRIER_CHIP's `Label · value` shape exactly.
//
// §7.7's eight are SERVER-COMPOSED: writeError bodies and warnings[] entries
// the console renders verbatim off the wire. They are frozen here as the
// wording the Go side must emit — the belt-and-braces
// ACCESS_ERROR.EMAIL_KEY_REFUSED already uses — and they follow the in-tree
// writeError convention (a lowercase-opening clause naming the wire field or
// the thing refused), not a console-styled sentence. Two are the enforcement
// lane's, adopted byte-exact rather than re-worded:
//   - DENIED_STALE_GROUPS is groupsSnapshotStaleMsg (internal/api/
//     governance.go) verbatim, wire prefix included. It beats the
//     console-voiced draft on the one thing that matters: it names BOTH
//     remedies, so it cannot mislead an API-token holder whose groups were
//     stamped at mint. PERM.STALE_GROUPS stays the advisory line about a
//     MISSING PERMISSION and is not reworded.
//   - WARN_GRANT_DROPPED is reintersectGovernanceGrants' warning, which fires
//     when a redeploy removes a pairing from WARDYN_DEFAULT_POLICY that a
//     stored profile still names: the grant is dropped rather than the run
//     failed, and the member is told.
// DENIED_CODEX_HOLD sits BESIDE, never replaces, runs_create_validate.go's
// existing explicit-hold refusal (§7.1) — one refuses a hold the caller asked
// for, the other a hold their profile derived. WARN_STORED_CLAMPED is the
// HEADER warning only: composer.Clamp's own per-field warnings ride the same
// warnings[] list and say WHAT changed.
export const MEMBER = {
  // ---- §7.6 the two display moments ----
  CEILING_PROFILE: (name: string) =>
    `Bounded by "${name}", the governance profile your admin assigned you. Your policy is clamped to it.`,
  GS_CHIP: (name: string) => `Governance · ${name}`,
  GS_BODY: (name: string) => `Your runs are bounded by "${name}". What you can change is what it leaves open.`,

  // ---- §7.7 refusals and warnings ----
  DENIED_TASK_MODE_EXEC: (name: string) =>
    `task_mode=exec is not allowed by your governance profile "${name}" — an exec run carries no agent and no tool approvals, so nothing supervises it. Launch with an agent instead.`,
  DENIED_INTERACTIVE: (name: string) =>
    `interactive runs are not allowed by your governance profile "${name}", and a request with no task comes up interactive too. Launch with a task, and without --interactive.`,
  DENIED_SEED_AUTO_TOOLS: (name: string) =>
    `seed_auto_tools is not allowed by your governance profile "${name}": its tool rules hold or deny, and the pre-attach seed runs before any human is at the pane. Launch without it.`,
  DENIED_CODEX_HOLD: (name: string) =>
    `codex-cli is not supported under your governance profile "${name}": its tool rules hold or deny, and codex-cli has no external tool-approval contract. Launch a different agent.`,
  WARN_STORED_CLAMPED: (policy: string, name: string) =>
    `saved policy "${policy}" was clamped to your governance profile "${name}"`,
  WARN_WORKSPACE_DENIED: (host: string, name: string) =>
    `workspace host "${host}" is denied by your governance profile "${name}" — the run launches, but that host is refused at the proxy`,
  WARN_GRANT_DROPPED: (name: string, kind: string, reason: string) =>
    `governance profile "${name}": dropped ${kind} grant no longer within the deployment's eligible grants (${reason})`,
  DENIED_STALE_GROUPS:
    "groups_snapshot_stale: your group membership snapshot is missing or was truncated at sign-in, and this deployment assigns governance profiles by group — sign in again (or re-mint your API token) so your ceiling can be resolved",

  // The two CAPABILITY refusals, which is why — alone in this group — they name
  // no profile: they fire whether or not the caller has one. Both close a door a
  // member could otherwise walk through AFTER the explicit check had already run
  // (denyMemberSeededImage, runs_create_validate.go; handleCreateWorkspace,
  // workspaces.go). The `{id}` below is a literal route segment, not a parameter.
  DENIED_SEEDED_IMAGE: (image: string) =>
    `image ${image} comes from your own workspace's base image and is not granted to you — ask an admin to grant the exact image ref, or launch with the agent's convention image`,
  DENIED_WORKSPACE_LLM_CRED:
    "llm_cred is operator-only — an admin binds a workspace's model/harness credential (PUT /workspaces/{id}/llm-cred); create your workspace without it and ask for the binding",
} as const;

// §7.8 — POSITIONING

// Four words carrying the combination the product actually has: sandbox depth,
// org-level governance, the org's own infrastructure, and no paywall. The
// long-form variant keeps its sentence shape and drops the keys-only frame.
// "Governed" is a mechanism the console demonstrates, not a compliance claim.
// The released videos narrate the old line and lag this change (§2.7).
export const POSITIONING = {
  HERO_SLOGAN: "Sandboxed. Governed. Self-hosted. Free.",
  SETUP_SUBTITLE: "Governed sandboxes for anything you run — on your own infrastructure, free.",
} as const;

// §7.9 — DIRECTORY

// SUGGEST_ROW's {displayName} is plain text and {detail} is mono (an email, an
// object id, an app-role value) — the row is the one place both appear
// together, and the mono half is what gets stored. GROUP_PICKED_CHIP shows the
// NAME while the field's value is the object id; GROUP_VALUE_NOTE is what
// stops that from being a lie, and is why the group case gets a chip at all
// while a user's email needs none.
//
// Absent mode has no string: with no directory configured the endpoint answers
// with its unconfigured code and the control IS the plain input — no note, no
// banner, no disabled state, nothing for a reader to act on. SEARCHING and
// MIN_CHARS_HINT are frozen here but their DISPLAY behaviour is Q7.
export const DIRECTORY = {
  // RE-EXPORTED, never retyped: §7.9 freezes this as the picker option, the
  // table chip AND the mapped-role label — one string for all three — and puts
  // its home next to PEOPLE.ROLE_ADMIN / PEOPLE.ROLE_MEMBER, which is why it
  // is title case AS THE CHIP; a sentence takes its lowercase
  // (people-access-prompt.md §7.2, access-panel.tsx's roleLabelInSentence).
  // Two homes for one frozen label is how they drift
  // (people-access-copy.ts:41-50 says so).
  ROLE_SECURITY_ADMIN: PEOPLE.ROLE_SECURITY_ADMIN,
  SUGGEST_ROW: (displayName: string, detail: string) => `${displayName} — ${detail}`,
  GROUP_PICKED_CHIP: (displayName: string) => `Group · ${displayName}`,
  GROUP_VALUE_NOTE: "Stored as the group's object id — the name is what your directory calls it today.",
  SEARCHING: "Searching your directory…",
  MIN_CHARS_HINT: "Type at least 2 characters to search your directory.",
  NO_MATCHES: "No matches in your directory. Type the value yourself if you know it.",
  LOOKUP_FAILED: "Couldn't check your directory — type the value yourself.",
} as const;

// #96/#93 — the autonomy rubric (0.8, #77). The mock round's frozen strings
// table (autonomy-launch.html surface 1), transcribed verbatim — not parsed
// back out of a doc like §7.2-§7.9 above, because the mock lives in a
// scratchpad rather than in this repo's docs/. Two rulings from #96's review
// (recorded on #96, applied here rather than in the mock, which froze before
// they were made):
//
//   1. bound_by is a LIST. A tie at the resolved level names EVERY cause, not
//      the first — autonomyBoundSentence below composes it.
//   2. The profiles-list chip (LIMITS_CHIP.AUTONOMY) names the STRICTEST cap,
//      not merely that a rubric exists — the detail is unreadable in a
//      tooltip on a phone or by keyboard, so it goes on screen.

// ---- the profile editor's Rubric section (profile-rubric.tsx) ----
export const RUBRIC = {
  HEADING: "Autonomy rubric",
  INTRO:
    "Cap how much a run may do on its own, by the posture it launches with. A run gets the lowest level any matching row sets. Rows left at No cap restrict nothing.",
  EMPTY_NOTE: "No row sets a cap, so this profile leaves autonomy exactly as it is today.",
  NOCAP: "No cap",
  GROUP_EGRESS: "Network reach",
  GROUP_SECRETS: "Secrets",
  GROUP_BARRIER: "Barrier",
  GROUP_BARRIER_HINT: "The enforced barrier — “confinement class” in the API and the docs.",
  // key -> [row label, why]. Iterate AUTONOMY_RUBRIC_ROW_KEYS for a stable,
  // Go-matching order rather than Object.keys.
  ROWS: {
    egress_open: ["Open", "The run can reach hosts beyond the baseline, or everything."],
    egress_reviewed: ["Reviewed", "New hosts are approved on first use."],
    egress_sealed: ["Sealed", "Baseline hosts only."],
    secrets_powerful: ["Powerful", "The run carries a credential that can write."],
    secrets_baseline: ["Baseline", "The run carries credentials, none of them write-capable."],
    secrets_none: ["None", "The run carries no credential."],
    confinement_cc1: ["Fence (CC1)", "Shared kernel."],
    confinement_cc2: ["Wall (CC2)", "gVisor userspace kernel."],
    confinement_cc3: ["Vault (CC3)", "Kata microVM."],
  } as Record<AutonomyRubricRowKey, [label: string, why: string]>,
  SET_NOTE: (n: number, level: string) => `${n} of 9 rows set a cap. The lowest is ${level}.`,
} as const;

// foldAutonomyRubric summarizes a WHOLE rubric for display — every row that
// sets a cap, and the lowest level among them — the same fold the profile
// editor's footer note and the profiles-list chip both need. Distinct from
// the SERVER's per-run FoldAutonomy (internal/composer/autonomy.go): that one
// folds the three rows ONE run's posture selects; this one is an
// admin-facing "what is the strictest this profile could ever cap at", over
// every row that is set, which is exactly what the mock's editor footer and
// list chip compute.
export function foldAutonomyRubric(rubric: AutonomyRubric | null | undefined): {
  setKeys: AutonomyRubricRowKey[];
  lowest: AutonomyLevel | null;
} {
  const setKeys = AUTONOMY_RUBRIC_ROW_KEYS.filter((k) => !!rubric?.[k]);
  let lowest: AutonomyLevel | null = null;
  for (const k of setKeys) {
    const level = rubric![k]!;
    if (lowest === null || AUTONOMY_LEVEL_ORDER.indexOf(level) < AUTONOMY_LEVEL_ORDER.indexOf(lowest)) {
      lowest = level;
    }
  }
  return { setKeys, lowest };
}

// ---- the profiles list chip (governance-screen.tsx) ----
export const LIMITS_CHIP = {
  // Ruling 2 (#96 review): the STRICTEST cap, on the chip face — not merely
  // that a rubric exists, and not behind a tooltip. `label` is the strictest
  // level's AUTONOMY_META label (e.g. "Attended"), looked up by the caller.
  AUTONOMY: (label: string) => `Autonomy: ${label} at the strictest`,
} as const;

// ---- the New Run rail's Autonomy section + the run header (new-run-rail.tsx
// / run-detail-summary-header.tsx) ----
export const AUTONOMY_RAIL = {
  HEADING: "Autonomy",
  NO_CAP: "Your organization's profile sets no autonomy rubric, so nothing caps this run.",
  NO_PROFILE: "No governance profile applies to you, so nothing caps this run.",
  DERIVED_HOLD_NOTE: "Every tool call in this run will wait for your confirmation.",
  PROFILE_LINE: (p: string) => `From the ${p} governance profile.`,
} as const;

// Per-cause fragments — the single source both AUTONOMY_BOUND's one-cause
// sentences and autonomyBoundSentence's tied-cause composition read from, so
// the two can never say something different about the same row.
const AUTONOMY_BOUND_DETAIL: Record<AutonomyRubricRowKey, { dimension: string; detail: string }> = {
  egress_open: { dimension: "network reach", detail: "it can reach hosts beyond the baseline" },
  egress_reviewed: { dimension: "network reach", detail: "new hosts are approved on first use" },
  egress_sealed: { dimension: "network reach", detail: "baseline hosts only" },
  secrets_powerful: { dimension: "secrets", detail: "it carries a credential that can write" },
  secrets_baseline: { dimension: "secrets", detail: "it carries credentials, none of them write-capable" },
  secrets_none: { dimension: "secrets", detail: "it carries none" },
  confinement_cc1: { dimension: "barrier", detail: "Fence, confinement class CC1" },
  confinement_cc2: { dimension: "barrier", detail: "Wall, confinement class CC2" },
  confinement_cc3: { dimension: "barrier", detail: "Vault, confinement class CC3" },
};

// One sentence per cause — frozen, byte for byte, from the mock round's
// strings table. What the rail renders when bound_by carries exactly one
// cause (the common case: only one axis actually bound the level).
export const AUTONOMY_BOUND: Record<AutonomyRubricRowKey, string> = Object.fromEntries(
  AUTONOMY_RUBRIC_ROW_KEYS.map((k) => [
    k,
    `Bound by this run's ${AUTONOMY_BOUND_DETAIL[k].dimension}: ${AUTONOMY_BOUND_DETAIL[k].detail}.`,
  ]),
) as Record<AutonomyRubricRowKey, string>;

function joinAnd(items: string[]): string {
  if (items.length <= 1) return items[0] ?? "";
  return `${items.slice(0, -1).join(", ")} and ${items[items.length - 1]}`;
}

// A cause's own detail can carry a comma of its own (confinement's "Fence,
// confinement class CC1"), so joining multiple details with plain ", " reads
// ambiguously about where one detail ends and the next begins — a three-way
// tie could read as four comma-separated fragments. Semicolons disambiguate
// unconditionally, so a tied detail list uses them instead of joinAnd's
// comma-plus-"and" shape.
function joinDetails(items: string[]): string {
  if (items.length <= 1) return items[0] ?? "";
  return items.join("; ");
}

// autonomyBoundSentence composes AutonomyResolution.bound_by into the rail's
// one sentence — ruling 1 (#96 review): every tied cause is named, never just
// bound_by[0]. A single cause renders AUTONOMY_BOUND's frozen sentence
// unchanged; a tie composes across the fragments so the dimensions and their
// details each read as their own list rather than retyping a sentence per
// combination (nine rows tie in at most 2^3 - 1 = 7 shapes — a table would
// have to enumerate all of them and would still drift from AUTONOMY_BOUND the
// day a fragment's wording changes).
export function autonomyBoundSentence(causes: AutonomyRubricRowKey[]): string {
  if (causes.length === 0) return AUTONOMY_RAIL.NO_CAP;
  if (causes.length === 1) return AUTONOMY_BOUND[causes[0]];
  const dims = causes.map((c) => AUTONOMY_BOUND_DETAIL[c].dimension);
  const details = causes.map((c) => AUTONOMY_BOUND_DETAIL[c].detail);
  return `Bound by this run's ${joinAnd(dims)}: ${joinDetails(details)}.`;
}
