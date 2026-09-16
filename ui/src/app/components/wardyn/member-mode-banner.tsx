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
  // The banner. Present tense and "paused", because the admin role is coming
  // back the moment they exit — it was not taken away.
  BANNER: "Viewing as member — your admin role is paused for this session",
  EXIT: "Exit member mode",
  // The three CEILINGS, verbatim from the design, on the banner as a title
  // tooltip. They are here and not only in OPERATIONS.md because the one
  // mistake this mode invites is reading it as proof that a member is refused:
  // it shows you what a member SEES. A real second identity is the proof.
  CEILINGS:
    "Member mode clamps your ROLE only. Runs and workspaces you created stay yours, " +
    "and governance ceilings still resolve against your real group membership. " +
    "Your SSH key keeps its admin stamp until it is refreshed at your next sign-in. " +
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
  meta: { operator: boolean; securityOperator: boolean; method: string };
  /** Injected in tests; the default reloads at the root because the session
   *  cookie changed and every screen's cached data was fetched as an admin. */
  onEntered?: () => void;
}) {
  const [failed, setFailed] = React.useState(false);
  const eligible = meta.operator || meta.securityOperator;
  if (!eligible || meta.method !== "sso") return null;
  return (
    <DropdownMenuItem
      onSelect={(e) => {
        // The menu would close and unmount this item mid-request otherwise,
        // taking the failure message with it.
        e.preventDefault();
        setFailed(false);
        api.setMemberMode(true).then(onEntered, () => setFailed(true));
      }}
    >
      <Eye className="size-4" />{" "}
      {failed ? MEMBER_MODE.FAILED : MEMBER_MODE.MENU}
    </DropdownMenuItem>
  );
}

/**
 * MemberModeBanner — the persistent state-of-the-console banner and the way OUT.
 * Renders nothing when the mode is off, so the shell needs no conditional of
 * its own.
 */
export function MemberModeBanner({
  active,
  onExited = () => window.location.reload(),
}: {
  active: boolean;
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
      <span title={MEMBER_MODE.CEILINGS}>{MEMBER_MODE.BANNER}</span>
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
