/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// People step (step-bodies.tsx's DeploymentStep) — canon per the approved mock.
export const PEOPLE_STEP = {
  SINGLE_USER_CHIP: "Single-user",
  MULTI_USER_CHIP: "Multi-user",
  SINGLE_USER_LEDE_LOCAL:
    "One admin credential — no sign-in at all — anyone who reaches this console on this machine is the admin. No per-person identity.",
  SINGLE_USER_LEDE_TOKEN:
    "One admin credential — the token the installer printed. No per-person identity.",
  SINGLE_USER_BODY:
    "Just you. Whoever holds the admin token (or reaches a local-mode console) is the admin; runs, policies, secrets and approvals are all yours. There is no member role until people sign in as themselves.",
  SINGLE_USER_SSO_NOTE_PREFIX:
    "To add people, configure SSO: each person gets their own identity and an admin or member role, and members get their own Getting Started. The recipe is in ",
  SINGLE_USER_SSO_NOTE_DOC: "docs/OPERATIONS.md",
  SINGLE_USER_SSO_NOTE_SUFFIX: ', "Second user, same host".',
  MULTI_USER_LEDE: "People sign in with SSO; each is an admin or a member, per your role map.",
  // The PREFIX/SUFFIX pair must name all three sources a role can come from —
  // WARDYN_OIDC_ROLE_MAP, the operator allowlist, AND a console row (the
  // acting-surface table right below this lede) — not just the env var and
  // the allowlist, or the lede would contradict the table.
  // PREFIX ends with ONE trailing space (before MULTI_USER_ROLES_VAR is
  // concatenated in) — copy the literal string, do not trim it.
  MULTI_USER_ROLES_PREFIX: "Roles come from the mappings below — your chart's ",
  MULTI_USER_ROLES_VAR: "WARDYN_OIDC_ROLE_MAP",
  MULTI_USER_ROLES_SUFFIX: ", console rows added here, or the operator allowlist.",
  MULTI_USER_SSO_CHIP: "SSO",
  MULTI_USER_ADMINS_LABEL: "Admins",
  MULTI_USER_ADMINS_BODY: " set the ceiling — policies, secrets, workspaces, site configuration.",
  MULTI_USER_MEMBERS_LABEL: "Members",
  MULTI_USER_MEMBERS_BODY:
    " run inside it — their own workspaces, runs, approvals and SSH keys. They land on their own Getting Started the first time they sign in.",
  MULTI_USER_PERMISSIONS_ACTION: "Open Permissions",
  MULTI_USER_PERMISSIONS_HINT: "Capability grants, per person or group",
} as const;

// DRAFT (M2 canon pending) — staged in workspace-providers-prompt.md §7.6
// ("U1 → corp-network-step / wardyn/copy.ts (B2, F22)"), parsed by nothing
// today; each row moves into its lane's own frozen table at the M2 sitting.
// Rendered here ahead of that sitting because the states themselves (the save
// note, the trusted-CA count) already exist and shipping words for them beats
// a blank control.
// 0.7.3 F6: this block carries no CONFINEMENT_NETPOL_* rows (app-shell.tsx's
// header chip was their only consumer) — the netpol verdict lives on the
// setup Environment step alone; see docs/design/workspace-providers-prompt.md
// §7.6 for the retired rows.
// M-6 (QM-8/§4.8, admin-member-modes-design.md, modes-b.html) — the admin
// funnel's Finish step, once the demos and the "Your work" name both left it.
export const SETUP = {
  FINISH_TITLE: "Finish",
  FINISH_SWITCH: "Switch to user view",
} as const;

export const SITE = {
  // B2: the site-config save path's own note — a change here does not reach a
  // run already going (the egress sidecar compiles its config once at sandbox
  // start).
  SAVE_NOTE: "Saved. This applies to runs started from now — a run already going keeps the network settings it started with.",
  // F22: the Network step's trusted-CA count, from /setup/status
  // (trusted_ca_certs) — the inline ternary (§5 #9), never a second helper.
  TRUSTED_CA_COUNT: (n: number) => `${n} trusted CA certificate${n === 1 ? "" : "s"}`,
  // #492 — the same If-Match discipline sign-in-help-card.tsx's own
  // SIGNIN_HELP.SAVED_ELSEWHERE names for its card, worded for a step rather
  // than a card: setup-screen.tsx's saveSiteConfig throws this in place of
  // the server's raw "If-Match does not match…" refusal on a 412, so the
  // toast every corp-network save already shows (useSiteConfigStep's mutate)
  // reads as a sentence an admin acts on, not an HTTP precondition.
  SAVED_ELSEWHERE:
    "Someone else saved this deployment's site config since this step last loaded it. Reloaded the latest — your change here wasn't saved; make it again if it still applies.",
} as const;

