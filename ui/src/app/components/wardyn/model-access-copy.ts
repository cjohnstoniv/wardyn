/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { AGENTS } from "../../lib/workspace-providers-copy";

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
  // The shared row's ADMIN reads their own repair sentence, never the
  // member's "ask them" line about themselves (review-1 S2; mirrors
  // MODEL_ACCESS_BANNER.SHARED_ADMIN_EXPIRED, reworded for the rail's
  // "before you launch" voice).
  SHARED_ADMIN_EXPIRED: "The shared AWS sign-in no longer works — sign in again before you launch.",
  SIGN_IN_ARIA: "Sign in to AWS — from the New Run rail",
  // The existing inline warning, lifted (strings law) — a DEPLOYMENT with no
  // model path at all, a different fact from the four above.
  NO_PROVIDER: "No model provider is connected. This run launches; its first model call fails.",
  NO_PROVIDER_CTA: "Connect →",
} as const;

// DRAFT (M2 canon pending) — ruled by the UX rounds (B7, S7, S8)
//
// The FAILED run's own door (0.7.6 Finding 3). Its sentence is the SERVER's,
// rendered from the run's failure_hint exactly as it always was; these two
// strings are everything the console adds.
export const MODEL_ACCESS_RUN_DOOR = {
  // What the button does NOT do, said before the person clicks it: signing in
  // here does not restart anything. It names the run HEADER rather than the
  // control on it ("Start a run like this one"), because this block also
  // renders inside focus mode, which portals the terminal pane alone and never
  // draws that header (round-2 UX B7 / S7).
  NOTE: "Sign in here. This run stays failed — relaunch it from the run header.",
  // The accessible name, distinct from every other "Sign in to AWS" a page can
  // carry (S8; the SIGN_IN_AWS_ARIA_CARD precedent in wardyn/copy.ts). The
  // visible label stays AGENTS.SIGN_IN_AWS — one spelling of one control.
  SIGN_IN_ARIA: "Sign in to AWS — for this failed run",
} as const;

// DRAFT (M2 canon pending) — the mid-run re-auth row (Finding 4), ruled by the
// UX rounds (B2, B3, S6, S8) and Codex #5.
//
// THE RULE THESE STRINGS FOLLOW: say what the ROW PROVES, and nothing more. A
// PENDING credential_reauth row is evidence a sign-in was ASKED FOR. It is not
// evidence that a request is still parked — the hold may have timed out, the
// SDK may have disconnected, or the final resolve may have refused a roster
// drift — and the console has no way to tell. So nothing here promises that a
// run will continue.
export const REAUTH_ROW = {
  // States the need, not the state of the request.
  label: "AWS sign-in needed",
  // Both branches, because the console cannot tell which one the person is in.
  hint: "If a model call is waiting on your sign-in, the run continues after you sign in; if it already failed, relaunch it.",
  // AGENTS.SIGN_IN_AWS BY REFERENCE, not a second copy of its letters (general
  // N1): the comment used to claim reuse while the string was retyped, which is
  // exactly how one control acquires four spellings.
  action: AGENTS.SIGN_IN_AWS,
  // The row's button has a name of its own, because a strip can carry several
  // and "Sign in to AWS" three times is three identical accessible names.
  ariaLabel: "Sign in to AWS — to resume this run",
  // The shared lane: a MEMBER's run can raise a request only an admin can
  // satisfy, so the row states the instruction instead of offering a dead door
  // (Codex #7 — the audience decides which of the two renders).
  sharedMemberHint: "This run uses the shared AWS sign-in — ask your admin to sign in again.",
} as const;

// The strip's heading when every pending row is a re-auth. "approve to let it
// through" is FALSE here (nobody approves this kind), and so is any sentence
// claiming the run is paused (Codex #5) — this names the NEED.
export const REAUTH_HEADING = "AWS sign-in needed — sign in to let this run's model calls through";

// The board card's and the cockpit header's chip. It keeps the COUNT (round-2
// UX S8): a count-free string would hide a co-pending egress approval, and the
// person would sign in and watch the run sit there.
export const waitingReauth = (n: number): string =>
  n > 1 ? `Waiting for your AWS sign-in · ${n - 1} more waiting` : "Waiting for your AWS sign-in";

// LiveApprovals' toast when a re-auth row leaves PENDING as APPROVED — the
// person's only "it worked" moment, since the row vanishes on the next 4s poll.
// Two words, because APPROVED proves the sign-in landed and proves NOTHING
// about the run (round-2 UX S10, Codex #5): "the run is continuing" would be a
// claim on forgeable-to-the-console evidence.
export const REAUTH_SIGNED_IN_TOAST = "Signed in";

// The /approvals card's title for the kind (UX round B3): a request that mints
// nothing must not be titled "Mint a scoped credential", and the card carries
// no blast-radius claim. "paused" is the word for PEOPLE; "held" is the wire's
// (round-2 UX S6).
export const REAUTH_TITLE = "Sign in to AWS again — this run is paused";
