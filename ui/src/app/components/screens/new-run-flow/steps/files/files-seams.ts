/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What the Files step reads that the TS twins do not carry yet (DS3-S2 and
// DS3-S3), typed as the wire contract draws them, plus the one read of them.
// Every field is optional or closed: absent means the server did not say, and
// the step then shows less, never a guess. When a twin lands, its type
// replaces the placeholder here and nothing else changes.

import * as React from "react";
import { runs } from "../../../../../lib/api/runs";
import type { PreflightResult } from "../../../../../lib/types/preflight";
import type { LimitSource, RunnerPoolChoice, RunnerPoolRunType } from "../../../../../lib/types/runner-pools";
import type { Workspace } from "../../../../../lib/types/workspaces";
import type { CreateRunWire } from "../../draft/intent-to-wire";
import { PREFLIGHT_DEBOUNCE_MS } from "../../draft/use-intent-checks";

/** Seam: DS3-S3. The weakest word across the pool's eligible targets. */
export type MainDriveEnforcement = "filesystem" | "eviction" | "none" | "unknown";

/** Seam: DS3-S3 (pkg/client.MainDriveFacts): what this pool's runners do with the main drive's size. */
export interface MainDriveFacts {
  enforcement: MainDriveEnforcement;
  basis: "substrate" | "runner_asserted";
  coverage?: "all" | "scratch_paths";
  keep: { supported: boolean; grace_sec?: number };
  drive_mounts: boolean;
}

/** Seam: DS3-S2 (pkg/client.MainDriveApplied): preflight's `main_drive`. */
export interface MainDriveApplied {
  requested_mib?: number;
  applied_mib: number;
  basis: "request" | "policy" | "default" | "none";
  default_source?: LimitSource;
  clamped_by?: "pool" | "governance" | "deployment" | "policy";
  soft: boolean;
  facts?: MainDriveFacts;
}

export type FilesPoolChoice = RunnerPoolChoice & { main_drive?: MainDriveFacts };

export type FilesPreflight = Omit<PreflightResult, "runner_pools"> & {
  runner_pools?: FilesPoolChoice[];
  main_drive?: MainDriveApplied;
};

/** One repository source of an onboarded workspace: a row of the Repositories list. */
export interface RepoRow {
  key: string;
  workspaceId: string;
  repo: string;
  name: string;
  /** The source's own ref. */
  ref?: string;
  /** Where the clone lands by default (the workspace read's `clone_target`). */
  path?: string;
  /** Every repository of this workspace, for an entry that carries no repos list. */
  siblings: string[];
}

const repoName = (source: string): string => source.replace(/[/\\]+$/, "").replace(/\.git$/, "").split(/[/:\\]/).pop() || source;

/** The repository sources of the workspaces the person may launch, one row each. */
export function repoRows(workspaces: readonly Workspace[]): RepoRow[] {
  return workspaces
    .filter((w) => w.available_to_you !== false)
    .flatMap((w) => {
      const repos = (w.sources ?? []).filter((s) => s.type === "repo" && s.source);
      const siblings = repos.map((s) => s.source as string);
      return repos.map((s) => ({
        key: `${w.id}/${s.source}`, workspaceId: w.id, repo: s.source as string, name: repoName(s.source as string),
        ref: s.ref, path: s.clone_target, siblings,
      }));
    });
}

/** What the step reads of the pool the run lands in, for the run's type. */
export interface MainDriveView {
  poolName?: string;
  /** Whole MiB; undefined when the pool and server set no cap. */
  capMib?: number;
  capSource?: LimitSource;
  defaultMib?: number;
  facts?: MainDriveFacts;
  /** The Interactive lifetime's maximum in seconds, undefined when unlimited or unknown. */
  maxLifetimeSec?: number;
}

export function readMainDrive(pre: FilesPreflight | null, runType: RunnerPoolRunType | undefined): MainDriveView {
  const pool = pre?.runner_pools?.find((p) => p.id === pre.runner_pool?.id);
  const limits = pool?.limits?.find((l) => l.run_type === runType);
  const disk = limits?.disk_mib;
  const cap = disk && !disk.cap.unlimited ? disk.cap.value : undefined;
  const fallback = disk && !disk.default.unlimited && disk.default.value > 0 ? disk.default.value : undefined;
  const span = runType === "interactive" ? limits?.lifetime_span_sec.cap : undefined;
  return {
    poolName: pre?.runner_pool?.name,
    capMib: cap,
    capSource: cap === undefined ? undefined : disk?.cap_source,
    defaultMib: fallback ?? cap,
    facts: pre?.main_drive?.facts ?? pool?.main_drive,
    maxLifetimeSec: span && !span.unlimited ? span.value : undefined,
  };
}

/** What the draft's own preflight answered. `current` is false while a newer draft is still being asked. */
export function useFilesPreflight(body: CreateRunWire | null): { result: FilesPreflight | null; current: boolean } {
  const key = body === null ? "" : JSON.stringify(body);
  const [read, setRead] = React.useState<{ key: string; result: FilesPreflight } | null>(null);
  React.useEffect(() => {
    if (body === null) return;
    const controller = new AbortController();
    const timer = setTimeout(() => {
      // A refusal leaves the last answer in place: the step reads facts, it does not gate on them.
      runs.preflightRunWire(body, controller.signal).then(
        (result) => { if (!controller.signal.aborted) setRead({ key, result: result as FilesPreflight }); },
        () => {},
      );
    }, PREFLIGHT_DEBOUNCE_MS);
    return () => { clearTimeout(timer); controller.abort(); };
    // `key` stands for `body`: a new object with the same content is no new question.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key]);
  return { result: read?.result ?? null, current: read !== null && read.key === key };
}
