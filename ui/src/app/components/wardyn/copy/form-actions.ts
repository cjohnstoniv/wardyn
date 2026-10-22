/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

export const FORM_ACTIONS = {
  SAVE: "Save",
  CANCEL: "Cancel",
  UNSAVED: (n: number) => `${n} unsaved ${n === 1 ? "change" : "changes"}`,
  JUMP_TO: "Jump to section",
} as const;
