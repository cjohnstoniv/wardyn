/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { ApprovalRequest } from "../../lib/types";
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
  // happen at launch: "refused at launch" is true of the interactive and
  // ephemeral-workspace shapes but not of every composed request — a scan run
  // makes no model call at all — and the server's own 422 says it at the
  // click, where it is certainly true.
  NOT_SIGNED_IN: "You are not signed in to AWS — Claude Code needs your AWS sign-in",
  // "lapsed" would be FALSE for a pin-contradicted session, which is live and
  // the wrong identity (internal/api/modelaccess.go's
  // modelAccessPinContradictedAction) — one sentence true of both; the
  // server's action carries the account/role pair.
  EXPIRED: "Your AWS sign-in no longer works for Claude Code — sign in again",
  // The same fact WITHOUT its imperative, for when a page surface has claimed
  // the door: the strip keeps its sentence and drops its button, and "sign in
  // again" beside no button points at nothing.
  EXPIRED_SHORT: "Your AWS sign-in no longer works for Claude Code",
  // {when} = relativeTime(deadline) ("in 3h"); absoluteTime(deadline) rides the
  // element's title. The deadline is IN the sentence, which is why no separate
  // server action line renders for this state.
  EXPIRING: "Your AWS sign-in lapses {when}",
  // The shared row's ADMIN: their own sign-in repairs it, and the sentence
  // names the blast radius because every run rides that one credential.
  SHARED_ADMIN_EXPIRING: "The shared AWS sign-in every Claude Code run uses lapses {when}",
  SHARED_ADMIN_EXPIRED:
    "The shared AWS sign-in no longer works — every Claude Code run needs it; sign in again",
  // shared_expired (member): the server's action line renders ALONE — no
  // sentence of ours, because it would be a second sentence saying the one
  // fact, and there is no button: nobody but the admin can repair it.
  DIALOG_TITLE: "Sign in to AWS",
  // <DialogDescription className="sr-only"> — the dialog's accessible
  // description. Sighted readers never see it; a screen-reader user hears what
  // this dialog is about before the login terminal starts writing into it.
  DIALOG_DESCRIPTION:
    "Signs you in to your organization's AWS access portal for your Claude Code runs. The sign-in runs in the terminal in this dialog.",
  // CONSOLE-RULES §9's transient case: the strip vanishes on the next status
  // read, and a surface that disappears is not a confirmation.
  SIGNED_IN_TOAST: "Signed in to AWS — your runs can use your session now",
  // Canon (packet E, Q151-2) — the provider sign-in doors' one line about the
  // sandbox the person watches start.
  DIALOG_CLEANUP_NOTE: "This sign-in runs in its own sandbox. It is stopped as soon as your sign-in is stored.",
  // B9 (packet MP-D): beside the server's sentence for a refusal that landed
  // after the person left New Run.
  REFUSAL_DISMISS: "Dismiss",
  // The per-viewer, per-session set-aside. Offered ONLY where the viewer cannot
  // act (W0-mock ruling 3): the first-run state, and a dead SHARED credential
  // for a non-operator. A lapse of something the person already had is never
  // dismissable.
  NOT_NOW: "Not now",
  // The strip's button has a name of its own ON /settings, where a MEMBER sees
  // it beside the Settings card's own disabled "Sign in to AWS" (the strip
  // deliberately stays there, so the collision is with a dead control rather
  // than a live one). getByRole matches a disabled button,
  // so without this one page carries two controls with one accessible name.
  // The rail's precedent: RAIL_MODEL_ACCESS.SIGN_IN_ARIA. Elsewhere the strip
  // is the only "Sign in to AWS" on the page and keeps the plain label, which
  // is also what the live SSO walk locates it by.

} as const;

// DRAFT (M2 canon pending) — the rail's per-PERSON model-access lines. Distinct
// from RAIL_CREDENTIAL, which states where the credential LANDS: these state
// whether the person launching has one at all. "refused at launch" is the
// server's word: create answers 422 for every model-calling shape while the
// declared per-user lane is unsatisfied (enforceCreateLLMMechanism); a scan or
// exec run needs no model credential and shows no line.
export const RAIL_MODEL_ACCESS = {
  // NON-VERDICT copy: "refused at launch" over-claims for some composed shapes (a batch with a
  // pure-ephemeral primary emits workspace_id in wizard-spec.ts; llmMechanismGateApplies exempts a
  // non-interactive workspace-bound request at create while dispatch can refuse later); the rail states
  // credential readiness, never the exact refusal stage, and never duplicates the server gate.
  NOT_SIGNED_IN: "Sign in to AWS before you launch — Claude Code needs your AWS sign-in.",
  EXPIRED: "Your AWS sign-in no longer works for Claude Code — sign in again before you launch.",   // true of a pin mismatch too
  EXPIRING: (when: string) => `Your AWS sign-in lapses ${when} — sign in again soon.`,
  SHARED_EXPIRED: "Your admin's AWS credential has expired — Claude Code runs need it reconnected.",
  // The shared row's ADMIN reads their own repair sentence, never the
  // member's "ask them" line about themselves (mirrors
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
  // draws that header.
  NOTE: "Sign in here. This run stays failed — relaunch it from the run header.",
  // The accessible name, distinct from every other "Sign in to AWS" a page can
  // carry (the SIGN_IN_AWS_ARIA_CARD precedent in wardyn/copy.ts). The
  // visible label stays AGENTS.SIGN_IN_AWS — one spelling of one control.
  SIGN_IN_ARIA: "Sign in to AWS — for this failed run",
  // #543 (packet 1, canon Table 2): a provider run's refusal opens that
  // provider's own door, so the Claude, key and token buttons get names on
  // SIGN_IN_ARIA's pattern, and the key and token doors their own note.
  SIGN_IN_CLAUDE_ARIA: "Sign in to Claude — for this failed run",
  ADD_KEY_ARIA: "Add your key — for this failed run",
  ADD_TOKEN_ARIA: "Add your token — for this failed run",
  NOTE_KEY: "Add it here. This run stays failed — relaunch it from the run header.",
  // Anyone but the owner, and every run in the Admin view: the credential is
  // the owner's alone, so there is no door.
  NOT_OWNER: (owner: string) => `It ran on ${owner}'s own credential — only they can reconnect it.`,
} as const;

// DRAFT (M2 canon pending) — the mid-run re-auth row (Finding 4), ruled by the
// UX rounds (B2, B3, S6, S8) and Codex #5.
//
// The rule these strings follow: say what the ROW PROVES, and nothing more. A
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
  // AGENTS.SIGN_IN_AWS BY REFERENCE, not a second copy of its letters: a
  // comment claiming reuse while the string is retyped is exactly how one
  // control acquires four spellings.
  action: AGENTS.SIGN_IN_AWS,
  // The row's button has a name of its own, because a strip can carry several
  // and "Sign in to AWS" three times is three identical accessible names.
  ariaLabel: "Sign in to AWS — to resume this run",
  // The shared lane: a MEMBER's run can raise a request only an admin can
  // satisfy, so the row states the instruction instead of offering a dead door
  // (Codex #7 — the audience decides which of the two renders).
  sharedMemberHint: "This run uses the shared AWS sign-in — ask your admin to sign in again.",
  // The PER_USER lane's non-owner — an admin reading a member's held run. The
  // admin's own sign-in captures into the ADMIN's scope and can never resolve
  // this row (reauthResolvableBy), so the sentence names whose sign-in is
  // awaited and carries no button. "the run owner" stands in when the row
  // names no owner — a sentence with an empty possessive is worse than a
  // generic one.
  notYoursHint: (owner: string) =>
    `Waiting on ${owner || "the run owner"}'s AWS sign-in — only they can complete it.`,
  // #543: under the card's title, which AWS provider the hold is for — a
  // sign-in to another one cannot clear it.
  PROVIDER: (name: string) => `Model provider · ${name}`,
} as const;

/** Who a held re-auth row is addressed to, for THIS viewer. */
export interface ReauthAudience {
  /** Whether this viewer's own sign-in can clear the row. */
  canAct: boolean;
  /** The SHARED lane: the credential is the operator's, not the row owner's. */
  shared: boolean;
  /** The subject a capture is matched against; "" on a shared row. */
  owner: string;
  /** The model provider the hold names (#530), "" on a roster run's row. Its
   *  door is the one that opens: a sign-in to another AWS provider cannot
   *  clear it (#543). */
  provider: string;
  /** The row names this viewer — true for the admin's own run in the Admin
   *  view too, where a provider's door does not open (canAct is false). */
  mine: boolean;
}

/**
 * reauthAudience — ONE ownership rule for the held-run sign-in door, mirroring
 * the server's own admission test (reauthResolvableBy, internal/api's
 * injection_awssso.go): a capture lands in the CAPTURER's scope, so a per_user
 * row is resolvable only by the subject named on it, and a shared row only by
 * an operator, whose sign-in is the one the shared credential holds.
 *
 * Both surfaces that render the door — the cockpit row and the /approvals card
 * — call this, because two copies of the rule are two audiences that can
 * disagree, and the disagreement reads as a button the server then refuses.
 *
 * `principal` must be the RESOLVED viewer subject (door.principal): "" while
 * /me is in flight, which grades every per_user row as "not yours" — the
 * fail-closed direction, and the same one failure-block.tsx's `!!principal`
 * takes.
 *
 * `view` is the page's console view: a provider's door opens in the User view
 * only (packet MP-E), so in the Admin view nobody is offered it (#543).
 */
export function reauthAudience(
  request: Pick<ApprovalRequest, "requested_scope">,
  viewer: { operator: boolean; principal: string; view?: "admin" | "user" },
): ReauthAudience {
  const shared = String((request.requested_scope?.credential_source as string) ?? "") === "shared";
  const owner = String((request.requested_scope?.owner as string) ?? "");
  const provider = String((request.requested_scope?.provider as string) ?? "");
  const mine = !!viewer.principal && owner === viewer.principal;
  const canAct = shared ? viewer.operator : mine && !(provider && viewer.view === "admin");
  return { canAct, shared, owner, provider, mine };
}

/** The row's sentence for that audience — the door's own hint, or the one
 *  sentence that names who can actually clear it. */
export function reauthRowHint(audience: ReauthAudience): string {
  if (audience.canAct) return REAUTH_ROW.hint;
  return audience.shared ? REAUTH_ROW.sharedMemberHint : REAUTH_ROW.notYoursHint(audience.owner);
}

// The strip's heading when every pending row is a re-auth. "approve to let it
// through" is FALSE here (nobody approves this kind), and so is any sentence
// claiming the run is paused (Codex #5) — this names the NEED.
export const REAUTH_HEADING = "AWS sign-in needed — sign in to let this run's model calls through";

// The board card's and the cockpit header's chip, RE-EXPORTED from a leaf
// module. It is defined in lib/reauth-waiting-copy.ts because the Runs board
// carries it and the board is on the EAGER graph, while everything else in this
// file is lazy-side: defining it here put this module — and, through its AGENTS
// import, lib/workspace-providers-copy.ts — into the entry chunk. The re-export
// keeps ONE definition and keeps this file the place a reader looks.
export { waitingReauth } from "../../lib/reauth-waiting-copy";

// LiveApprovals' toast when a re-auth row leaves PENDING as APPROVED — the
// person's only "it worked" moment, since the row vanishes on the next 4s poll.
// Two words, because APPROVED proves the sign-in landed and proves NOTHING
// about the run (Codex #5): "the run is continuing" would be a claim on
// forgeable-to-the-console evidence.
export const REAUTH_SIGNED_IN_TOAST = "Signed in";

// The /approvals card's title for the kind: a request that mints nothing must
// not be titled "Mint a scoped credential", and the card carries no
// blast-radius claim, and no claim that the run is paused, which a PENDING
// row does not prove (#146 defect 3): it names the need.
export const REAUTH_TITLE = "AWS sign-in needed for this run";
