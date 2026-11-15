/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Notes for a supporting read that failed, where the page answers without it and says so instead of guessing.

/** Secrets: the token broker switch could not be read, so the page says nothing about where a Git token is held. */
export const SECRETS_BROKER_UNCHECKED = {
  NOTE: "Couldn't check how stored Git tokens are handled, so this page says nothing about it.",
  RETRY: "Try again",
  /** The header line while the switch is unknown: true with the broker on or off, and makes no claim about a Git token. */
  VALUE_DELIVERABLE: "A stored value can be delivered to a run inside its sandbox, where any code running there can read it.",
} as const;

/** The workspace model access picker: the model providers list could not be read. */
export const MODEL_PROVIDERS_UNAVAILABLE = {
  NOTE: "Couldn't load the model providers, so the list is not shown. None still works.",
  RETRY: "Try again",
} as const;

/** A run's Recording tab: a list of its recordings could not be read, or filled its page. */
export const RECORDING_INDEX_INCOMPLETE = {
  NOTE: "Couldn't list all of this run's recordings, so some may be missing here.",
  RETRY: "Try again",
  /** The retry's accessible name: the tab can show another "Try again" (cast, terminal data or replay) beside it. */
  RETRY_LABEL: "Try again: list this run's recordings",
} as const;
