/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { RunOutput } from "../types";
import { asJson, wfetch } from "./core";

export const runOutput = {
  // GET /api/v1/runs/{id}/output. A refusal throws an HttpError whose
  // `reason` names it (run_output_off, run_output_not_kept, ...).
  async get(runId: string): Promise<RunOutput> {
    const res = await wfetch(`/runs/${encodeURIComponent(runId)}/output`, { method: "GET" });
    return asJson<RunOutput>(res);
  },
};
