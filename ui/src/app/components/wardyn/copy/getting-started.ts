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
  SETUP_SUMMARY_HELPER: "Your admin configured the barrier, network and shared credentials. Your runs inherit them.",
  BARRIER_CHIP: (label: string) => `Barrier · ${label}`,
  MODEL_ACCESS_OWN_CHIP: "Model access · Your key",
  MODEL_ACCESS_PROVIDED_CHIP: "Model access · Provided by your admin",
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
  // DRAFT (M2 canon pending) — Appendix A finding 2b. Under a per_user
  // roster row the credential is specifically NOT shared and NOT inherited
  // — that is the entire point of the lane, and the chip beside this
  // sentence already says so.
  SETUP_SUMMARY_HELPER_PER_USER:
    "Your admin configured the barrier, network and the model-access lane. Model access uses your own AWS sign-in; your runs inherit the rest.",
  // DRAFT (M2 canon pending) — U-13 (a11y). This page renders TWO buttons whose
  // visible text is "Sign in to AWS" (this card's and "Your model key"'s) plus a
  // plain-text action line saying the same words, so a screen reader's button
  // list carried the same name twice with nothing to choose by. The visible text
  // is unchanged; the accessible name adds the section. It STARTS with
  // AGENTS.SIGN_IN_AWS so a lookup by the visible name still finds it (pinned in
  // member-getting-started.test.tsx).
  SIGN_IN_AWS_ARIA_SUMMARY: "Sign in to AWS — from What's set up for you",
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
  // The card's own sentence once a hold isHeld no longer counts as live (the
  // 60-minute stale-hold ceiling, lib/types/approvals.ts) — replaces the
  // per-run reason line, not the group's chip below.
  STALE_CARD: "Was held — check the run",
  // The header's uncounted-elsewhere chip for the same fact, at group
  // granularity: a degraded claim, not silence about what happened here.
  STALE_GROUP: (n: number) => (n === 1 ? "1 was held" : `${n} were held`),
} as const;

