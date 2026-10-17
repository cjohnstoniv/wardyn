/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run canon strings for step 2, Run, byte for byte from docs/design/new-run-canon.md.
// Lazy-only: imported by the New Run flow and never re-exported through copy.ts, whose barrel is in the eager
// entry chunk. new-run-copy.test.ts keeps this file and the doc equal in both directions.

export const NR_RUN = {
  TITLE_AGENT: "Choose how and where it runs",
  TITLE_ENV: "Choose where the environment runs",
  TITLE_APP: "Choose where the automation runs",
  /** pending W-05: the recommended text until the owner answers. */
  LEDE_ORG: "Your organisation's defaults are selected.",
  /** pending W-05: the recommended text until the owner answers. */
  LEDE_PERSONAL: "Your defaults are selected.",
  /** pending W-05: the recommended text until the owner answers. */
  LEDE_NONE: "Choose where it runs.",
  MODE_LABEL: "How should it run?",
  MODE_INTERACTIVE: "Interactive",
  /** pending W-02: the recommended text until the owner answers. */
  MODE_INTERACTIVE_BODY: "You work in the sandbox; one harness can start with it.",
  MODE_AUTONOMOUS: "Autonomous",
  MODE_AUTONOMOUS_BODY: "An agent runs on its own, with no SSH or terminal.",
  TIMING_LABEL: "When should it run?",
  TIMING_NOW: "Run Now",
  TIMING_NOW_BODY: "Start as soon as you launch the run.",
  TIMING_LATER: "Schedule For Later",
  TIMING_LATER_BODY: "Choose a future date and time for this run to start.",
  APP_LABEL: "What should run?",
  APP_AUTOMATION: "Automation / script",
  APP_AUTOMATION_BODY: "A command or job that runs on its own.",
  APP_AUTOMATION_META: "Available now",
  APP_SERVER: "Application / server",
  APP_SERVER_BODY: "App servers and MCP servers.",
  COMMAND_LABEL: "Command",
  /** Lead wording 2026-10-17 (FR-0099): the Review blocker for a command run with no command. */
  COMMAND_REQUIRED: "A command run needs a command to run.",
  COMMAND_HELP: "Run verbatim in the sandbox. The run ends when the command exits.",
  WHERE_LABEL: "Where should it run?",
  POOL_LABEL: "Pool",
  POOL_HELP:
    "A pool is required. Pools you can use stay visible, with the reason when one can't take this run.",
  POOL_OPTION_REASON: (pool: string | number, reason: string | number) => `${pool} · ${reason}`,
  REASON_NO_ONLINE: "No online runners",
  /** pending W-55: the recommended text until the owner answers. */
  FIT_STARTS_NOW: "Starts now",
  /** pending W-55: the recommended text until the owner answers. */
  FIT_WAITS: "Waits for a slot",
  /** pending W-03: the recommended text until the owner answers. */
  REASON_SWITCHED_OFF: "Switched off",
  /** pending W-03: the recommended text until the owner answers. */
  REASON_NO_RUNNERS: "No runners",
  /** pending W-03: the recommended text until the owner answers. */
  REASON_BACKGROUND_ONLY: "Background only",
  /** pending W-03: the recommended text until the owner answers. */
  REASON_INTERACTIVE_ONLY: "Interactive only",
  /** pending W-03: the recommended text until the owner answers. */
  REASON_NOT_ALLOWED_PROFILE: "Not allowed by your profile",
  /** pending W-03: the recommended text until the owner answers. */
  REASON_RUNNERS_OFF: "Runners are switched off",
  /** pending W-03: the recommended text until the owner answers. */
  PLACEMENT_DENIED: (profile: string | number, kind: string | number) =>
    `${profile} doesn't allow runs on ${kind} pools.`,
  CAPACITY_LABEL: "Total pool capacity",
  CAPACITY_ARIA: "Total pool capacity used",
  USED_OF_MAX: (used: string | number, max: string | number) => `${used} of ${max} running`,
  RUNNING_ONLY: (used: string | number) => `${used} running`,
  MAX_RUNS: (max: string | number) => `${max} runs maximum in this pool`,
  /** pending W-50: the recommended text until the owner answers. */
  POOL_LIMIT_FROM_RUNNERS: "Set by its runners",
  ALLOWANCE_LABEL: "Your allowance",
  ALLOWANCE_ARIA: "Your allowance used",
  ALLOWANCE_USED: (active: string | number, max: string | number) =>
    `${active} of ${max} running (all pools)`,
  ALLOWANCE_INCLUDES: (active: string | number) => `Includes your ${active} current runs in any pool`,
  ALLOWANCE_INCLUDES_ONE: "Includes your 1 current run in any pool",
  NO_PERSONAL_LIMIT: "No personal limit",
  WAITING_LABEL: "Waiting",
  IN_QUEUE: (n: string | number) => `${n} in queue`,
  NO_RUNS_WAITING: "No runs waiting",
  JOIN_QUEUE: "New runs join the queue",
  START_WHEN_LAUNCHED: "A run can start when launched",
  RUN_TYPES_LABEL: "Run types",
  RUN_TYPES_BOTH: "Interactive and background",
  RUN_TYPES_BACKGROUND: "Background only",
  RUN_TYPES_INTERACTIVE: "Interactive only",
  SET_BY_POOL: "Set by this pool",
  NO_RESTRICTION: "No restriction",
  SET_BY: (source: string | number) => `Set by ${source}`,
  /** pending W-03: the recommended text until the owner answers. */
  SET_BY_RUNNERS: "Set by your runners",
  /** pending W-03: the recommended text until the owner answers. */
  SET_BY_ORG: "Set by your organisation",
  LIMITS_LABEL: "Per-run limits",
  LIMITS_NOTE: (profile: string | number) => `Strictest of this pool, ${profile} and this server applies`,
  LIMITS_NOTE_UNGOVERNED: "Strictest of this pool and this server",
  /** pending W-03: the recommended text until the owner answers. */
  LIMITS_NOTE_ORG: "Strictest of this pool, your organisation and this server applies",
  LIMIT_CPU: "CPU",
  LIMIT_MEMORY: "Memory",
  LIMIT_LIFETIME: "Lifetime",
  LIMIT_MAIN_DRIVE: "Main drive",
  LIMIT_MAXIMUM: "Maximum per run",
  LIMIT_EXTRA_DRIVES: "Extra allowed drives can still be attached",
  /** pending W-03: the recommended text until the owner answers. */
  LIMIT_NO_MAXIMUM: "No maximum",
  BARRIER_LABEL: "Barrier",
  COMPARE_BARRIERS: "Compare barriers",
  TAGLINE_FENCE: "Weakest",
  TAGLINE_WALL: "Stronger",
  TAGLINE_VAULT: "Strongest",
  STRENGTH_ARIA: (n: string | number) => `Barrier strength ${n} of 3`,
  ABOUT_ARIA: (barrier: string | number) => `About ${barrier}`,
  PROTECTS_HEAD: "Protects against",
  NOT_PROTECTS_HEAD: "Doesn't protect against",
  HOW_HEAD: "How it works",
  FENCE_PROTECTS:
    "The agent seeing or touching the rest of your machine — it gets its own files, processes, and network.",
  FENCE_NOT_PROTECTS:
    "A kernel exploit or container escape — it shares your machine's kernel (the “holes” in the fence).",
  FENCE_HOW:
    "Shared-kernel container (runc) hardened with user namespaces, seccomp, AppArmor, and capability drop.",
  WALL_PROTECTS:
    "Everything Fence does, plus most kernel exploits — a software kernel (gVisor) handles the agent's system calls so it never touches your real kernel (the holes are closed).",
  WALL_NOT_PROTECTS: "A flaw in the sandbox software itself (rare); it's still not a fully separate machine.",
  WALL_HOW:
    "gVisor userspace kernel intercepts syscalls — the default; needs a native Docker engine with its runsc runtime registered (a one-time setup step).",
  VAULT_PROTECTS:
    "Everything Wall does, plus a full break-in staying trapped — its own hardware-walled VM with its own kernel; even total takeover inside stays inside.",
  VAULT_NOT_PROTECTS:
    "A flaw in the virtualization layer itself — a very rare hypervisor- or CPU-level escape.",
  VAULT_HOW: "Kata microVM — hardware-virtualized, with its own guest kernel (requires /dev/kvm).",
  ROW_ISOLATION: "Isolated from your files, processes, and network",
  ROW_KERNEL: "A kernel exploit can't reach your host kernel",
  ROW_BREAK_IN: "A full break-in stays sealed inside",
  ROW_WHERE: "Where it runs",
  WHERE_FENCE: "Any host",
  WHERE_WALL: "Native Docker engine + setup",
  WHERE_VAULT: "Needs KVM hardware",
  STATUS_YES: "Yes",
  STATUS_NO: "No",
  STATUS_CAVEAT: "With a caveat",
  CAVEAT_EXCEPT: (text: string | number) => `Except: ${text}`,
  BADGE_ORG_DEFAULT: "Organisation default",
  BADGE_DEFAULT: "Default",
  BADGE_EXPERIMENTAL: "Experimental",
  BARRIER_REQUIRED: (barrier: string | number) => `Your organisation requires at least ${barrier}.`,
  BARRIER_NO_KVM: "This pool's runners can't run Vault (no KVM).",
  BARRIER_POOL_DISALLOWS: (pool: string | number, barrier: string | number) =>
    `${pool} doesn't allow ${barrier}.`,
  BARRIER_NONE_OFFERS: (barrier: string | number) => `No runner in this pool offers ${barrier}.`,
  BARRIER_KNOWN_WHEN_ONLINE: "Known when one of your runners is online.",
  BARRIER_POLICY_REQUIRES: (barrier: string | number) => `This run's policy requires at least ${barrier}.`,
  BARRIER_ACCESS_VAULT: "This run's access requires Vault.",
  /** pending W-03: the recommended text until the owner answers. */
  BARRIER_UNKNOWN: "Not known right now. Check again in a moment.",
  /** pending W-07: the recommended text until the owner answers. */
  BARRIER_NO_TERMINAL: "This pool's runners can't open a terminal at Vault.",
  POLICY_NOTE:
    "Whatever the barrier, every run still gets Wardyn's egress filtering, short-lived brokered credentials, human approvals, and full audit — those are set by policy, not the barrier. The barrier only decides how strongly the sandbox is walled off from your machine.",
} as const;
