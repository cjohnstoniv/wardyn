/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Governance › Changes: a proposer's note (reason and ticket), the "Only security can decide" filter, a
// proposer's own request, and an elevation request's rows. Character for character as the approved boards draw
// them. The server's refusal sentences show as the server sends them and are not copied here.
export const CHANGE_NOTE = {
  FILTER_LABEL: "Who can decide",
  FILTER_ALL: "All changes",
  FILTER_SECURITY: "Only security can decide",
  FILTERED_EMPTY_TITLE: "Nothing is waiting for security alone",
  FILTERED_EMPTY_BODY: "Changes a second admin or a unit admin can decide aren't shown while this filter is on.",
  FILTERED_EMPTY_ACTION: "Show all changes",

  WHY_TITLE: "Why this change",
  /** "Ticket <value>": the label and the value, which the drawer sets in mono. */
  TICKET: (value: string) => ({ label: "Ticket", value }),
  ADD: "Add a note",
  EDIT: "Edit note",
  NO_NOTE_HINT: "Tell whoever decides it why, and which ticket it belongs to.",

  WHY_LABEL: "Why",
  WHY_PLACEHOLDER: "What this change is for",
  WHY_HINT: "Up to 500 characters. Shown to whoever decides it and kept in the audit trail.",
  TICKET_LABEL: "Ticket",
  TICKET_PLACEHOLDER: "A ticket number or link",
  TICKET_HINT: "Kept as text. Up to 128 characters.",
  CANCEL: "Cancel",
  SAVE: "Save note",
  REMOVE: "Remove note",
  SAVED: "Note saved",
  REFUSED_TITLE: "Couldn't save the note",

  OWN_SECURITY_TITLE: "Only security can decide this change",
  OWN_SECURITY_BODY: "You proposed it, so you can follow it, add a note, or reject it to withdraw it.",

  WITHDRAW_REQUEST: "Withdraw request",
  OWN_REQUEST: "You asked for this, so someone else must approve it. You can withdraw it.",

  /** An elevation row's second line: "4 hours from approval", then " · SUP-1" when there is a ticket. */
  ELEVATION_LINE: (length: string, ticket?: string) => `${length} from approval${ticket ? ` · ${ticket}` : ""}`,
  ELEVATION_TARGET: (person: string, unit: string) => `${person} → ${unit}`,
  ELEV_PERSON: "Person",
  ELEV_UNIT: "Unit",
  ELEV_LENGTH: "Length",
  ELEV_LENGTH_VALUE: (length: string) => `${length}, counted from when it is approved`,
} as const;
