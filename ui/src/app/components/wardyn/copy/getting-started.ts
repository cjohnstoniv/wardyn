/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Member Getting Started (Phase 5) — the six-SectionCard page a member lands
// on at /setup. Canon per the approved mock; member-getting-started.tsx is
// the sole reader.
export const MEMBER_GETTING_STARTED = {
  TITLE: "Getting started",
  // UT-7a: introduces the caller's own user type by name (packet B's "You're
  // set up as a Portfolio manager: {description}", without the article — a
  // type name is admin-typed, and "a Engineer" reads wrong). name/description
  // are the SESSION's stamped type (health.ts's Me.user_type); every SSO
  // sign-in stamps one, Standard user at the least. undefined only for a
  // caller with none (the admin token, local mode), which keeps the original
  // sentence. An empty description reads the same as none, and a trailing
  // period on one is dropped so the sentence never ends "..".
  SUBTITLE: (typeName?: string, typeDescription?: string): string => {
    if (!typeName) return "You're a member of this Wardyn. Your admin set the ceiling; you run inside it.";
    const about = typeDescription?.trim().replace(/\.+$/, "");
    return `You're set up as ${typeName}${about ? `: ${about}` : ""}. Your admin set the ceiling; you run inside it.`;
  },
  UNREACHABLE_TITLE: "Couldn't reach Wardyn.",
  UNREACHABLE_BODY:
    "Nothing below is marked done until it can be checked — a broken connection is not a finished step.",
  RETRY: "Retry",
  SETUP_SUMMARY_TITLE: "What's set up for you",
  BARRIER_CHIP: (label: string) => `Barrier · ${label}`,
  SIGNIN_SSO_CHIP: "Sign-in · SSO",
  WORKSPACE_TITLE: "Add your workspace",
  WORKSPACE_BODY: "A repo or directory a run can attach. Runs can only attach what is listed here.",
  WORKSPACE_ERROR: "Couldn't check your workspaces.",
  WORKSPACE_ACTION: "Add workspace",
  FIRST_RUN_TITLE: "Your first run",
  FIRST_RUN_BODY: "Launch a governed run against your workspace.",
  FIRST_RUN_HINT:
    "Your policy is clamped to your admin's ceiling. Preflight shows exactly what launch will do — read its warnings before you go.",
  FIRST_RUN_ACTION: "New run",
  APPROVALS_TITLE: "Approvals you can decide",
  APPROVALS_BODY: "When one of your runs reaches a host that isn't on the list, it holds at the door.",
  APPROVALS_HINT:
    "You decide — once, for this run, until, or always. Credential and tool-call approvals stay with your admin.",
  APPROVALS_ACTION: "Open approvals",
  CONNECT_TITLE: "Connect your tools",
  CONNECT_BODY: "Attach from your own terminal or editor over SSH.",
  CONNECT_HINT_PREFIX: "Register a key once: ",
  CONNECT_COMMAND: "wardyn ssh-key ensure",
  CONNECT_ACTION: "Add SSH key",
  // M-6 (D5, admin-member-modes-design.md §4.8): demos are sandbox runs, a
  // user act, so they moved here from the admin funnel — same section labels
  // (SETUP.PHASE_DEMOS_*, modes-b.html), reused rather than retyped, since
  // both name the exact same two Demo.section groups (demo-catalog.ts).
  DEMOS_EGRESS_TITLE: "Egress demos",
  DEMOS_SECRETS_TITLE: "Secrets demos",
  DEMO_OPEN: "Open",
  // Packet M-B (modes-b.html): the "Your model connections · in Your account"
  // row, which links to /account.
  MODEL_CONNECTIONS: "Your model connections",
  MODEL_CONNECTIONS_WHERE: "in Your account",
} as const;

// DRAFT (M2 canon pending) — X3-F4, the MEMBER's empty runs board. The operator
// first-run funnel it replaces is a host-barrier readout plus a setup
// checklist: redacted blank for a member, and pointing at routes their role
// cannot reach. These lines are what a member can actually do instead. Sited
// after MEMBER_GETTING_STARTED because GUIDE is that page's own title — the
// link names where it lands, and a second literal is how the two drift.
export const RUNS_MEMBER_EMPTY = {
  TITLE: "Runs you launch appear here",
  BODY: "Nothing is running yet. Start one against a workspace your admin has made available to you.",
  ACTION: "New run",
  GUIDE: MEMBER_GETTING_STARTED.TITLE,
} as const;

// RUNS_WAIT (title-group.tsx's per-group wait chip) was removed here —
// #1197 L3 review F12: title-group.tsx is gone (the Runs landing page
// groups by need then time, not by title), and it was RUNS_WAIT's only
// consumer.

