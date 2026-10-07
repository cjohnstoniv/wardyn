/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { asJson, wfetch } from "./core";
import { runWireBody, type RunWireInput } from "./runs";
import type { PolicyPreviewResult } from "../types/policy-preview";

/** Read the effective policy using exactly the input create and preflight send. */
export async function previewRunPolicy(input: RunWireInput, signal?: AbortSignal): Promise<PolicyPreviewResult> {
  return asJson<PolicyPreviewResult>(await wfetch("/policies/preview", {
    method: "POST", body: JSON.stringify(runWireBody(input)), signal,
  }));
}
