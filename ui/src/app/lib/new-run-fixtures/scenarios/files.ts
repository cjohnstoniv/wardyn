/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { RunAttachment } from "../../types/new-run-contract";
import type { EffectiveLimit, EffectiveRunLimits, LimitSource } from "../../types/runner-pools";
import type { Workspace } from "../../types/workspaces";
import type { FilesPreflight, MainDriveFacts } from "../../../components/screens/new-run-flow/steps/files/files-seams";
import { everyScenario, type ByScenario } from "./index";

const STAMP = "2026-10-01T09:00:00Z";
const CAP = { value: 0, unlimited: true };
const amount = (mib: number, source?: LimitSource): EffectiveLimit => ({ default: { value: Math.min(mib, 4096) }, cap: { value: mib }, cap_source: source });
const unlimited: EffectiveLimit = { default: CAP, cap: CAP };

/** One run type's effective limits with the main drive capped at `diskMib` (undefined: no cap). */
export const limitsFor = (runType: EffectiveRunLimits["run_type"], diskMib?: number, source: LimitSource = "pool"): EffectiveRunLimits => ({
  run_type: runType,
  cpu_millis: unlimited, memory_mib: unlimited,
  disk_mib: diskMib === undefined ? unlimited : amount(diskMib, source),
  lifetime_sec: unlimited, lifetime_span_sec: { default: { value: 3600 }, cap: { value: 28800 }, cap_source: "pool" }, lifetime_lease_sec: unlimited,
  no_end_allowed: false, concurrency: [],
});

const KEEP = { supported: true, grace_sec: 86400 };
/** One fixture per enforcement word, so each chip and tooltip is exercised. */
export const MAIN_DRIVE_FACTS: Record<"filesystem" | "eviction" | "none" | "unknown" | "runner", MainDriveFacts> = {
  filesystem: { enforcement: "filesystem", basis: "substrate", coverage: "all", keep: KEEP, drive_mounts: true },
  eviction: { enforcement: "eviction", basis: "substrate", coverage: "scratch_paths", keep: { supported: false }, drive_mounts: true },
  none: { enforcement: "none", basis: "substrate", keep: KEEP, drive_mounts: true },
  unknown: { enforcement: "unknown", basis: "substrate", keep: { supported: false }, drive_mounts: false },
  runner: { enforcement: "filesystem", basis: "runner_asserted", coverage: "all", keep: { supported: false }, drive_mounts: false },
};

const POOL = { id: "c1111111-1111-4111-8111-111111111111", name: "Shared pool", scope: "organization" as const };

export const REPO_ATTACHMENT: RunAttachment = {
  kind: "repository", workspace_id: "ws-payments", repo: "github.com/acme/payments", name: "payments",
  target: "/home/agent/work/payments", read_only: false, starts_here: false,
};

/** POST /runs/preflight's pool and main-drive facts, per scenario. Attachments are the test's own. */
export const filesPreflight = (facts: MainDriveFacts, diskMib = 8192, runType: EffectiveRunLimits["run_type"] = "interactive"): FilesPreflight => ({
  setup_items: [],
  enforced_confinement_class: "CC2",
  runner_pool: { ...POOL, revision: 1, selection: "organisation_default" },
  runner_pools: [{ ...POOL, availability: "available", barriers: ["CC2"], limits: [limitsFor(runType, diskMib)], main_drive: facts }],
  attachments: [],
});

export const SCENARIO_FILES_PREFLIGHT: ByScenario<FilesPreflight> = everyScenario(filesPreflight(MAIN_DRIVE_FACTS.filesystem), {
  governed: filesPreflight(MAIN_DRIVE_FACTS.eviction),
  offline: filesPreflight(MAIN_DRIVE_FACTS.unknown),
  noKvm: filesPreflight(MAIN_DRIVE_FACTS.none),
  admin: filesPreflight(MAIN_DRIVE_FACTS.runner),
});

const workspace = (id: string, name: string, source: string, ref: string, target: string): Workspace => ({
  id, name, kind: "repo", source, ref, status: "scanned", available_to_you: true, created_at: STAMP, updated_at: STAMP,
  sources: [{ type: "repo", source, ref, clone_target: target }],
});

/** GET /workspaces, per scenario: two onboarded repositories. */
export const SCENARIO_FILES_WORKSPACES: ByScenario<Workspace[]> = everyScenario([
  workspace("ws-payments", "payments", "github.com/acme/payments", "main", "/home/agent/work/payments"),
  workspace("ws-docs", "docs", "github.com/acme/docs", "main", "/home/agent/work/docs"),
]);
