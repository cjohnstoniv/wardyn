/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Session continuity copy (#1995, mock M12). Frozen canon: docs/design/session-continuity-canon.md
// (session-continuity-copy.test.ts parses that table back and compares every row).
//
// Pure TS, no React and no fetch: the sign-in gate, the account menu and the terminal all read it,
// and the first two are on the eager entry path (bundle-split.test.ts).
//
// The fallback sentence for a session that ended with no reason, or one this build does not know, is
// SESSION_ENDED_REASON (lib/api/core.ts), kept as it was.
import { SESSION_ENDED_REASON } from "./api/core";
import { shortDate, shortTime } from "./format";

/** The 401 reasons (internal/api/auth_status.go) that name why a session ended, each with the sentence the gate shows. */
export const SESSION_ENDED = {
  EXPIRED: "Your session reached its time limit, so you were signed out. Sign in again to carry on.",
  // No cause named: SCIM group changes and suspensions also cut sessions.
  REVOKED: "Your session was ended on the server, so you were signed out. Sign in again to carry on.",
  ROLE_STALE: "Your sign-in is older than this deployment allows for your role. Sign in again to pick up your current access.",
  INVALID: "Wardyn couldn't verify your session, so you were signed out. Sign in again.",
  DEACTIVATED: "Your access to Wardyn has been turned off. Ask your Wardyn admin.",
  ELSEWHERE: "You signed out in another tab.",
  OWN: "You signed out of Wardyn.",
  OWN_SHARED:
    "On a shared computer, also sign out of your work account in this browser. Wardyn's sign-out does not end that sign-in.",
  RETURN: (run: string) => `After you sign in, you go back to Run ${run}.`,
} as const;

/** The account menu's two new lines. */
export const SESSION_MENU = {
  SIGNED_IN_UNTIL_TODAY: (time: string) => `Signed in until ${time} today`,
  // Not drawn: an expiry on a later day names its day.
  SIGNED_IN_UNTIL_DAY: (time: string, day: string) => `Signed in until ${time} on ${day}`,
  SIGN_OUT_HINT: "Ends your Wardyn session in this browser.",
} as const;

/** The run terminal across a sign-out. */
export const TERMINAL_SESSION = {
  RESUMED: (time: string) => `── Signed in again at ${time}. Reconnected; the output above is kept. ──`,
  SIGNED_OUT_NOTE:
    "The terminal keeps its scrollback while the dialog is open and reconnects once you are signed in. Keystrokes typed while signed out are not sent.",
} as const;

/** Why the sign-in gate is showing a notice. */
export type GateNotice =
  | { kind: "reason"; reason: string }
  | { kind: "fallback" }
  | { kind: "elsewhere" }
  | { kind: "own"; sso: boolean };

export type NoticeTone = "warning" | "danger" | "info" | "neutral";
export interface NoticeView {
  lines: string[];
  tone: NoticeTone;
  /** The Sign in button stays but quieter: signing in again will not help until access is back. */
  quietSignIn: boolean;
}

const BY_REASON: Record<string, string> = {
  expired_session: SESSION_ENDED.EXPIRED,
  revoked_session: SESSION_ENDED.REVOKED,
  role_stamp_stale: SESSION_ENDED.ROLE_STALE,
  invalid_session: SESSION_ENDED.INVALID,
};

export function noticeView(n: GateNotice): NoticeView {
  switch (n.kind) {
    case "elsewhere":
      return { lines: [SESSION_ENDED.ELSEWHERE], tone: "neutral", quietSignIn: false };
    case "own":
      return { lines: n.sso ? [SESSION_ENDED.OWN, SESSION_ENDED.OWN_SHARED] : [SESSION_ENDED.OWN], tone: "info", quietSignIn: false };
    case "reason":
      if (n.reason === "identity_deactivated") return { lines: [SESSION_ENDED.DEACTIVATED], tone: "danger", quietSignIn: true };
      return { lines: [BY_REASON[n.reason] ?? SESSION_ENDED_REASON], tone: "warning", quietSignIn: false };
    case "fallback":
      return { lines: [SESSION_ENDED_REASON], tone: "warning", quietSignIn: false };
  }
}

/** "Run 7f3c9a41" for a console path that is a run's page, else null: only that page is drawn. */
export function returnRunOf(path: string): string | null {
  const m = /^(?:\/admin)?\/runs\/([^/?#]+)/.exec(path);
  if (!m || m[1] === "new") return null;
  return m[1].replace(/^run_/, "").slice(0, 8);
}

/** The account menu's "Signed in until" line for a session ending at `expiry`. */
export function signedInUntil(expiry: Date, now: Date = new Date()): string {
  const time = shortTime(expiry.toISOString());
  if (expiry.toDateString() === now.toDateString()) return SESSION_MENU.SIGNED_IN_UNTIL_TODAY(time);
  return SESSION_MENU.SIGNED_IN_UNTIL_DAY(time, shortDate(expiry.toISOString()));
}
