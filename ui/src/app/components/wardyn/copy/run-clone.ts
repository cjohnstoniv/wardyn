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
  // Exactly one installed class meets the floor: nothing to ask, so the Seg
  // collapses to this sentence instead (new-run-screen.tsx).
  BARRIER_ONLY_QUALIFIER: "— the only barrier this run can use.",
  // An inconclusive host probe never blocks launch and does not leave every
  // tier guessably selectable either: an untouched pick sends no
  // confinement_class at all, so the server's own read decides.
  BARRIER_UNKNOWN:
    "Couldn't check which barriers this host has — leave this alone and Wardyn will use the strongest one it can, or pick one yourself.",
} as const;

