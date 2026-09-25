/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #460 — the unsaved-guard / sidebar-Settings canon (docs/design/
// unsaved-guard-canon.md carries the full table + the Q460 decisions,
// including the save-conflict CONFLICT.* strings, which live directly in
// workspace-providers-copy.ts's PROVIDERS/PROVIDERS_DRAFT — their existing
// home — rather than being re-exported here).
//
// This module stays on the console's EAGER entry path (use-unsaved-guard.tsx
// -> app-shell.tsx): it imports ONLY wardyn/copy/shell.ts, never
// workspace-providers-copy.ts, whose ~400 lines of provider-specific copy
// belong to the route-split /providers screen (bundle-split.test.ts's entry
// budget — model-access.ts's own file-header note is the same lesson learned
// once already).
import { UNSAVED_GUARD } from "../components/wardyn/copy/shell";

export const UNSAVED = {
  DIRTY_CHIP: UNSAVED_GUARD.DIRTY_CHIP,
  TITLE: UNSAVED_GUARD.TITLE,
  BODY: UNSAVED_GUARD.BODY,
  STAY: UNSAVED_GUARD.STAY,
  DISCARD: UNSAVED_GUARD.LEAVE,
} as const;

// Single-sourced (AGENTS.md §1 — console copy in one place): the sidebar
// entry (sidebar-settings-link.tsx) and the account menu's own entry
// (top-bar.tsx) render the same word.
export const NAV = {
  SETTINGS: "Settings",
} as const;
