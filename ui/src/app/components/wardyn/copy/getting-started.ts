/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Member Getting Started (Phase 5) — the six-SectionCard page a member lands
// on at /setup. Canon per the approved mock; member-getting-started.tsx is
// the sole reader.
export const MEMBER_GETTING_STARTED = {
  TITLE: "Getting started",
  SUBTITLE: "You're a member of this Wardyn. Your admin set the ceiling; you run inside it.",
  UNREACHABLE_TITLE: "Couldn't reach Wardyn.",
  UNREACHABLE_BODY:
    "Nothing below is marked done until it can be checked — a broken connection is not a finished step.",
  RETRY: "Retry",
  SETUP_SUMMARY_TITLE: "What's set up for you",
  SETUP_SUMMARY_HELPER: "Your admin configured the barrier, network and shared credentials. Your runs inherit them.",
  // Restored (#541 fix review): SETUP_SUMMARY_HELPER's "shared credentials …
  // your runs inherit them" is FALSE whenever the credential is per person —
  // a legacy per_user AWS-SSO roster row, or any install with a provider
  // block at all (every provider is a per-person credential by design, never
  // shared — model-connections.ts's own doc). member-getting-started.tsx
  // picks this one instead in both cases.
  SETUP_SUMMARY_HELPER_PER_USER:
    "Your admin configured the barrier, network and the model-access lane. Model access uses your own AWS sign-in; your runs inherit the rest.",
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

// #160 — TitleGroup's second chip row (runs/title-group.tsx): what a group's
// runs are waiting on, one counted chip per reason instead of a bare count.
// `n` is always the GROUP's count for that reason — CONSOLE-RULES §10's "say
// how many", never "some runs need attention".
export const RUNS_WAIT = {
  HELD: (n: number) => `${n} awaiting confirmation`,
  REAUTH: (n: number) => `${n} awaiting AWS sign-in`,
  STARTING: (n: number) => `${n} waiting to start`,
  // Absent while anything else waits; a single UNCOUNTED chip, and only once
  // the approvals fetch has resolved enough to know the group is really clean.
  NONE: "Nothing waiting",
  // The pre-fetch window: nothing derived from the approvals fetch may paint
  // before it resolves, so this stands alone rather than reading as "nothing
  // is held" (the empty-Map default's lie).
  CHECKING: "Checking…",
} as const;

