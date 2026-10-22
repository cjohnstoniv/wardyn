/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The status pills on a collapsed Settings card, and the headings the cards sit under.
export const CARD_STATUS = {
  CANT_LAUNCH: "Can't launch",
  NOT_SET_UP: "Not set up",
  DEFAULT: "Default",
  NEEDS_ATTENTION: "Needs attention",
} as const;

export const SETTINGS_GROUPS = {
  HOST: "This host",
  CONNECTIONS: "Connections",
  PEOPLE: "People and access",
} as const;
