/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Canonical, HONEST autonomy metadata — the single source of truth for
// AutonomyChip (primitives.tsx) and the profile editor's rubric selects
// (profile-rubric.tsx), mirroring cc-meta.ts's own role for ConfinementChip.
//
// L0-L3 is the internal WIRE value (mirrors internal/types/governance.go's
// AutonomyLevel). Users see the friendly display label — Attended / Gated /
// Unattended / Unrestricted — the internal code shows only in the tooltip,
// exactly the way ConfinementChip keeps CC1/CC2/CC3 out of the visible label
// (docs/design/CONSOLE-RULES.md).
import type { AutonomyLevel } from "../../lib/api/governance";

export interface AutonomyMeta {
  /** Friendly display label — the ONLY thing shown on screen. */
  label: string;
  /** One sentence: what this level permits. Used as the chip's tooltip body. */
  tagline: string;
}

// Weakest (most supervised) -> strongest (least) — mirrors
// internal/types/governance.go's AutonomyLevel doc comment and
// lib/api/governance.ts's AUTONOMY_LEVEL_ORDER, which carries the same ladder
// for code that may not import this components/ module.
export const AUTONOMY_META: Record<AutonomyLevel, AutonomyMeta> = {
  L0: {
    label: "Attended",
    tagline: "Interactive runs only, and no tool call is pre-approved.",
  },
  L1: {
    label: "Gated",
    tagline: "Can run unattended, but every tool call waits for your confirmation.",
  },
  L2: {
    label: "Unattended",
    tagline: "Can run and approve its own tool calls.",
  },
  L3: {
    label: "Unrestricted",
    tagline: "No autonomy cap, including running a command directly.",
  },
};

// AutonomyChip's label when no rubric applies to a run at all (primitives.tsx).
export const AUTONOMY_NO_CAP_LABEL = "No limit";
