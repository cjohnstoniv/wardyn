/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// CLI sign-in (wardyn login), approved in mock round M-ATT r8 (boards MATT2-* and MATT3-*), character for
// character: the /cli/sign-in page and Account's "Signed-in CLIs" card. cli-sign-in.test.ts freezes them.
// A function key renders the board's own template with the server's value in it.
//
// The server's own refusals are shown as the server says them, except the two that mintGuards gives on the
// approve page, which are worded for a sign-in (DESIGN-ATTACH 11.14 #10); approveRefusal maps them by reason.
import { dayOf, lifetimeWords } from "./service-tokens";

/** "19 Oct 2026, 18:42 UTC". */
export function stampOf(iso: string): string {
  const d = new Date(iso);
  const p = (n: number) => String(n).padStart(2, "0");
  return `${dayOf(iso)}, ${p(d.getUTCHours())}:${p(d.getUTCMinutes())} UTC`;
}

/** "1-hour" for 3600: a lifetime said as an adjective ("1-hour access"). */
const adjective = (seconds: number) => lifetimeWords(seconds).replace(/^(\d+) (\w+?)s?$/, "$1-$2");

export const SIGN_IN = {
  EYEBROW: "Wardyn CLI sign-in",
  TITLE: "Sign in the Wardyn CLI?",
  LEAD: (who: string) => `A terminal asked to sign in as ${who}. Approve only if you just ran wardyn login yourself.`,
  COMPUTER: "Computer",
  FROM: "Requested from",
  REQUESTED: "Requested",
  EXPIRES_IN: (seconds: number) => `Expires in ${lifetimeWords(seconds)}`,
  CODE: "Code from your terminal",
  CODE_NOTE: "Approve only if your terminal printed this same code.",
  ALLOWS: "What this allows",
  GRANT: (access: number, ttl: number, idle: number) =>
    `A CLI session for you: ${adjective(access)} access that refreshes. It ends after ${lifetimeWords(ttl)}, or after ${lifetimeWords(idle)} without use. It acts with your user role only.`,
  APPROVE: "Approve",
  DENY: "Deny",

  MISMATCH_TITLE: "This request came from a different address than this browser.",
  MISMATCH_BODY: "If you did not start this sign-in yourself, choose Deny.",
  NOT_THIS_BROWSER: "Not the address of this browser",

  ADMIN_TITLE: (access: number) => `Admin access for ${lifetimeWords(access)}`,
  ADMIN_LEAD: (access: number) =>
    `A terminal asked to act as you with your admin role. It lasts ${lifetimeWords(access)} and does not refresh. Approve only if you just ran the command yourself.`,
  ADMIN_GRANT: (access: number) =>
    `Admin access for ${lifetimeWords(access)}. It does not refresh, and it ends sooner if you sign this computer out. The CLI session itself stays a user session.`,

  PASTE_TITLE: "Approved. Paste this code into your terminal.",
  PASTE_COPY: "Copy code",
  PASTE_WARN: "Paste this only into a terminal you started yourself.",
  PASTE_ONCE: (seconds: number) => `Shown once. It works once, for ${lifetimeWords(seconds)}. After that, run wardyn login again.`,

  DENIED_TITLE: "Denied",
  DENIED_BODY: "Nothing was signed in. You can close this tab.",
  EXPIRED_TITLE: "This sign-in request expired",
  EXPIRED_BODY: "Requests last 10 minutes. Run wardyn login again in your terminal.",
  USED_TITLE: "This sign-in request was already used",
  USED_BODY: "Run wardyn login again if you need a new one.",

  REFUSED_USER_VIEW: "Exit the user view to sign in a CLI.",
  REFUSED_NOT_ALLOWED: "CLI sign-in isn't available to you. Ask your admin.",

  // Not on the boards (listed in the PR body).
  BROWSER_ADDRESS: (address: string) => `This browser's address is ${address}.`,
  NO_CALLBACK: "The server did not return the CLI's address. Run wardyn login again in your terminal.",
} as const;

/** The approve page's sentence for a mintGuards refusal, by reason; "" for any other, which shows the server's own text. */
export function approveRefusal(reason: string): string {
  if (reason === "api_token_member_mode_mint") return SIGN_IN.REFUSED_USER_VIEW;
  if (reason === "capability_feature") return SIGN_IN.REFUSED_NOT_ALLOWED;
  return "";
}

export const SIGNED_IN_CLIS = {
  TITLE: "Signed-in CLIs",
  SUMMARY: (n: number) => (n === 0 ? "No computers signed in" : n === 1 ? "1 computer" : `${n} computers`),
  LEDE: "Computers where you signed in with the Wardyn CLI. Signing one out ends its session now.",
  COL_COMPUTER: "Computer",
  COL_STORE: "Store",
  COL_SIGNED_IN: "Signed in",
  COL_ENDS: "Ends",
  COL_IDLE_END: "Idle end",
  CHIP: "CLI sign-in",
  STORE_KEYCHAIN: "Keychain",
  STORE_FILE: "File, 0600",
  FILE_NOTE: "No keychain here: the sign-in is kept in a file only you can read, so it ends after 7 days.",
  ADMIN_CHIP: (minutes: number) => `Admin access, ends in ${minutes} min`,
  SIGN_OUT: "Sign out",
  SIGN_OUT_LABEL: (machine: string) => `Sign out ${machine}`,
  DIALOG_TITLE: (machine: string) => `Sign out ${machine}?`,
  DIALOG_BODY: (machine: string) => `The Wardyn CLI on ${machine} stops working now. Run wardyn login there to sign in again.`,
  TOAST: (machine: string) => `Signed out ${machine}.`,
  EMPTY: "No computers are signed in with the Wardyn CLI.",
  RUN_LEAD: "To sign in another computer, run",
  RUN_COMMAND: (url: string) => `wardyn login ${url}`,

  // Not on the boards (listed in the PR body).
  LOADING: "Loading signed-in CLIs",
  TABLE_LABEL: "Signed-in CLIs",
} as const;
