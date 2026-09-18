/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The 0.7.6 model-access / re-authentication strings, in their OWN module.
//
// Not in wardyn/copy.ts, and that is a rule rather than a preference: copy.ts
// is 961 lines against a 1000-line cap with no allowlist entry
// (scripts/check-file-size.sh), so this round's strings would have taken it
// over on their own. copy.ts keeps only the one-line map entries that have to
// live beside their maps.
//
// The BUTTON label is not here either: it is AGENTS.SIGN_IN_AWS
// (lib/workspace-providers-copy.ts), reused and never retyped, so the strip,
// the rail, the failure block and the two card CTAs cannot drift into four
// spellings of one control.

// DRAFT (M2 canon pending) — ruled by the UX round (S2, S3, S13, nits)
export const MODEL_ACCESS_BANNER = {
  // The first-run state. It states the NEED, never a verdict about what will
  // happen at launch (Codex #12): "refused at launch" is true of the
  // interactive and ephemeral-workspace shapes but not of every composed
  // request — a scan run makes no model call at all — and the server's own 422
  // says it at the click, where it is certainly true.
  NOT_SIGNED_IN: "You are not signed in to AWS — Claude Code needs your AWS sign-in",
  // "lapsed" would be FALSE for a pin-contradicted session, which is live and
  // the wrong identity (internal/api/modelaccess.go's
  // modelAccessPinContradictedAction) — one sentence true of both (round-2 UX
  // B5); the server's action carries the account/role pair.
  EXPIRED: "Your AWS sign-in no longer works for Claude Code — sign in again",
  // The same fact WITHOUT its imperative, for when a page surface has claimed
  // the door (round-2 UX S7): the strip keeps its sentence and drops its
  // button, and "sign in again" beside no button points at nothing.
  EXPIRED_SHORT: "Your AWS sign-in no longer works for Claude Code",
  // {when} = relativeTime(deadline) ("in 3h"); absoluteTime(deadline) rides the
  // element's title (round-2 UX B6). The deadline is IN the sentence, which is
  // why no separate server action line renders for this state (S1).
  EXPIRING: "Your AWS sign-in lapses {when}",
  // The shared row's ADMIN: their own sign-in repairs it, and the sentence
  // names the blast radius because every run rides that one credential (S2).
  SHARED_ADMIN_EXPIRING: "The shared AWS sign-in every Claude Code run uses lapses {when}",
  SHARED_ADMIN_EXPIRED:
    "The shared AWS sign-in no longer works — every Claude Code run needs it; sign in again",
  // shared_expired (member): the server's action line renders ALONE — no
  // sentence of ours, because it would be a second sentence saying the one fact
  // (S13), and there is no button: nobody but the admin can repair it.
  DIALOG_TITLE: "Sign in to AWS",
  // <DialogDescription className="sr-only"> — the dialog's accessible
  // description. Sighted readers never see it; a screen-reader user hears what
  // this dialog is about before the login terminal starts writing into it.
  DIALOG_DESCRIPTION:
    "Signs you in to your organization's AWS access portal for your Claude Code runs. The sign-in runs in the terminal in this dialog.",
  // CONSOLE-RULES §9's transient case: the strip vanishes on the next status
  // read, and a surface that disappears is not a confirmation.
  SIGNED_IN_TOAST: "Signed in to AWS — your runs can use your session now",
  // The per-viewer, per-session set-aside. Offered ONLY where the viewer cannot
  // act (O-11, W0-mock ruling 3): the first-run state, and a dead SHARED
  // credential for a non-operator. A lapse of something the person already had
  // is never dismissable.
  NOT_NOW: "Not now",
} as const;

// DRAFT (M2 canon pending) — the rail's per-PERSON model-access lines. Distinct
// from RAIL_CREDENTIAL, which states where the credential LANDS: these state
// whether the person launching has one at all. "refused at launch" is the
// server's word: create answers 422 for every model-calling shape while the
// declared per-user lane is unsatisfied (enforceCreateLLMMechanism); a scan or
// exec run needs no model credential and shows no line.
export const RAIL_MODEL_ACCESS = {
  // UX round S1: "This run is refused at launch." was a verdict on a run that does not exist yet.
  // Codex #12: NON-VERDICT copy — "Launch is refused" over-claims for some composed shapes (a batch with a
  // pure-ephemeral primary emits workspace_id in wizard-spec.ts; llmMechanismGateApplies exempts a
  // non-interactive workspace-bound request at create while dispatch can refuse later); the rail states
  // credential readiness, never the exact refusal stage, and never duplicates the server gate.
  NOT_SIGNED_IN: "Sign in to AWS before you launch — Claude Code needs your AWS sign-in.",
  EXPIRED: "Your AWS sign-in no longer works for Claude Code — sign in again before you launch.",   // true of a pin mismatch too (B5)
  EXPIRING: (when: string) => `Your AWS sign-in lapses ${when} — sign in again soon.`,
  SHARED_EXPIRED: "Your admin's AWS credential has expired — Claude Code runs need it reconnected.",
  SIGN_IN_ARIA: "Sign in to AWS — from the New Run rail",
  // The existing inline warning, lifted (strings law) — a DEPLOYMENT with no
  // model path at all, a different fact from the four above.
  NO_PROVIDER: "No model provider is connected. This run launches; its first model call fails.",
  NO_PROVIDER_CTA: "Connect →",
} as const;
