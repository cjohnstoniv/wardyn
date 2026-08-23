/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Permissioning copy canon (0.6, WS-A stage A-A) — the frozen canonical-strings
// table from docs/design/permissioning-prompt.md, transcribed verbatim. The
// admin Permissions screen (A-E/E2) and the member why-denied surfaces (A-E/E3)
// read these instead of retyping the copy, so the shipped wording can't drift
// from the reviewed mock (docs/design/permissioning-mock/index.html).
//
// Pure TS — no React, no fetch, no DOM. Nothing imports it yet: A-A is a mock
// round, and the screens that consume it land in A-E.
//
// Two laws are baked into the wording below and must survive any edit:
//   1. DOCTRINE — a capability bounds what the MEMBER chose, never what the
//      ADMIN pre-authorized. Workspace-, policy-, and scan-seeded egress is not
//      narrowed; only egress the member typed is.
//   2. A grant is amber, never green. A capability grant is not a success
//      state — it is a widened blast radius, and the console says so.

// ============================ Kinds ============================

// The closed set, in the order the admin screen renders them. Mirrors the Go
// slice in internal/api/capabilities.go (A-B) — a fifth kind is a Go constant
// plus a row here, no DDL.
export const CAPABILITY_KINDS = ["egress_host", "secret", "workspace", "image"] as const;
export type CapabilityKind = (typeof CAPABILITY_KINDS)[number];

// Whether granting this kind takes power away from members ("narrows" — the
// default posture already allows it) or gives it ("widens" — the default
// posture already refuses it). `image` is the only widening kind, and the
// enforcement toggle for it therefore reads completely differently: turning it
// on can never lock anyone out.
export type CapabilityDirection = "narrows" | "widens";

export interface KindCopy {
  /** Card + column label. */
  label: string;
  /** One line under the label: what this capability covers. */
  blurb: string;
  /** Label of the value field in the add-grant form. */
  valueLabel: string;
  /** Helper under the value field: the accepted value shape, verbatim. */
  valueHint: string;
  /** Body of the "Not enforced" state — today's member powers, stated plainly. */
  unenforced: string;
  /** Body of the "Enforced" state — what starts being refused. */
  enforced: string;
  direction: CapabilityDirection;
}

export const KIND: Record<CapabilityKind, KindCopy> = {
  egress_host: {
    label: "Egress hosts",
    blurb: "Which hosts a member may approve for their own run.",
    valueLabel: "Host",
    valueHint: "A host, or *.suffix for a domain and everything under it. Use * for every host.",
    unenforced: "Members can approve any host their own run asks for, and add any host to a run they launch.",
    enforced:
      "A member can only approve hosts granted to them. A host they add to a run they launch is dropped before it starts, with a warning naming it.",
    direction: "narrows",
  },
  secret: {
    label: "Secrets",
    blurb: "Which stored secrets a member's run may reference.",
    valueLabel: "Secret name",
    valueHint: "The exact secret name. Use * for every secret.",
    unenforced: "Members can reference any stored secret their run's ceiling already allows.",
    enforced:
      "A member's run can only reference secrets granted to them. Ungranted names are dropped before launch, and the Secrets page lists only what they hold.",
    direction: "narrows",
  },
  workspace: {
    label: "Workspaces",
    blurb: "Which workspaces a member may launch a run against.",
    valueLabel: "Workspace",
    valueHint: "One workspace. Use * for every workspace.",
    unenforced: "Members can launch a run against any workspace.",
    enforced:
      "A member can only launch against workspaces granted to them. The rest stay listed — a run against one is refused at launch, with the reason.",
    direction: "narrows",
  },
  image: {
    label: "Base images",
    blurb: "Which base images a member may name on a run of their own.",
    valueLabel: "Image ref",
    valueHint: "The exact image ref, registry and tag included. Use * for every image.",
    unenforced: "Members can't name their own base image at all. Runs use what the workspace carries.",
    enforced: "A member can name an image granted to them. Every other ref is still refused.",
    direction: "widens",
  },
};

// ============================ Admin surface ============================

export const PERM = {
  TITLE: "Permissions",
  LEAD: "Grant members and groups specific Wardyn capabilities. Each capability is enforced on its own — until you enforce one, nothing about it changes.",

  // The doctrine sentence, on the screen because it is the single most common
  // misread of the feature ("I granted two hosts, why did the run still reach
  // a third?").
  DOCTRINE:
    "A capability bounds what a member chose, never what an admin pre-authorized. Egress a workspace, a stored policy, or a scan already carries is never narrowed by a grant.",
  EXEMPT: "Admins, the admin token, and local mode are never bounded by these rules.",

  // ---- enforcement switch ----
  ENFORCEMENT_TITLE: "Enforcement",
  ENFORCEMENT_LEAD: "Turn a capability on to start refusing what isn't granted.",
  CHIP_OFF: "Not enforced",
  CHIP_ON: "Enforced",
  // The upgrade-from-0.5 banner: every kind off, no grants. This is what an
  // operator sees the first time they open the screen.
  DEFAULT_POSTURE:
    "Nothing is enforced yet. Members have exactly the powers they had before this screen existed, and adding a grant on its own changes nothing.",
  // Grants exist but the kind is off.
  ADVISORY:
    "Advisory until enforced. These grants are recorded and shown here, but nothing is refused while this capability is off.",
  // A deny is the one thing that bites with the switch off — the adoption
  // on-ramp (blocklist one contractor without bounding everyone).
  DENY_BEFORE_ENFORCE:
    "A deny applies even while this capability is not enforced — you can block one host for one person without bounding everyone.",

  // Confirming a switch-on. The zero-grant form is the lockout guard: turning
  // on `workspace` with no grants refuses every member run.
  ENFORCE_ON_TITLE: (kind: string) => `Enforce ${kind}?`,
  ENFORCE_ON_BODY: (n: number) =>
    `${n} member${n === 1 ? "" : "s"} ${n === 1 ? "is" : "are"} bounded by the grants below from their next request. Anything not granted starts being refused.`,
  ENFORCE_ON_ZERO:
    "There are no allow grants for this capability. Enforcing it now refuses every member request until you add one.",
  // ADDITION to §7.2 (0.6 implementation): the canon table froze one title and
  // an off-BODY, so the off-dialog asked "Enforce Egress hosts?" over a body
  // saying members go back and a button saying Stop enforcing. Same shape as
  // ENFORCE_ON_TITLE, same verb as ENFORCE_STOP — noted as an addition in
  // docs/design/permissioning-prompt.md §7.2.
  ENFORCE_OFF_TITLE: (kind: string) => `Stop enforcing ${kind}?`,
  ENFORCE_OFF_BODY: "Members go back to the powers they had before this capability was enforced. Denies still apply.",
  ENFORCE_CONFIRM: "Enforce",
  ENFORCE_STOP: "Stop enforcing",

  // ---- grant table ----
  GRANTS_TITLE: "Grants",
  COL_WHO: "Who",
  COL_CAPABILITY: "Capability",
  COL_VALUE: "Value",
  COL_EFFECT: "Effect",
  COL_ADDED: "Added",
  EFFECT_ALLOW: "Allow",
  EFFECT_DENY: "Deny",
  SUBJECT_USER: "User",
  SUBJECT_GROUP: "Group",
  SUBJECT_ALL: "Everyone signed in",
  PRECEDENCE:
    "A deny always wins — over an allow on the same person, over a group they're in, and over this capability being unenforced.",
  EMPTY_TITLE: "No grants yet",
  EMPTY_BODY: "Add a grant, then enforce its capability. Enforcing with nothing granted refuses everything.",
  REMOVE: "Remove",
  REMOVE_CONFIRM: (who: string) => `Remove this grant from ${who}? They lose it on their next request.`,

  // ---- add form ----
  ADD_TITLE: "Add a grant",
  ADD_CTA: "Add grant",
  FIELD_WHO: "Who",
  FIELD_CAPABILITY: "Capability",
  FIELD_EFFECT: "Effect",
  HINT_USER: "An email address or the sign-in subject id. Either one matches the same person.",
  HINT_GROUP: "A group or app-role name exactly as your identity provider sends it in the token.",
  HINT_ALL: "Every signed-in member. Admins are exempt.",
  DUPLICATE: "That grant already exists — its effect was updated.",

  // ---- group snapshot honesty ----
  SNAPSHOT_TITLE: "Groups are read at sign-in",
  SNAPSHOT_BODY:
    "Group membership is recorded once, when a member signs in. A group added in your identity provider reaches Wardyn on their next sign-in. Grants themselves take effect on the next request.",

  // A grant is a widened blast radius, not an achievement — this is the
  // one-liner the amber styling exists to carry.
  GRANT_IS_NOT_SUCCESS: "Every allow below is something a member can reach that they otherwise couldn't.",
};

// ============================ Member why-denied ============================

// Inline moments only — 0.6 ships no member-facing permissions screen. Member
// nav stays Runs · Approvals.
export const DENIED = {
  // Approvals: the Approve/Deny buttons disable, with the reason beside them.
  APPROVE_CHIP: "Not a host you're granted",
  APPROVE_BODY: (host: string) =>
    `${host} isn't in the hosts granted to you, so you can't decide this one. An admin can approve it, or grant you the host.`,
  // `always` is operator-only even for a host the member holds — an existing
  // rule the grant must not appear to have lifted.
  ALWAYS_STILL_ADMIN: "Always is admin-only, even for a host you're granted.",

  // New Run: workspace picker annotations. The list is NOT narrowed — visibility
  // is not capability — so the ungranted rows say why they'll refuse.
  WORKSPACE_CHIP: "Not granted",
  WORKSPACE_BODY: "A run against this workspace is refused at launch. Ask an admin to grant it to you.",

  // (§7.3's SECRET_DROPPED(n)/EGRESS_DROPPED(n) are deliberately NOT here. They
  // are count-shaped copy for a preflight/Review surface, and 0.6 ships none:
  // the drop is surfaced at launch instead, as one toast per dropped value
  // carrying the SERVER's text, which names the kind and the exact value
  // (internal/api/runs.go -> new-run/run-warnings.ts). A string defined here
  // and rendered nowhere is not canon, it is a claim — so it waits for the
  // surface that draws it.)

  // Secrets page, member view, `secret` enforced.
  SECRETS_NARROWED: "Only secrets granted to you are listed.",

  // The member half of the stale-groups story.
  STALE_GROUPS:
    "You signed in before Wardyn started recording your groups. If a permission looks missing, sign out and back in.",
};
