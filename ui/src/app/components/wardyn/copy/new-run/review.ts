/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run canon strings for Review, byte for byte from docs/design/new-run-canon.md.
// Lazy-only: imported by the New Run flow and never re-exported through copy.ts, whose barrel is in the eager
// entry chunk. new-run-copy.test.ts keeps this file and the doc equal in both directions.

export const NR_REVIEW = {
  EYEBROW: "Ready for a final check",
  TITLE: "Review your run",
  LEDE: "This is what Wardyn will create. Every applied limit names its source.",
  VIEW_ARIA: "Review view",
  VIEW_PRETTY: "Pretty",
  VIEW_YAML: "YAML",
  VIEW_JSON: "JSON",
  /** pending W-04: the recommended text until the owner answers. */
  NARROWED_BY: (source: string | number) => `Your request was narrowed by ${source}.`,
  NARROWED_APPLIES: (applied: string | number) => `${applied} will apply.`,
  /** pending W-04: the recommended text until the owner answers. */
  NARROWED_TAIL_POOL: (pool: string | number) => `Not allowed in ${pool}.`,
  NARROWED_TAIL_CEILING: "Not allowed by your ceiling.",
  /** pending W-04: the recommended text until the owner answers. */
  NARROWED_TAIL_SERVER: "Not allowed on this server.",
  /** pending W-04: the recommended text until the owner answers. */
  NARROWED_TAIL_RUNNERS: "More than your runners can give.",
  BLOCKERS_ONE: "1 blocker before launch",
  BLOCKERS_MANY: (n: string | number) => `${n} blockers before launch`,
  BLOCKER_STALE: "The setup changed. Check the server's current result again.",
  /** pending W-29: the recommended text until the owner answers. */
  BLOCKER_UNSUPPORTED: (path: string | number) => `This document field isn't supported here: ${path}.`,
  CHECK_AGAIN: "Check again",
  CHECKED_ANNOUNCE: "Server result checked. Launch is ready.",
  SETUP_TITLE: "Run setup",
  SETUP_AGENT_AUTONOMOUS: "Autonomous agent task",
  SETUP_AGENT_INTERACTIVE: "Interactive agent task",
  SETUP_APPLICATION: "Background automation",
  SETUP_ENVIRONMENT: "Interactive environment",
  BADGE_ATTENTION: "Needs attention",
  BADGE_COMPLETE: "Complete",
  /** The chip when only a stale server result is in the way and no group is to blame (#3043). */
  BADGE_CHECK: "Check needed",
  GROUP_RUN: "Run and timing",
  GROUP_WHERE: "Where it runs",
  GROUP_WORK: "What it works on",
  GROUP_TOOLS: "Tools and model",
  GROUP_ACCESS: "Access",
  GROUP_LIMITS: "Limits",
  VALUE_AGENT: "Agent task",
  VALUE_APPLICATION: "Application sandbox",
  VALUE_ENVIRONMENT: "Environment sandbox",
  TIMING_APPLICATION: "Background automation · Runs when launched",
  TIMING_INTERACTIVE: "Interactive · Starts when launched",
  TIMING_NOW: "Autonomous · Run now",
  WHERE_VALUE: (pool: string | number) => `${pool} pool`,
  /** The Where row when no pool is chosen: the pool-required blocker points at it (#3043). */
  WHERE_NONE: "No pool chosen yet",
  WHERE_DETAIL: (kind: string | number, used: string | number, max: string | number) =>
    `${kind} · ${used} of ${max} running`,
  /** pending W-03: the recommended text until the owner answers. */
  WHERE_ONLINE: (n: string | number) => `${n} of your runners online`,
  PROV_ADDED: "Added by you",
  PROV_SELECTED: "Selected by you",
  PROV_RUN_SETUP: "Run setup",
  PROV_ORG_DEFAULT: "Organisation default",
  PROV_MODEL_PROVIDER: "Selected model provider",
  PROV_SOURCE_POLICY: "Source policy",
  WORK_NONE: "No repositories or drives",
  WORK_ITEM_ONE: "1 item attached",
  WORK_ITEMS: (n: string | number) => `${n} items attached`,
  MAIN_DRIVE: (size: string | number) => `Main drive · ${size}`,
  NO_HARNESSES: "No harnesses",
  TOOLS_COMMAND_ONE: "Command · 1 optional harness",
  TOOLS_COMMAND: (n: string | number) => `Command · ${n} optional harnesses`,
  TOOLS_NONE: "Optional · none added",
  TOOLS_STARTS: (name: string | number, provider: string | number) =>
    `${name} starts with the run · ${provider}`,
  TOOLS_NO_HARNESS: "No harness",
  TOOLS_AUTO_ONE: "1 harness starts automatically",
  TOOLS_AUTO: (n: string | number, total: string | number) =>
    `${n} of ${total} ${Number(total) === 1 ? "harness starts" : "harnesses start"} automatically`,
  HARNESS_STARTS: "Starts with run",
  HARNESS_AFTER: "Available after start",
  HARNESS_AUTO: "Starts automatically",
  HARNESS_NOT_AUTO: "Not auto-started",
  HARNESS_NO_PROMPT: "No starting prompt",
  HARNESS_HAS_PROMPT: "Has prompt",
  HARNESS_STARTS_LONG: "Starts with the run",
  HARNESS_AFTER_LONG: "Available after start",
  HARNESS_INCLUDED: "Included, not auto-started",
  ACCESS_NOTE: "Hosts are layered onto the organisation default, then narrowed by your ceiling.",
  LIMITS_DETAIL: "Ends at its limit or when the work finishes",
  LIMITS_VALUE: (cpu: string | number, memory: string | number, lifetime: string | number) =>
    `${cpu} CPU · ${memory} MiB · ${lifetime}`,
  /** pending W-27: the recommended text until the owner answers. */
  LIMIT_UP_TO: (cap: string | number, source: string | number) => `Up to ${cap} · Set by ${source}`,
  /** pending W-29: the recommended text until the owner answers. */
  DOC_ONLY_CHIP: "Run document",
  /** pending W-09: the recommended text until the owner answers. */
  SHARED_MODEL_ACCESS:
    "Each harness's model access is added by the proxy, and any process in this sandbox can use it.",
  CHANGE: "Change",
  CLOSE: "Close",
  DONE: "Done",
  EDIT_RUN: "Edit run and timing",
  EDIT_FILES: "Edit files",
  EDIT_HARNESSES: "Edit harnesses",
  /** pending W-26: the recommended text until the owner answers. */
  EDIT_TOOLS_ACCESS: "Edit tools and access",
  POOL_LABEL: "Pool",
  BARRIER_LABEL: "Barrier",
  COMMAND_LABEL: "Command",
  CPU_LABEL: "CPU",
  MEMORY_LABEL: "Memory MiB",
  POLICY_TITLE: "Effective policy",
  POLICY_BODY: "What the sandbox can actually do",
  ONLY_CHANGED: "Only what I changed",
  NOTHING_CHANGED: "You haven't changed anything for this run.",
  SHOW_EVERYTHING: "Show everything",
  POLICY_BARRIER: "Barrier",
  POLICY_NETWORK: "Network",
  POLICY_HOSTS: (n: string | number) => `${n} hosts allowed`,
  POLICY_HOST_ONE: "1 host allowed",
  POLICY_CREDENTIALS: "Credentials",
  POLICY_NO_CREDENTIALS: "No credentials requested",
  POLICY_HIDDEN: "Selected model provider · Hidden",
  POLICY_FILES: "Files and code",
  POLICY_TOOLS: "Tools and pushes",
  POLICY_HOLD: "Hold for approval",
  POLICY_ACCEPTS_EDITS: "For tools without a named rule, Claude Code accepts file edits without asking and asks before other tools. Named rules still apply.",
  POLICY_AUTOMATIC: "Run automatically",
  /** One harness's default when the included harnesses default differently. */
  POLICY_TOOLS_OF: (harness: string | number, effect: string | number) => `${harness}: ${effect}`,
  /** The harness's own walls (#2822), on Review's tools row and the run page's policy row. */
  INNER_WALLS: (sandbox: string | number, approvals: string | number) => `Harness walls: ${sandbox}, ${approvals}`,
  INNER_SANDBOX_OFF: "harness sandbox off",
  INNER_SANDBOX_OWN: "the harness's own sandbox",
  INNER_SANDBOX_MODE: (mode: string | number) => `harness sandbox ${mode}`,
  INNER_APPROVALS_HARNESS: "approvals in the harness",
  INNER_APPROVALS_WARDYN: "approvals in Wardyn",
  INNER_APPROVALS_NONE: "no approvals",
  RAW_TITLE: (format: string | number) => `${format} run document`,
  RAW_BODY: "Read-only resolved draft",
  COPY: "Copy",
  COPIED: "Copied",
  EDIT_FORMAT: (format: string | number) => `Edit ${format}`,
  SAVE_TEMPLATE: "Save as template",
  SAVE_POLICY: "Save as policy",
  /** Below sm, Review keeps Save as template and Save as policy behind this menu. */
  MORE: "More",
  MORE_ARIA: "More actions",
  LAUNCH_RUN: "Launch run",
  DROPPED_HOST_ONE: (host: string | number) => `This run can't reach ${host}: your organisation doesn't allow it. Ask your admin to allow it.`,
  DROPPED_HOSTS_MANY: (hosts: string | number) => `This run can't reach ${hosts}: your organisation doesn't allow them. Ask your admin to allow them.`,
} as const;
