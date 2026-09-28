/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// DRAFT (M2 canon pending) — B4b, the clone of a run that ended.
export const RUN = {
  // The affordance the killed panel's own advice ("Start a new run if the work
  // still needs doing") never had.
  CLONE_CTA: "Start a run like this one",
  // What carried over, and — the half that matters — what deliberately did not.
  CLONE_NOTE:
    "Prefilled from this run — task, agent, barrier, policy and what it attached. Credentials and approvals are minted fresh.",
  // The one thing a clone CANNOT carry: an inline policy is never persisted
  // (internal/api/inline_policy.go attaches it with a nil id), so the run row
  // records only that there was one. A named ceiling beats a silent default.
  CLONE_INLINE_POLICY_CEILING:
    "This run used an inline policy, which isn't stored — pick a saved policy or write one again.",
  // DRAFT (M2) — §7.6's staged RUN_CLONE_CEILING_NOTE, which shipped nowhere
  // until now. It is the console half of §7 B4b's "create re-clamps": a member
  // cloning an admin's run is narrowed AT LAUNCH, not flattered in this form,
  // and the banner has to say so before they press Launch rather than after.
  CLONE_CEILING_NOTE:
    "Your ceiling applies again at launch — anything this run had above it is narrowed, with the reason.",
  // DRAFT (M2 canon pending) — F2-F5: a saved-policy reference that no longer
  // resolves (deleted elsewhere) needs its own reason; "pick a saved policy,
  // or write a custom one" is false once one WAS picked.
  POLICY_GONE: "That saved policy no longer exists — pick another.",
  // DRAFT (M2 canon pending) — F2-F2: the original sentence claimed the saved
  // lane merges nothing; runs_create.go's create door prepends the attached
  // Workspace card's mounts even when launching by policy_id. Named, not
  // silently contradicted.
  SAVED_POLICY_GOVERNS: (barrier: string, egress: string) =>
    `The stored spec governs this run — barrier floor ${barrier}, ${egress}. Your attached workspace mounts into it; nothing else on this page is merged.`,
  // #1200 review P2-1 — restored: exactly one installed+allowed tier does
  // NOT mean an admin set a floor (a Fence-only host with no governance
  // profile lands here too). TierPicker's decidedLine override renders this
  // instead of the default "set by your admin" line whenever the sole
  // survivor is NOT the governance ceiling's doing.
  BARRIER_ONLY_QUALIFIER: "— the only barrier this run can use.",
  // An inconclusive host probe never blocks launch and does not leave every
  // tier guessably selectable either: an untouched pick sends no
  // confinement_class at all, so the server's own read decides.
  BARRIER_UNKNOWN:
    "Couldn't check which barriers this host has — leave this alone and Wardyn will use the strongest one it can, or pick one yourself.",
  // #214 (owner comment on #214, corrected by #269's own SF-26 review round —
  // the original wording claimed a run had been created; genericFailure
  // (use-launch.ts) means the console cannot tell whether one was) — a
  // launch that fires and gets back no server-composed reason at all.
  LAUNCH_FAILED_TITLE: "Wardyn didn't answer the launch.",
  LAUNCH_FAILED_BODY: "A run may or may not have started — check the Runs board before trying again.",
  LAUNCH_FAILED_OPEN_RUN: "Open Runs",
  LAUNCH_FAILED_DISMISS: "Dismiss",
} as const;

// #214 (mock approved 2026-09-20) — a host with no confinement class it can
// enforce says so, rather than leaving Launch clickable for a run that can
// only fail after the click. LAUNCH_REASON/FINISH_GATE_* are stated beside
// the control they block, never in a tooltip.
export const NO_BARRIER = {
  CTA: "Set up a barrier",
  // #1328 review F2 — an ABSOLUTE route, correct for a caller reached the
  // same way regardless of view (New Run's own Launch reason: no separate
  // /admin/runs/new exists). A caller that can ALSO render inside the Admin
  // view's own setup funnel (mounted at /admin/setup) must NOT use this one —
  // it would drop an admin out of the Admin view (console-view.tsx's
  // viewVerdict). See RELATIVE_ROUTE and the shell/top-bar's own
  // view+operator-aware route below.
  ROUTE: "/setup?step=environment",
  // The Setup funnel's OWN Finish-setup gate is mounted at EITHER /setup or
  // /admin/setup — a query-only link resolves against whichever one is
  // already current, so it can never itself switch the view.
  RELATIVE_ROUTE: "?step=environment",
  LAUNCH_REASON: "No barrier can be built on this host, so no run can be confined.",
  BANNER_TITLE: "No barrier can be built on this host — runs can't launch.",
  BANNER_BODY:
    "Wardyn confines every run. Until a container runtime answers, there is nothing to confine it with.",
  FINISH_GATE_HEAD: "Setup can't finish without a barrier.",
  FINISH_GATE_REASON: "Wardyn confines every run.",
} as const;

