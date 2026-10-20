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
import type { MeDrive } from "../../../../../lib/api/me-drives";
import { DENIED } from "../../../../../lib/permissions-copy";
import type { PreflightResult } from "../../../../../lib/types/preflight";
import type { LimitSource, RunnerPoolChoice, RunnerPoolRunType } from "../../../../../lib/types/runner-pools";
import type { Workspace } from "../../../../../lib/types/workspaces";
import { PROVIDERS } from "../../../../../lib/workspace-providers-copy";
import type { CreateRunWire } from "../../draft/intent-to-wire";
import { useDraftIdentity, useIntentChecks } from "../../draft/use-intent-checks";

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

/**
 * Seam: #1991. A drive a run kept its work folder in names that run (its title). The catalogue row does not
 * carry it yet; absent, the drive is listed as any other.
 */
export type KeptMeDrive = MeDrive & { kept_from_run?: string };

export type FilesPoolChoice = RunnerPoolChoice & { main_drive?: MainDriveFacts };

export type FilesPreflight = Omit<PreflightResult, "runner_pools"> & {
  runner_pools?: FilesPoolChoice[];
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
  /** The sentence for why this row cannot be included; absent when it can. */
  unavailable?: string;
}

const repoName = (source: string): string => source.replace(/[/\\]+$/, "").replace(/\.git$/, "").split(/[/:\\]/).pop() || source;

/**
 * The repository sources of every workspace the person can see, one row each. A row the server says
 * they cannot launch stays listed with its reason (visibility is not capability); only `admitted ===
 * false` means refused, an absent flag says nothing.
 */
export function repoRows(workspaces: readonly Workspace[]): RepoRow[] {
  return workspaces.flatMap((w) => {
    const repos = (w.sources ?? []).filter((s) => s.type === "repo" && s.source);
    const siblings = repos.map((s) => s.source as string);
    return repos.map((s) => ({
      key: `${w.id}/${s.source}`, workspaceId: w.id, repo: s.source as string, name: repoName(s.source as string),
      ref: s.ref, path: s.clone_target, siblings,
      unavailable: w.available_to_you === false ? DENIED.WORKSPACE_NOT_AVAILABLE : s.admitted === false ? PROVIDERS.CARD_NOT_ADMITTED : undefined,
    }));
  });
}

/** What the step reads of the pool the run lands in, for the run's type. */
export interface MainDriveView {
  poolName?: string;
  /** Whole MiB; undefined when the pool and server set no cap. */
  capMib?: number;
  capSource?: LimitSource | "policy";
  defaultMib?: number;
  facts?: MainDriveFacts;
  /** The Interactive lifetime's maximum in seconds, undefined when unlimited or unknown. */
  maxLifetimeSec?: number;
}

export function readMainDrive(pre: FilesPreflight | null, runType: RunnerPoolRunType | undefined): MainDriveView {
  const pool = pre?.runner_pools?.find((p) => p.id === pre.runner_pool?.id);
  // The run's effective limits (pool, governance and deployment) are preflight's own `limits`; the pool
  // choices carry none. An answer for another run type says nothing about this one.
  const limits = pre?.limits?.run_type === runType ? pre?.limits : undefined;
  const disk = limits?.disk_mib;
  const cap = disk && !disk.cap.unlimited ? disk.cap.value : undefined;
  const fallback = disk && !disk.default.unlimited && disk.default.value > 0 ? disk.default.value : undefined;
  const span = runType === "interactive" ? limits?.lifetime_span_sec.cap : undefined;
  // A size the run's policy wrote is the size the run gets (a policy size is never soft), so it prefills the
  // field and caps it: the pool's default and cap say nothing more about this run.
  const applied = limits ? pre?.main_drive : undefined;
  const policyMib = applied && (applied.basis === "policy" || applied.clamped_by === "policy") && applied.applied_mib > 0 ? applied.applied_mib : undefined;
  const policyCaps = policyMib !== undefined && (cap === undefined || policyMib < cap);
  return {
    poolName: pre?.runner_pool?.name,
    capMib: policyCaps ? policyMib : cap,
    capSource: policyCaps ? "policy" : cap === undefined ? undefined : disk?.cap_source,
    defaultMib: policyMib ?? fallback ?? cap,
    facts: pool?.main_drive,
    maxLifetimeSec: span && !span.unlimited ? span.value : undefined,
  };
}

/**
 * The run's preflight through the shared checks hook, which fences every answer by identity and auth
 * generation and sends nothing while `body` is null. The last answer of the current identity is kept
 * across edits, so what the step shows never falls back to a local guess while a newer one is asked.
 */
export function useFilesPreflight(body: CreateRunWire | null): FilesPreflight | null {
  const identity = useDraftIdentity();
  const checks = useIntentChecks({
    body, selectionKey: "", identity, externalRevision: "", sourceRefreshPending: false,
    autoCheck: { local: true, backendArm: false, modelArm: false }, doorOpen: false, adoDoorOpen: false,
  });
  const identityKey = `${identity.principal}|${identity.authGeneration}`;
  const last = React.useRef<{ key: string; result: FilesPreflight } | null>(null);
  if (checks.preflightResult) last.current = { key: identityKey, result: checks.preflightResult as FilesPreflight };
  return last.current?.key === identityKey ? last.current.result : null;
}
