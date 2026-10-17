/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { RunSignIn } from "../types";
import { asJson, wfetch } from "./core";

export const runSignIn = {
  // GET /api/v1/runs/{id}/sign-in — the run's owner only. A waiting answer
  // whose link is not https is a failed read here, never a link on screen.
  async get(runId: string, signal?: AbortSignal): Promise<RunSignIn> {
    const res = await wfetch(`/runs/${encodeURIComponent(runId)}/sign-in`, { method: "GET", signal });
    const body = await asJson<RunSignIn>(res);
    if (body.state === "waiting") {
      let https = false;
      try {
        https = new URL(body.verification_url ?? "").protocol === "https:";
      } catch {
        https = false;
      }
      if (!https || !body.user_code) throw new Error("sign-in answer without an https verification link");
    }
    return body;
  },
};
