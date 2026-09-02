/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// User drives copy canon (0.7, the admin's drive registry + the member's
// per-run mount) — the frozen canonical-strings tables from
// docs/design/user-drives-prompt.md §7.2-§7.8, transcribed verbatim. The
// /drives screen (the drives table + editor, allocations + preview), the
// setup-step/Settings card, the member's New Run checkbox and Getting
// Started chip, and the run rail's Drive line all read these instead of
// retyping the copy, so the shipped wording can't drift from the reviewed
// mock.
//
// Pure TS — no React, no fetch, no DOM. Same discipline as governance-copy.ts
// and permissions-copy.ts: the components that consume this add NO copy of
// their own.
//
// user-drives-copy.test.ts PARSES §7.2-§7.8's tables back out of the prompt
// doc and compares all 140 keys below against them, so a swapped hyphen, a
// dropped ellipsis or a new doc row fails a gate instead of shipping.
//
// Backtick-mono rule (§7 header note): a backticked substring inside a frozen
// string (the mount target, an env var, a wire field or value, a directory or
// object name) is PLAIN TEXT here — the mono span is a DISPLAY concern the
// consuming component applies, uniformly at EVERY recurrence of that
// substring, never baked into the string (the governance-copy.ts precedent).
// A drive NAME is never mono: it is a human-chosen label, and the strings
// below already spell the double quotes it is rendered inside.

// ==================== §7.1 — reused canon, referenced never re-frozen ========
//
// These already exist and are IMPORTED, not retyped. Re-exported from here so
// a drives surface has one import site and cannot accidentally grow a second
// home for a string that already has one:
//
//   PERM.COL_WHO / FIELD_WHO / COL_ADDED / SUBJECT_USER / SUBJECT_GROUP /
//     SUBJECT_ALL / HINT_USER / HINT_GROUP / HINT_ALL / REMOVE — the
//     allocations table's "Who" column, its three subject kinds and their
//     hints, and the per-row remove action — the same shape governance's
//     assignments table already uses.
//   PEOPLE.CANCEL — the "Allocate a drive" form's Cancel button.
//   PREVIEW.FIELD_CLAIMS / FIELD_CLAIMS_HINT — the "Who gets what" preview
//     takes the claims a token would carry, exactly what the People step's
//     and the governance preview take (§7.3): it has no field label or hint
//     of its own.
//   ACCESS_STATE.FETCH_FAILED_RETRY — the Retry beside DRIVES's own
//     FETCH_FAILED_TITLE / _BODY (§7.4).
export { PERM } from "./permissions-copy";
export { ACCESS_STATE, PEOPLE, PREVIEW } from "./people-access-copy";

// The rest of §7.1 is reused AT ITS OWN HOME, not through here — each already
// lives beside the component that draws it, and a re-export would only add an
// import hop:
//   GOV.FIELD_PRIORITY / COL_PRIORITY / PRIORITY_HINT / PRIORITY_NA /
//     PREVIEW_NOT_SAVED / PREVIEW_RESULT_UNKNOWN (governance-copy.ts) — the
//     allocations table's own Priority column/field and the preview's footer
//     and failure arm, reused verbatim: "the priority copy and the preview's
//     'not saved' are governance-copy.ts's... referenced, never retyped"
//     (§5 #6).
//   GOV.LIMITS_TITLE / LIMITS_LEAD / LIMIT_EXEC_LABEL / LIMIT_INTERACTIVE_LABEL
//     / LIMIT_DRIVE_LABEL / LIMIT_DRIVE_HINT (governance-copy.ts) — the
//     profile editor's own Limits section. The door's two strings live in
//     governance-prompt.md §7.2 and governance-copy.ts, appended there; "The
//     profile editor renders them from GOV.*; the drives module never
//     carries them" (§5 #5) — so they are NOT re-exported here even though
//     they render in the same mock.
//   DIRECTORY.* (governance-copy.ts) — the combobox's suggestion row, group
//     chip, note and three lookup states; the allocation's subject field when
//     a directory is configured (governance-prompt §7.9). A shared combobox
//     component imports it from governance-copy.ts directly.
//   MEMBER.DENIED_STALE_GROUPS (governance-copy.ts) — the truncated-snapshot
//     refusal, reused verbatim when group-tier allocations exist (§7.3,
//     §7.7) — imported from governance-copy.ts directly at whatever call site
//     renders it, alongside DRIVE_MEMBER (never re-exported as MEMBER here:
//     §5 #11, see DRIVE_MEMBER's own note below).
//   MEMBER_GETTING_STARTED.SETUP_SUMMARY_TITLE / _HELPER / BARRIER_CHIP /
//     MODEL_ACCESS_PROVIDED_CHIP / SIGNIN_SSO_CHIP / WORKSPACE_TITLE /
//     WORKSPACE_BODY / WORKSPACE_ACTION (wardyn/copy.ts) — the rest of the
//     member's Getting Started screen; DRIVE_MEMBER.GS_DRIVE_* below sits
//     beside these, not through them.
//   The Workspaces page header + action, the New Run workspace section +
//     Select, the setup step heading (STEP_HEADING.workspaces), the Settings
//     page title + Host card, IdentityWidget's labels, relativeTime(t) and
//     the launch-warning toast title — each component-local text, not a
//     copy.ts export.
//
// DELIBERATELY ABSENT — the strings the SERVER emits. §7.1's second table
// freezes seven Go format strings that this module does NOT carry, because
// the console renders them FROM THE WIRE, verbatim, and a second copy here
// would be a claim rather than canon. All seven are ADMIN-facing (registering
// or deleting a drive, granting an allocation) — the member's own doors are
// §7.7, which §7 froze as canon keys instead (see DRIVE_MEMBER's note):
//   - host root outside the roots (400, validateUserDrive) — host_root
//     "{path}" is not inside WARDYN_USER_DRIVE_HOST_ROOTS ({roots});
//   - host root under a denied prefix (400, validateUserDrive) — the same
//     deny list every host bind obeys;
//   - backend / runner mismatch (400, validateUserDrive);
//   - template invalid for a share (400, validateUserDrive) — home_template
//     "hash" on a share backend, when only sub or email_local can name one;
//   - size required for a managed claim (400, validateUserDrive) — size_mib
//     must be above 0 for a k8s_pvc drive;
//   - delete while allocated (409, handleDeleteUserDrive) — DRIVES.
//     DELETE_RESTRICT_BODY below is the CLIENT-SIDE PRE-FILL only; it names a
//     count, which is knowable only client-side. On the race path the client
//     believed the count was zero, so it renders the shipped 409, which
//     carries no count and must not grow one (§7.4);
//   - home override on a non-user row (400, validateUserDriveGrant) —
//     home_override is accepted on a user-tier allocation only.
//
// DRIVE_MEMBER below is the one place that shape is inverted: §7.7's eight
// strings are server-composed too, but §7 froze them as canon keys, so they
// are transcribed here as the wording the Go side must emit — the same
// belt-and-braces governance-copy.ts's MEMBER already uses for its own §7.7.
// See DRIVE_MEMBER's own note.

// ==================== §7.2-§7.5 — DRIVES, the admin screen ==================

export const DRIVES = {
  // ---- §7.2 the drives block ----
  // TITLE is ONE string for four places — the screen heading, the Workspaces
  // header's outline button, the setup card's title and the Settings card's
  // title — the way GOVERNANCE.TITLE serves nav and heading (§7.2 prose).
  TITLE: "User drives",
  LEAD: "Persistent storage a run can mount at /home/agent/drive. An admin registers a drive and allocates it to people or groups; each person gets their own directory in it, and chooses per run whether to mount it.",
  DRIVES_TITLE: "Drives",
  DRIVES_LEAD:
    "A drive is one place storage comes from: a share your platform already mounts, or a volume Wardyn creates per person.",
  COL_NAME: "Name",
  COL_BACKEND: "Backend",
  COL_SIZE: "Size",
  COL_MODE: "Mode",
  COL_RECLAIM: "When a person leaves",
  COL_ALLOCATED: "Allocated to",
  NEW_CTA: "New drive",
  EDIT: "Edit",
  DELETE: "Delete",
  // The backend column renders KIND_MANAGED / KIND_SHARE as a neutral chip
  // over the backend's wire value in mono; the four long BACKEND_* labels
  // below are the editor's select options only.
  KIND_MANAGED: "Managed",
  KIND_SHARE: "Share",
  ALLOCATED_NONE: "Allocated to nobody",
  // Inline pluralisation, the shape PERM.ENFORCE_ON_BODY already uses — not a
  // second pluralisation helper (§5 #9).
  ALLOCATED_COUNT: (n: number) => `${n} subject${n === 1 ? "" : "s"}`,
  MODE_RO: "Read-only",
  MODE_RW: "Writable",
  MODE_RO_INLINE: "read-only",
  MODE_RW_INLINE: "writable",
  // A size cell renders SIZE_MIB / SIZE_GIB (or SIZE_NONE) with the drive's
  // ENFORCEMENT_* gloss as its sub-line, so the honesty is on every number,
  // not only in the HONESTY note below. Both take a number, not a string
  // placeholder (§5 #10) — checked in their own `it` block, not through the
  // rendered-map path.
  SIZE_NONE: "No allocation shown",
  SIZE_MIB: (n: number) => `${n} MiB`,
  SIZE_GIB: (n: number) => `${n} GiB`,
  EMPTY_TITLE: "No drives yet",
  EMPTY_BODY: "Runs keep nothing between them today. Register a drive to give people a directory that persists.",
  EDITOR_TITLE_NEW: "New drive",
  EDITOR_TITLE_EDIT: (name: string) => `Edit "${name}"`,
  FIELD_NAME: "Name",
  NAME_HINT: "What this drive is called on the allocations below and in a member's run. Names are unique.",
  FIELD_BACKEND: "Backend",
  BACKEND_HINT: "Only the backends this deployment's runner can mount are offered.",
  BACKEND_DOCKER_VOLUME: "Wardyn-managed volume (this Docker host)",
  BACKEND_HOST_PATH: "Share mounted on this host",
  BACKEND_K8S_PVC: "Wardyn-managed volume (a storage class)",
  BACKEND_K8S_PVC_STATIC: "Share provisioned by your platform (a pre-created volume claim per person)",
  BACKEND_UNAVAILABLE_DOCKER_ROOTS:
    "Not available: this deployment sets no WARDYN_USER_DRIVE_HOST_ROOTS, so no host path may back a drive.",
  FIELD_HOST_ROOT: "Host root",
  HOST_ROOT_HINT:
    "The mounted share's root on this host, under one of WARDYN_USER_DRIVE_HOST_ROOTS. Each person's directory is a subdirectory of it; only that subdirectory is ever mounted into a run.",
  FIELD_STORAGE_CLASS: "Storage class",
  STORAGE_CLASS_HINT:
    "Leave empty for the cluster default. A block storage class enforces the size; a network-share provisioner does not.",
  // HOME_RULE renders under the directory-name field for every option.
  FIELD_HOME: "Directory name",
  HOME_HINT:
    "How each person's directory is named inside the drive. A share uses the name your directory already has; a managed drive can use a derived id.",
  HOME_HASH: "Derived (stable id)",
  HOME_HASH_HINT:
    "A short id derived from the drive and the person. It never collides and says nothing about who it is — Preview below finds a person's directory.",
  HOME_SUB: "Sign-in subject",
  HOME_SUB_HINT: "The subject id your identity provider sends, lowercased. Stable, but rarely what a share already calls a person.",
  HOME_EMAIL_LOCAL: "Email, before the @",
  HOME_EMAIL_LOCAL_HINT: "The part before the @, lowercased — the usual shape of a corporate home directory.",
  HOME_RULE:
    "A directory name is lowercase letters and digits, then any of . _ -, up to 63 characters. A person whose claim cannot name one is refused at their run, never guessed.",
  FIELD_SIZE: "Size (MiB)",
  // SIZE_HINT_REQUIRED replaces SIZE_HINT under the Size field when the
  // backend is k8s_pvc (Q7); no required-marker glyph exists — the hint
  // carries the word.
  SIZE_HINT: "The allocation shown to each person, and on Kubernetes the volume request. 0 shows no allocation. Override it per person below.",
  SIZE_HINT_REQUIRED:
    "The allocation shown to each person, and the volume request Kubernetes makes. Required — a claim cannot request zero. Override it per person below.",
  FIELD_WRITABLE: "Writable",
  WRITABLE_HINT: "Off by default. A writable drive is where a run's changes persist — and what a compromised run could alter.",
  FIELD_RECLAIM: "When a person leaves",
  RECLAIM_RETAIN: "Keep their directory",
  RECLAIM_DELETE: "Delete their directory",
  RECLAIM_HINT: "Recorded here, carried out by you: removing an allocation below never deletes data. Preview prints the exact object to remove.",
  SAVE_CTA: "Save drive",
  SAVE_ERROR: "Couldn't save this drive.",
  // SAVE_REFUSED_TITLE heads every 400 the editor can meet, over the server's
  // text (the §7.1 "deliberately absent" table above); SAVE_ERROR is the
  // unreachable-server arm, not a refusal.
  SAVE_REFUSED_TITLE: "This drive can't be saved as written",
  // ENFORCEMENT_FILESYSTEM is frozen for a value no v1 backend yields (§2.7).
  ENFORCEMENT_FILESYSTEM: "Size enforced by the filesystem",
  ENFORCEMENT_REQUEST: "Size requested; the storage class decides",
  ENFORCEMENT_EXTERNAL: "Size bounded by the share's own quota",
  ENFORCEMENT_NONE: "Size shown, not enforced",
  // Renders once, as the plain note under the drives table (Q1).
  HONESTY:
    "Wardyn never enforces a drive's size itself. On Kubernetes the size is the volume request and the storage class decides whether it binds — block disks do, network-share provisioners do not. On Docker a managed drive has no byte cap, the same gap disk_mib has. A share is bounded by its own quota. The size you see is the allocation, not a guarantee.",

  // ---- §7.3 allocations and the preview ----
  ALLOC_TITLE: "Allocations",
  ALLOC_LEAD: "Who gets a drive. A person's own row beats their group's; a group's beats everyone's. One drive per person.",
  PRECEDENCE:
    "The most specific allocation wins: a person beats a group, and a group beats everyone. Within a person, the sign-in subject beats the email. Between groups, the higher priority wins, then the drive name.",
  // EFFECT_NOTE and SIGNIN_NOTE are two halves of ONE fact and render
  // together, the governance round's lesson carried over with the object
  // changed: an ALLOCATION is a row the resolver re-reads every run, so it
  // binds at the next RUN; a person's GROUP MEMBERSHIP is read once at
  // sign-in, so a group's allocation reaches them at their next SIGN-IN — or,
  // on an API token, its next mint. GOV.SIGNIN_NOTE is not reused: it ends on
  // "keep their current ceiling", and what lags here is a mount (§7.3 prose).
  EFFECT_NOTE: "Takes effect on their next run. A run already dispatched keeps what it mounted.",
  SIGNIN_NOTE:
    "A change to someone's groups in your identity provider reaches Wardyn only when they next sign in, so a group's allocation reaches them then — or, for an API token, when it is re-minted.",
  ADD_TITLE: "Allocate a drive",
  ADD_CTA: "Allocate",
  FIELD_DRIVE: "Drive",
  DRIVE_PLACEHOLDER: "Choose a drive",
  FIELD_SIZE_OVERRIDE: "Size override (MiB)",
  SIZE_OVERRIDE_HINT: "Empty or 0 keeps the drive's size.",
  FIELD_WRITABLE_OVERRIDE: "Writable",
  WRITABLE_OVERRIDE_INHERIT: "Same as the drive",
  WRITABLE_OVERRIDE_HINT:
    "Same as the drive, or set it for this subject alone. A run can narrow the result to read-only, never widen it.",
  FIELD_HOME_OVERRIDE: "Directory name",
  // HOME_OVERRIDE_NA replaces HOME_OVERRIDE_HINT under a disabled
  // directory-name field when the subject type is not a user.
  HOME_OVERRIDE_HINT: "For one person only: the exact directory name inside the drive, when it differs from the drive's rule.",
  HOME_OVERRIDE_NA: "Only a person's own allocation can name a directory.",
  FIELD_ENABLED: "Enabled",
  ENABLED_HINT: "Off pauses the drive for this subject without deleting anything.",
  COL_DRIVE: "Drive",
  // The Overrides column renders zero or more chips: OVERRIDE_SIZE, MODE_RO /
  // MODE_RW (a writable override, in the mode's own chip vocabulary),
  // OVERRIDE_HOME, and PAUSED_CHIP for enabled=false; OVERRIDES_NONE when
  // there are none and the row is enabled.
  COL_OVERRIDES: "Overrides",
  OVERRIDES_NONE: "None",
  OVERRIDE_SIZE: (size: string) => `Size · ${size}`,
  OVERRIDE_HOME: (name: string) => `Directory · ${name}`,
  PAUSED_CHIP: "Paused",
  // An inline `plain` note after an upsert that repointed an existing row —
  // not a toast: it is worth reading twice.
  ALLOC_REPLACED: "This subject already had an allocation — it was replaced.",
  REMOVE_CONFIRM: (who: string, name: string) =>
    `Remove "${name}" from ${who}? Their next run mounts whatever else matches them, or nothing. Nothing on the drive is deleted — the directory stays until you reclaim it.`,
  EMPTY_ALLOC_TITLE: "No allocations yet",
  EMPTY_ALLOC_BODY: "A drive with no allocation is mounted by nobody. Allocate one to a group to start.",
  PREVIEW_TITLE: "Who gets what",
  // The preview has no field label or hint of its own (PREVIEW.FIELD_CLAIMS /
  // _HINT, §7.1). Its footer is GOV.PREVIEW_NOT_SAVED; its failure arm is
  // GOV.PREVIEW_RESULT_UNKNOWN. It resolves the claims AS TYPED against the
  // allocations and is not a person lookup; a truncated snapshot surfaces at
  // the member's launch as MEMBER.DENIED_STALE_GROUPS, not here.
  PREVIEW_LEAD: "Paste the roles, groups, or email a person's token would carry, and see which drive and directory they would mount.",
  PREVIEW_CTA: "Preview",
  PREVIEW_NONE: "No drive is allocated to these claims.",
  // {tier} is one of the three PREVIEW_TIER_* words, frozen in the table
  // rather than in prose (the governance round's MATCHED_* lesson).
  PREVIEW_RESULT: (drive: string, tier: string) => `"${drive}" via the ${tier} allocation`,
  PREVIEW_TIER_USER: "user",
  PREVIEW_TIER_GROUP: "group",
  PREVIEW_TIER_ALL: "everyone",
  PREVIEW_OBJECT_LABEL: "Storage object",
  PREVIEW_OBJECT_HINT: "What the reclaim command names — copy it when someone leaves.",
  PREVIEW_ENFORCEMENT_LABEL: "Enforcement",

  // ---- §7.4 write refusals and states ----
  // Shown only when the drives table reports ALLOCATED_COUNT = 0; otherwise
  // the dialog opens pre-filled with DELETE_RESTRICT_TITLE / _BODY and its
  // confirm disabled (§2.4); on the race path the console's heading sits over
  // the server's 409 (§7.1's "deliberately absent" table), confirm still
  // enabled.
  DELETE_CONFIRM: (name: string) =>
    `Delete "${name}"? It is allocated to nobody, so no run loses a mount. Anything already stored in it stays where it is.`,
  DELETE_RESTRICT_TITLE: "This drive is still allocated",
  DELETE_RESTRICT_BODY: (name: string, n: number) =>
    `"${name}" is still allocated to ${n} subject${n === 1 ? "" : "s"}. Remove those allocations first. Deleting the row never deletes data — their directories stay until you reclaim them.`,
  // There is no "drives not configured" state: with no drives the feature is
  // empty, not unconfigured, and EMPTY_TITLE / EMPTY_BODY above say so. Retry
  // on fetch-failed is ACCESS_STATE.FETCH_FAILED_RETRY (§7.1, re-exported).
  FETCH_FAILED_TITLE: "Couldn't load user drives",
  FETCH_FAILED_BODY:
    "Something went wrong reaching the server. Allocations that already exist still bind every run — this list just can't show them right now.",

  // ---- §7.5 the card (setup step and Settings) and the entry points ----
  // One component in two homes (setup/user-drives-card.tsx; the Settings
  // shared-component rule). The card's title is TITLE above; its body is
  // CARD_LEAD over CARD_SUMMARY(CARD_DRIVES(n), CARD_ALLOCATIONS(m)) or
  // CARD_EMPTY; its action is the two-line link button with CARD_OPEN as its
  // label and the summary as its meta line. It counts ALLOCATIONS, never
  // people: a group allocation is one row and Wardyn holds no directory read.
  // The Workspaces header's outline button is TITLE. The card and the button
  // render for SUPER only.
  CARD_LEAD: "Persistent storage people can mount into a run — separate from any workspace.",
  CARD_EMPTY: "No drives yet.",
  CARD_DRIVES: (n: number) => `${n} drive${n === 1 ? "" : "s"}`,
  CARD_ALLOCATIONS: (n: number) => `${n} allocation${n === 1 ? "" : "s"}`,
  CARD_SUMMARY: (drives: string, allocations: string) => `${drives} · ${allocations}`,
  CARD_OPEN: "Manage drives",
} as const;

// ==================== §7.6-§7.7 — DRIVE_MEMBER ==============================

// §7.6's moments render ONLY when a drive is allocated (or, for the reason
// lines, when it exists but can't be mounted). GS_DRIVE_CHIP follows
// BARRIER_CHIP's `Label · value` shape (Q2) and sits beside MEMBER.GS_CHIP,
// the governance chip, in the same row on the Getting Started screen.
//
// §5 #11: this namespace is DRIVE_MEMBER, not MEMBER — governance-copy.ts
// already exports MEMBER, and MEMBER.GS_CHIP / MEMBER.GS_BODY are the
// governance round's, rendering on the SAME Getting Started screen as
// DRIVE_MEMBER.GS_DRIVE_* below. A same-named twin would be a silent shadow
// at that one import site (member-getting-started.tsx), so the module
// exports no name governance-copy.ts already exports.
//
// §7.7's eight are SERVER-COMPOSED: writeError bodies the console renders
// verbatim off the wire, following the in-tree convention — a
// lowercase-opening clause naming the wire field or the thing refused — not a
// console-styled sentence (§7 header note; Hard canon #4). They are frozen
// here as the wording the Go side must emit, the same shape
// governance-copy.ts's own MEMBER uses for ITS §7.7 (DENIED_TASK_MODE_EXEC
// etc.): the shape is server-composed, but §7 froze it as canon keys, so it
// is transcribed rather than left absent. DENIED_STALE_GROUPS is NOT among
// these eight — MEMBER.DENIED_STALE_GROUPS (governance-copy.ts) is reused
// verbatim for the truncated-snapshot case and is not re-frozen here (§7.1,
// §7.7).
export const DRIVE_MEMBER = {
  // ---- §7.6 display moments ----
  NR_CHECKBOX: "Mount my drive",
  // {mode} in NR_HINT / GS_DRIVE_CHIP is MODE_RO_INLINE / MODE_RW_INLINE; the
  // mode sentence after the hint is NR_RW_NOTE when the mount will be
  // writable and NR_RO_NOTE otherwise — including when a writable allocation
  // is narrowed by NR_READONLY_TOGGLE for this run, so the sentence never
  // promises persistence a read-only mount cannot give. size_mib = 0 selects
  // the _NOSIZE twin rather than rendering DRIVES.SIZE_NONE inside a member's
  // sentence.
  NR_HINT: (name: string, size: string, mode: string) => `"${name}" at /home/agent/drive — ${size}, ${mode}.`,
  NR_HINT_NOSIZE: (name: string, mode: string) => `"${name}" at /home/agent/drive — ${mode}.`,
  NR_RW_NOTE: "What a run writes there persists to your next run.",
  NR_RO_NOTE: "A run can read it and never change it.",
  // Renders only when the allocation is writable and defaults OFF (Q5).
  NR_READONLY_TOGGLE: "Mount read-only for this run",
  // The two reason lines render IN PLACE OF the checkbox: an unmountable drive
  // is not a disabled checkbox with a tooltip, it is one sentence where the
  // checkbox would be.
  //
  // There is NO "no drive is allocated to you" line, and its absence is the
  // rule rather than an omission: §2.5 ends "with /me.user_drive null AND no
  // door, there is no checkbox and no line — today's card byte-for-byte".
  // A caller with no allocation and no door is exactly that caller, so the
  // sentence had no state left to render in; workspace-card.tsx returns null
  // there. The launch-path answer to the same condition is a SERVER string
  // (REFUSED_NO_GRANT below), which is a different thing: it is the reply to an
  // attempt, not a caption on an offer nobody was made.
  NR_PAUSED: "Your drive is paused by your admin.",
  NR_DENIED: (profile: string) => `Your governance profile "${profile}" does not allow mounting a drive.`,
  GS_DRIVE_CHIP: (name: string, size: string, mode: string) => `Drive · ${name}, ${size}, ${mode}`,
  GS_DRIVE_CHIP_NOSIZE: (name: string, mode: string) => `Drive · ${name}, ${mode}`,
  GS_DRIVE_CHIP_PAUSED: (name: string) => `Drive · ${name} · Paused`,
  // Renders in the "Add your workspace" card, after WORKSPACE_BODY, only when
  // /me.user_drive is non-null.
  GS_DRIVE_BODY:
    "Your drive is not a workspace: mount it from New run alongside whatever you attach. It is yours alone — a run sees only your directory.",

  // ---- §7.7 refusals (server-composed) ----
  // DENIED_DRIVE is the one 403 (audited authz.denied, reason
  // governance_profile, target runs.drive — denyMemberDrive beside
  // denyMemberRunQuota); the six REFUSED_NO_GRANT..REFUSED_BACKEND keys are
  // 422s with no audit (seedRequestDrive, run create and preflight both).
  // REFUSED_TARGET_RESERVED is the 400 validatePolicySpec's unique-target arm
  // raises when a policy or workspace source names the reserved target — met
  // by whoever writes the policy, member or admin, and it belongs here
  // because it is a door this feature adds. This table is COMPLETE (§5 #4):
  // every string a member can be refused with at a door this feature adds is
  // here.
  //
  // NOTHING here names an unprovisioned k8s_pvc_static claim. No door this
  // feature adds can see that condition — the row is valid, the allocation
  // resolves, and the claim's absence is discovered by the k8s driver at
  // DISPATCH — so a frozen sentence for it would be a string no code path can
  // emit, which is the one thing a canon table must not carry.
  DENIED_DRIVE: (name: string) => `mounting a user drive is not allowed by your governance profile "${name}". Launch without drive.`,
  REFUSED_NO_GRANT: "drive: no user drive is allocated to you — ask an admin for an allocation",
  REFUSED_PAUSED: "drive: your allocation is paused by an admin",
  // {claim} is the template's claim name (sub, email_local).
  REFUSED_HOME_INVALID: (claim: string) =>
    `drive: your ${claim} cannot name a directory (lowercase letters and digits, then . _ -, up to 63 characters) — ask an admin to set your directory name`,
  REFUSED_HOME_MISSING: (name: string) => `drive: directory ${name} does not exist on the share — ask an admin to create it`,
  REFUSED_WRITABLE: "drive: your allocation is read-only; read_only:false cannot widen it",
  // {reason} is driveMountFor's own prose, composed in internal/api/
  // user_drives_run.go — the backend/runner mismatch ("it is a %q drive and
  // this deployment dispatches to %q") or, for a share, driveShareIsBindable's
  // host-root error. It is NOT an apiserver refusal: the console never asks the
  // cluster, and nothing on this path relays one.
  REFUSED_BACKEND: (reason: string) => `drive: this deployment cannot mount your drive (${reason})`,
  // [0] is the MOUNT'S POSITION, not a literal: validatePolicySpec prefixes
  // every mount error with workspace_mounts[i] (workspace_repos[i] for a
  // repo), so the canon spells the first mount's index and the string below
  // is the server's bytes for it.
  REFUSED_TARGET_RESERVED: "workspace_mounts[0]: target /home/agent/drive is reserved for the user drive",
} as const;

// ==================== §7.8 — DRIVE_RUN ======================================

// One <dt>/<dd> pair in IdentityWidget's <dl>, between Sandbox and Started,
// rendered ONLY when the run row carries a drive — which it does not in v1
// (§2.7); frozen here, shipped with run-row persistence. The name is quoted,
// never mono; the <dd>'s title carries the object name for the admin who
// hovers, and nothing else on the run page names it.
export const DRIVE_RUN = {
  RAIL_LABEL: "Drive",
  RAIL_VALUE: (name: string, mode: string) => `"${name}" · ${mode}`,
} as const;
