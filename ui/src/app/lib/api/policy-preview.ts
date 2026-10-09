/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { asJson, HttpError, wfetch } from "./core";
import { runWireBody, type RunWireInput } from "./runs";
import type { PolicyPreviewResult } from "../types/policy-preview";

/** Read the effective policy using exactly the input create and preflight send. */
export async function previewRunPolicy(input: RunWireInput, signal?: AbortSignal): Promise<PolicyPreviewResult> {
  const res = await wfetch("/runs/policy-preview", {
    method: "POST", body: JSON.stringify(runWireBody(input)), signal,
  });
  try {
    return await asJson<PolicyPreviewResult>(res);
  } catch (e) {
    // Only this read retries on Retry-After, so the header is read here and not in the eager transport.
    if (e instanceof HttpError) e.retryAfter = res.headers.get("Retry-After") ?? undefined;
    throw e;
  }
}
