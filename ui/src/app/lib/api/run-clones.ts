/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { asJson, errText, HttpError, wfetch } from "./core";

// internal/api/run_clone_status.go runRepoClone: one repository's clone outcome as the run's launcher reported it.
export interface RunRepoClone {
  repo?: string;
  dest: string;
  outcome: string; // ok | present | skipped | failed | not_reported
  cause?: string;
}

// internal/api/run_clone_status.go runClonesResponse. state is reported, not_reported, pending or none.
export interface RunClones {
  run_id: string;
  state: string;
  repos: RunRepoClone[];
}

// GET /api/v1/runs/{id}/clones: a hint from the sandbox's own report, never a verdict.
export async function getRunClones(runId: string): Promise<RunClones> {
  const res = await wfetch(`/runs/${encodeURIComponent(runId)}/clones`, { method: "GET" });
  if (!res.ok) throw new HttpError(res.status, await errText(res));
  return asJson<RunClones>(res);
}
