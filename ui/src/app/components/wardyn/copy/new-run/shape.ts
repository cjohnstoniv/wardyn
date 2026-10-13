/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run canon strings for step 1, Shape, byte for byte from docs/design/new-run-canon.md.
// Lazy-only: imported by the New Run flow and never re-exported through copy.ts, whose barrel is in the eager
// entry chunk. new-run-copy.test.ts keeps this file and the doc equal in both directions.

export const NR_SHAPE = {
  TITLE: "What are you running?",
  LEDE: "Pick the closest shape. You can adjust every detail before launch.",
  START_FROM: "Start from",
  SOURCE_SCRATCH: "From scratch",
  SOURCE_TEMPLATE: "Saved template",
  SOURCE_AGAIN: "Run again",
  TEMPLATE_LABEL: "Template",
  TEMPLATE_OPTION: (name: string | number, scope: string | number) => `${name} · ${scope}`,
  PREVIOUS_LABEL: "Previous run",
  AGAIN_OPTION: (title: string | number, when: string | number) => `${title} · ${when}`,
  /** pending W-01: the recommended text until the owner answers. */
  AGAIN_NOTE:
    "Settings are rechecked now. Short-lived credentials are issued fresh; standing approvals still apply.",
  /** pending W-23: the recommended text until the owner answers. */
  EMPTY_TEMPLATES: "No saved templates yet.",
  /** pending W-23: the recommended text until the owner answers. */
  EMPTY_AGAIN: "No previous runs to start again.",
  AGENT_TITLE: "Agent task",
  AGENT_BODY: "A one-time coding task, driven by you or running autonomously.",
  AGENT_SUB: "Interactive · Autonomous · Repeat / schedule coming soon",
  ENV_TITLE: "Environment sandbox",
  ENV_BODY: "A reusable workspace for development, analysis or investigation.",
  /** pending A18: the recommended text until the owner answers. */
  ENV_SUB: "Interactive · Your drive is attached when you have one",
  APP_TITLE: "Application sandbox",
  APP_BODY: "Run a sandboxed command, automation or script on its own.",
  APP_SUB: "Automation / script · Application / server coming soon",
} as const;
