/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "View as member" (0.7.4, field-report P2) — the console half: the account-menu
// way in, and the persistent banner that is the way out.
//
// Both live here rather than in app-shell.tsx because the shell is at the
// file-size gate, and because they are ONE control: a mode you can enter and
// cannot see yourself in is the trap this feature exists to avoid. The banner is
// unconditional while the mode is on — no dismiss, no collapse — for the same
// reason the unreachable-control-plane banner is.
//
// The server does the clamping (internal/auth/oidc's contextWithPrincipal);
// nothing here gates anything. Every operator-context default is untouched.

import * as React from "react";
import { Eye, EyeOff } from "lucide-react";
import { DropdownMenuItem } from "../ui/dropdown-menu";
import { health as api } from "../../lib/api/health";

// DRAFT (M2 canon pending)
export const MEMBER_MODE = {
  // The account-menu entry. "View as" rather than "become"/"impersonate":
  // nothing about the caller's identity changes, only what the server lets
  // them reach.
  MENU: "View as member",
  // The banner. Present tense and "paused", because the role is coming back the
  // moment they exit — it was not taken away.
  //
  // "your usual role", never "your admin role" (W6-5): the control is offered
  // to BOTH admin tiers (see `eligible` below, and OPERATIONS.md's "Both admin
  // tiers get the control"), so the banner naming one of them tells a
  // security_admin about a role they do not hold — on the one surface that is
  // unconditional and on every screen. This is the same reason EXIT names the
  // mode rather than a tier to go back to, two lines down.
  BANNER: "Viewing as member — your usual role is paused for this session",
  // The SECOND posture (0.7.5, field report finding 3). A separate menu item
  // rather than a toggle inside the mode: the two are different questions — "what
  // does a member see" and "what does a member who has not signed in see" — and
  // the second is the one a per_user deployment actually cannot show otherwise.
  MENU_NEW: "View as a new member (not signed in)",
  // Names the AWS state first, because that is the whole difference and it is
  // what the admin came to look at; the paused role rides second, as in BANNER.
  // U-11: "signing in is refused until you exit" was in the variant TOOLTIP
  // only, and a tooltip is a hover — no keyboard, no touch. It is the one limit
  // an admin can act on from inside the preview (Getting Started offers two
  // sign-in buttons here, and the launch 409s deterministically,
  // harnesscred_launch.go), so it rides the visible sentence.
  BANNER_NEW:
    "Viewing as a new member — not signed in to AWS; signing in is refused until you exit; your usual role is paused for this session",
  EXIT: "Exit member mode",
  // The three CEILINGS, verbatim from the design, on the banner as a title
  // tooltip. They are here and not only in OPERATIONS.md because the one
  // mistake this mode invites is reading it as proof that a member is refused:
  // it shows you what a member SEES. A real second identity is the proof.
  CEILINGS:
    // "and secrets" (U-12): OPERATIONS.md's ceiling 1 names all three, and the
    // code scopes them identically — secretOwnerFromRequest hands a clamped
    // admin their OWN namespace, exactly as run and workspace ownership survive
    // the clamp. Dropping the third made the tooltip narrower than the ceiling.
    "Member mode clamps your ROLE only. Runs, workspaces and secrets you created stay yours, " +
    "and governance ceilings still resolve against your real group membership. " +
    "Credentials you already hold — your SSH key, any API token — keep their admin stamp until refreshed at your next sign-in. " +
    // Ceiling 4 (0.7.5): the one the field report found by being misled by it.
    // It is in the PLAIN tooltip and not only in OPERATIONS.md because the
    // mistake it prevents is made while the banner is on screen.
    //
    // U-6: and it STOPS there. It used to point at 'View as a new member', an
    // item that is not rendered at all on a shared/legacy deployment or against
    // a 0.7.4 daemon (memberPreviewAvailable false), and that disappears from
    // the menu while ANY member mode is on — which is precisely when this
    // tooltip is on screen. Naming a control the reader cannot find is worse
    // than naming none: the ceiling itself is the true half.
    "Model access and ownership still resolve to you. " +
    "During a rolling upgrade an older replica ignores the flag and answers as admin. " +
    "It shows you what a member sees — sign in as a real member to prove what a member is refused.",
  // The variant tooltip swaps ceiling 4 for what the preview actually does. It
  // says HIDDEN, NOT REMOVED on purpose: the admin's captured session is still
  // in the store, untouched, and it comes back on exit — and it says sign-in is
  // refused because that is the one thing the preview cannot rehearse, so an
  // admin must not conclude from it that a first sign-in works.
  CEILINGS_NEW:
    "Member mode clamps your ROLE only. Runs, workspaces and secrets you created stay yours, " +
    "and governance ceilings still resolve against your real group membership. " +
    "Credentials you already hold — your SSH key, any API token — keep their admin stamp until refreshed at your next sign-in. " +
    "Your own AWS sign-in is hidden, not removed: model access reads as not signed in, runs that need it are refused, and signing in is refused until you exit. " +
    "During a rolling upgrade an older replica ignores the flag and answers as admin. " +
    "It shows you what a member sees — sign in as a real member to prove what a member is refused.",
  // Shown in place of a reload when the toggle itself failed. The console must
  // not reload on a failed POST: the admin would land back where they started
  // with nothing to read.
  FAILED: "Could not change member mode — try again.",
} as const;

/**
 * MemberModeMenuItem — the account-menu way IN.
 *
 * Offered to EITHER admin tier (`operator` is super-admin-only, so a
 * `security_admin` needs the second predicate) and only over SSO: the server
 * refuses the admin-token / local-mode / no-IdP lane with a 400, there being no
 * per-person role to pause there, so offering the control on those lanes would
 * be offering a refusal.
 *
 * Both tiers clamp to `member` — the server's clamp knows only "down to member"
 * — and both are restored to their own STAMPED tier on exit, which is why the
 * exit copy names the mode rather than a tier to go back to.
 *
 * It disappears once the mode is on, because both predicates are then false —
 * which is correct and not a gap: the way out is the banner, on every screen.
 */
export function MemberModeMenuItem({
  meta,
  onEntered = () => window.location.assign("/"),
}: {
  /** The three /me fields the predicate reads. Taken as ONE object so the whole
   *  rule lives in this module and app-shell.tsx (at the file-size gate) spends
   *  one line on the control — its ShellMeta satisfies this structurally. */
  meta: {
    operator: boolean;
    securityOperator: boolean;
    method: string;
    /** 0.7.5 — the server's answer to "does the preview hide anything here?".
     *  Under a `shared` roster row it hides nothing, so the second entry is not
     *  rendered at all: its banner would assert a state this deployment then
     *  contradicts. Optional so a caller that predates the field (and a
     *  pre-0.7.5 daemon, whose /me omits it) simply does not offer it. */
    memberPreviewAvailable?: boolean;
  };
  /** Injected in tests; the default reloads at the root because the session
   *  cookie changed and every screen's cached data was fetched as an admin. */
  onEntered?: () => void;
}) {
  // One `failed` per item, keyed by the posture that failed: against a 0.7.4
  // replica mid-upgrade the plain item still works and the new one 400s, and a
  // shared flag would paint that failure onto the item that did not fail.
  const [failed, setFailed] = React.useState<"" | "plain" | "new">("");
  const eligible = meta.operator || meta.securityOperator;
  if (!eligible || meta.method !== "sso") return null;
  const enter = (e: Event, noCredential: boolean) => {
    // The menu would close and unmount this item mid-request otherwise, taking
    // the failure message with it.
    e.preventDefault();
    setFailed("");
    api
      .setMemberMode(true, noCredential)
      .then(onEntered, () => setFailed(noCredential ? "new" : "plain"));
  };
  return (
    <>
      <DropdownMenuItem onSelect={(e) => enter(e, false)}>
        <Eye className="size-4" />{" "}
        {failed === "plain" ? MEMBER_MODE.FAILED : MEMBER_MODE.MENU}
      </DropdownMenuItem>
      {meta.memberPreviewAvailable && (
        <DropdownMenuItem onSelect={(e) => enter(e, true)}>
          <Eye className="size-4" />{" "}
          {failed === "new" ? MEMBER_MODE.FAILED : MEMBER_MODE.MENU_NEW}
        </DropdownMenuItem>
      )}
    </>
  );
}

/**
 * MemberModeBanner — the persistent state-of-the-console banner and the way OUT.
 * Renders nothing when the mode is off, so the shell needs no conditional of
 * its own.
 */
export function MemberModeBanner({
  active,
  noCredential = false,
  onExited = () => window.location.reload(),
}: {
  active: boolean;
  /** The 0.7.5 posture: it only swaps the sentence and the tooltip. Defaulted,
   *  so a caller that has not been taught about it renders the plain banner —
   *  which is what a pre-0.7.5 daemon answers anyway. */
  noCredential?: boolean;
  /** Injected in tests; the default reloads for the same reason entering does. */
  onExited?: () => void;
}) {
  const [failed, setFailed] = React.useState(false);
  if (!active) return null;
  return (
    <div
      role="status"
      className="relative z-50 flex shrink-0 items-center gap-2 border-b border-border bg-warning-subtle px-4 py-2 text-sm text-warning"
    >
      <EyeOff className="size-4 shrink-0" />
      <span title={noCredential ? MEMBER_MODE.CEILINGS_NEW : MEMBER_MODE.CEILINGS}>
        {noCredential ? MEMBER_MODE.BANNER_NEW : MEMBER_MODE.BANNER}
      </span>
      <button
        type="button"
        onClick={() => {
          setFailed(false);
          api.setMemberMode(false).then(onExited, () => setFailed(true));
        }}
        className="font-medium underline underline-offset-2"
      >
        {MEMBER_MODE.EXIT}
      </button>
      {failed && <span>{MEMBER_MODE.FAILED}</span>}
    </div>
  );
}
