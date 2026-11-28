/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run page's Preview URLs tile and the console route that opens one (/previews/{run}/{port}), character for
// character as the approved frames draw them. Every refusal sentence under a form or an Open is the server's, shown
// as it came; the two sentences below that are not (ENDED, and the route's revoked line) are the console's own.

export const RUN_PREVIEWS = {
  TITLE: "Preview URLs",
  INTRO: "Open a port this run is listening on in its own tab, on a different address than this console. You choose who else can open it.",
  ADMIN_INTRO: "You can see and revoke the previews on this run. Only the run's owner creates them.",
  EMPTY: "No previews yet.",
  PORT_LABEL: "Port",
  TTL_LABEL: "Lasts",
  VISIBILITY_LABEL: "Who can open it",
  PRIVATE: "Private",
  PRIVATE_BODY: "Only you can open it.",
  ORG: "Organisation",
  ORG_BODY: "Anyone signed in to this console can open it.",
  PUBLIC: "Public",
  PUBLIC_BODY: "Anyone with the link can open it, with no sign-in.",
  CAPPED: (ceiling: string) => `Your administrator limits previews to ${ceiling}.`,
  CREATE: "Create preview",
  CHANGE_HINT: "A port with a live preview is changed by creating it again.",
  ROW_TITLE: (port: number) => `Port ${port}`,
  EXPIRES: (rest: string) => `Expires ${rest}`,
  OPEN: "Open",
  REVOKE: "Revoke",
  REVOKED_CHIP: "Revoked",
  REVOKED_NOTE: "Revoked just now. It leaves this list when the page reloads.",
  NEW: "New preview",
  COPY_LINK: "Copy link",
  LINK_COPIED: "Link copied",
  ENDED: "This preview has ended. Create it again to open it.",
  OPEN_FAILED: (port: number) => `Couldn't open port ${port}`,
  POPUP_BLOCKED: "Your browser blocked the new tab. Allow pop-ups for this site, then press Open again.",
  FOOT: "Viewing a preview doesn't keep this run awake or wake it when it's paused. Wardyn records when a preview is opened and, for Organisation previews, the address of each request, never what is sent or shown.",
} as const;

/** The visibility labels the console shows, by wire value. */
export const PREVIEW_VISIBILITY_LABEL = {
  private: RUN_PREVIEWS.PRIVATE,
  org: RUN_PREVIEWS.ORG,
  public: RUN_PREVIEWS.PUBLIC,
} as const;

/** The lengths a preview may last: the label and the wire's ttl_seconds. The first of the five steps is the shortest the server takes. */
export const PREVIEW_TTL_OPTIONS = [
  { value: "3600", label: "1 hour" },
  { value: "21600", label: "6 hours" },
  { value: "86400", label: "24 hours" },
  { value: "259200", label: "3 days" },
  { value: "604800", label: "7 days" },
] as const;
export const PREVIEW_TTL_DEFAULT = "86400";

/** The console route /previews/{run}/{port}: the link an Organisation row copies. */
export const PREVIEW_ROUTE = {
  OPENING: "Opening preview…",
} as const;

/**
 * The sentence the route shows for a preview that has ended, was revoked or does not admit the person. It is the
 * gateway's own (previewRevokedMsg in internal/api/uigateway_preview.go), so the console says what a visitor of the
 * preview's address would be told.
 */
export const PREVIEW_REVOKED = "This preview has ended or was revoked. Ask the person who shared it for a new link.";
