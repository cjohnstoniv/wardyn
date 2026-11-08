/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Every string of the elevation request: the dialog, the person's own list of requests and the decider's card.
// Screens render these and author none. Lengths in sentences are words ("4 hours"); the presets are "4 h".

const cap = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);

// A phrase made of one decider reads with "decides", more than one (or the admins of a unit) with "decide".
const ONE_DECIDER = /^(a security admin|the .+ group)$/;
// "Payments' limits", "Platform's limits".
const possessive = (name: string) => (/s$/i.test(name) ? `${name}'` : `${name}'s`);
const singular = (who: string) => ONE_DECIDER.test(who) && !who.includes(" or ") && !who.includes(", ");

export const ELEV = {
  ACTION: "Request elevation",
  TITLE: "New elevation request",
  LEAD: "Ask for time-limited access to a profile. The people who decide for it see your request, and the time starts when one of them approves.",

  UNIT_LABEL: "Unit",
  UNIT_HINT: "Only units that offer elevation are listed. A profile is a governance unit.",

  LENGTH_LABEL: "How long",
  PRESETS: ["1 h", "4 h", "8 h", "Custom"],
  CUSTOM_HINT: (max: string, unit: string) => `Up to ${max} in ${unit}. The time starts when someone approves.`,
  PRESET_OVER: (unit: string, max: string) => `${unit} allows at most ${max}.`,
  CUSTOM_COUNT_LABEL: "Length",
  CUSTOM_UNIT_LABEL: "Length unit",
  MINUTES: "minutes",
  HOURS: "hours",

  REASON_LABEL: "Why do you need it?",
  REASON_PLACEHOLDER: "Say what you need to do and why. The people who decide read this.",
  REASON_HINT: "Shown to the people who decide, and kept in the audit trail.",
  COUNT: (n: number) => `${n} of 500`,

  TICKET_LABEL: "Ticket (optional)",
  TICKET_REQUIRED_LABEL: "Ticket",
  TICKET_PLACEHOLDER: "For example SUP-1234",
  TICKET_HINT: "A reference the deciders can look up.",

  CANCEL: "Cancel",
  SEND: "Request elevation",
  SENDING: "Sending…",
  DONE: "Done",
  CLOSE: "Close",
  RETRY: "Retry",

  SENT_TITLE: "Request sent",
  SENT_BODY: (who: string, time: string) =>
    `It is waiting for ${who}. You'll see the answer under Approvals. If nobody decides it by ${time}, it expires.`,
  REFUSED_TITLE: "Couldn't send the request",
  ALREADY_WAITING: (unit: string) => `You already have a request waiting for ${unit}. Withdraw it first, or wait for an answer.`,
  NOT_OFFERED: (unit: string) => `${unit} doesn't offer elevation.`,
  NOT_OFFERED_ANY: "No unit offers you elevation.",
  UNITS_FAILED_TITLE: "Couldn't load your units",
  UNITS_FAILED_BODY: "Wardyn isn't answering. Try again.",
  LOADING_UNITS: "Loading units…",

  LIST_TITLE: "Your requests",
  LIST_LEAD: "Time-limited access to a profile. A profile is a governance unit.",
  LIST_EMPTY: "You have no requests.",
  STATE_WAITING: "Waiting for approval",
  STATE_APPROVED: "Approved",
  STATE_DENIED: "Denied",
  STATE_EXPIRED: "Expired",
  STATE_ENDED: "Ended",
  LINE_WAITING: (when: string, who: string) => `Requested ${when}. ${cap(who)} ${singular(who) ? "decides" : "decide"} it.`,
  LINE_APPROVED: (at: string, left: string, unit: string) => `Ends ${at} (in ${left}). Pick ${unit} when you start a run.`,
  LINE_DENIED: (by: string, reason: string, when: string) => `${by}: “${reason}” · ${when}`,
  LINE_EXPIRED: (when: string) => `Nobody decided it in time · ${when}`,
  LINE_ENDED: (when: string) => `Ended ${when}`,
  WITHDRAW: "Withdraw",
  TOAST_WITHDRAWN: "Request withdrawn",
  WITHDRAW_FAILED: "Couldn't withdraw the request",

  F_KIND: "Elevation",
  F_TITLE: (p: string, u: string, l: string) => `Elevate ${p} to ${u} for ${l}`,
  F_REASON: "Their reason:",
  F_WHAT_LABEL: "What you're approving:",
  F_WHAT: (p: string, u: string, l: string) => `${p} becomes a member of ${u} for ${l}, starting when you approve.`,
  F_BLAST_LABEL: "Blast radius:",
  F_BLAST: (u: string) =>
    `They can start runs under ${possessive(u)} limits until then, and each run they start ends when the elevation ends. Ending the elevation early doesn't stop a run that has already started.`,
} as const;
