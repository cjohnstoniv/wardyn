/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The four-panel New Run flow's own words (#1922), byte for byte from the
// approved M-R prototype. Imported directly by the lazy New Run screen and never
// re-exported through copy.ts: that barrel is in the eager entry chunk.

export const NEW_RUN_FLOW = {
  /** The nav's accessible name, and the page's existing title. */
  NAV: "New run",
  RUN: "Run",
  WORKSPACE: "Workspace",
  ACCESS: "Access",
  POLICY: "Policy",
  RUN_DETAILS: "Run details",
  RUN_DETAILS_NOTE:
    "For people, not the agent. A title and description help you find and understand this run later. They are never sent into the sandbox.",
  BACK: "Back",
  CONTINUE: (panel: string) => `Continue to ${panel}`,
  ISSUE_COUNT: (n: number) => (n === 1 ? "1 issue" : `${n} issues`),
  TITLE_REQUIRED: "Give this run a title.",
  HOLD_LABEL: "Hold in Wardyn — tool calls wait for approval, by tool rule",
  TOOL_RULES_LINK: "Tool rules",
  /** The rail's heading and, below `lg`, the summary toggle's name. */
  RAIL_TITLE: "What this run can do",
} as const;
