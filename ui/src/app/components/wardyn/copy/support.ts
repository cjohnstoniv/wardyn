/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Support sessions: a super admin asks to help inside someone's running run, the run's owner accepts with
// doors and minutes. Every string is the approved board's (STRINGS.md), byte for byte; the consent paragraphs
// are the design's section 7 copy, changed only by the lead. The server's refusals are shown as it says them.

import type { SupportDoor, SupportGrantView } from "../../../lib/api/support";

export const SUPPORT_DOORS: readonly SupportDoor[] = ["terminal", "commands", "files", "forward", "web_apps", "desktop"];

/** D-1 to D-6 and DH-1 to DH-6: each door's name and its plain words. */
export const SUPPORT_DOOR: Record<SupportDoor, { name: string; help: string }> = {
  terminal: { name: "Terminal", help: "Use the run's terminal from a browser, or open a shell over SSH." },
  commands: { name: "Commands", help: "Run commands in the sandbox." },
  files: { name: "Files", help: "Copy files in and out." },
  forward: { name: "Forward", help: "Reach the ports the run listens on." },
  web_apps: { name: "Web apps", help: "Open the run's web apps." },
  desktop: { name: "Desktop", help: "See and use the run's desktop." },
};

export const SUPPORT_MINUTES = [15, 30, 60] as const;
export const SUPPORT_REASON_MAX = 500;

export const SUPPORT_REQUEST = {
  /** Q-01: the button and the dialog's title. */
  TITLE: "Request help",
  LEDE: "Ask this run's owner to let you in. They choose which doors to open and for how long.",
  DOORS: "Doors",
  REASON: "Reason",
  REASON_HINT: "The owner reads this before they decide. 1 to 500 characters.",
  COUNT: (n: number) => `${n} / 500`,
  TOO_LONG: "Use 500 characters or fewer.",
  PATH_MODE: "Web apps and Desktop are not offered for this run.",
  SEND: "Send request",
  CANCEL: "Cancel",
} as const;

/** C-01 with the run's recording answer filled in, and C-02, as section 7 words them. */
export const SUPPORT_CONSENT = {
  TITLE: (helper: string) => `${helper} wants to help in this run`,
  NOTHING_OPENS: "Nothing opens until you accept.",
  REQUESTED: (time: string) => `Requested ${time}. A request nobody answers lapses after 15 minutes.`,
  LAPSED: "This request lapsed. Ask the helper to send a new one.",
  THEIR_REASON: "Their reason",
  DOORS: "Doors to open",
  DOORS_HINT: "Tick the doors you allow. Every door stays closed unless you tick it.",
  HOW_LONG: "How long",
  MINUTES: (n: number) => `${n} minutes`,
  NO_EXTENSION: "Help ends then. It cannot be extended: more time needs a new request.",
  WHERE: "Where the sandbox runs",
  OWN_COMPUTER: "The helper reaches the sandbox only through Wardyn's server.",
  RECORDING: (recording: string) =>
    `Terminal: ${recording} on this run. Commands, Files, Forward and Web apps are never recorded. The audit trail keeps each command's text (a very long one is cut, with its length and checksum) and exit code. For files it keeps direction and size, and in the Files panel a checksum, never a file's name or content; a sync also keeps the folder it started in, and a download sent for review keeps the file's name on its approval request. For a forward it keeps the port and a byte count. What the helper does inside the run appears in the audit trail as this run's activity. No credential is paused: the helper can use every connection this run has, as you. The helper cannot approve or deny any of this run's holds raised after their request, even once help ends.`,
  REACH:
    "Terminal and Commands each run anything the agent can, so either one reads every file and reaches the run's network. Forward reaches every port the run listens on, including its web apps and desktop, unrecorded.",
  DESKTOP: (recording: string) => `Desktop: ${recording} on this run.`,
  ACTIVITY: "While they are in the run they count as activity: a paused run wakes, and the idle clock restarts.",
  ACCEPT: "Accept",
  DECLINE: "Decline",
  CLOSE: "Close",
} as const;

export const SUPPORT_RECORDED = { yes: "recorded", no: "not recorded" } as const;

export const SUPPORT_BANNER = {
  HELPER: "Helper",
  DOORS: "Doors",
  UNTIL: "Until",
  LAST_SEEN: "Last seen",
  END_NOW: "End now",
  /** N-01 and N-02. */
  ACTIVE_OTHERS: (helper: string) => `${helper} is helping in this run`,
  ACTIVE_HELPER: "You are helping in this run",
  /** N-03 and N-04. */
  ASKED: (helper: string) => `${helper} asked to help in this run`,
  REVIEW: "Review request",
  /** N-05 and N-06. */
  WAITING: (owner: string) => `Waiting for ${owner} to answer.`,
  CANCEL_REQUEST: "Cancel request",
  /** N-13, T-04 and T-05: the helper's terminal after the owner joined. */
  OWNER_JOINED: "The run's owner joined",
  OWNER_DRIVING: "The owner is driving this terminal now. Reconnect to keep watching.",
  RECONNECT: "Reconnect",
  CLOSE: "Close",
} as const;

export type SupportViewer = "owner" | "helper" | "security";

/**
 * The line for an ended grant, by end reason and who is looking (N-07 to N-22; the board's viewer table). null is
 * no line: the owner who declined sees none, and a reason this console does not know says nothing.
 */
export function supportEndLine(g: Pick<SupportGrantView, "end_reason" | "doors" | "minutes" | "expires_at">, viewer: SupportViewer): string | null {
  const accepted = !!g.expires_at || !!g.minutes || (g.doors?.length ?? 0) > 0;
  const owner = viewer === "owner";
  const helper = viewer === "helper";
  switch (g.end_reason) {
    case "expired":
      return "Help ended: the time ran out.";
    case "revoked":
      return owner ? "Help ended: you ended it." : "Help ended: the owner ended it.";
    case "run_ended":
      return "Help ended: the run stopped or was lost.";
    case "declined":
      return owner ? null : "The owner declined this request.";
    case "lapsed":
      return "The request lapsed. Nobody answered in 15 minutes.";
    case "ended_by_helper":
      if (accepted) return helper ? "Help ended: you ended it." : "Help ended: the helper ended it.";
      return helper ? "You cancelled this request." : "The helper cancelled this request.";
    case "ended_by_security":
      return "Help ended: a security admin ended it.";
    case "helper_signed_out":
      return helper ? "Help ended: your sign-in ended." : "Help ended: the helper's sign-in ended.";
    case "owner_revoked":
      return owner ? "Help ended: your account was revoked or deactivated." : "Help ended: the owner's account was revoked or deactivated.";
    case "support_off":
      return "Help ended: support sessions were turned off.";
  }
  return null;
}
