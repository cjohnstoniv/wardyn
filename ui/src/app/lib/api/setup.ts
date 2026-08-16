/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Host/barrier readiness — GET /api/v1/setup/status, with a permissive
// fallback so a daemon that can't answer never reads as a genuinely bare host.
import type { SetupStatus } from "../types";
import { HttpError, wfetch } from "./core";

// GET /api/v1/setup/status permissive fallback — an endpoint-less build (older
// backend, the endpoint mid-rollout, or a daemon that simply didn't answer)
// must never read as "no barrier installed" (empty confinement_classes).
// `unreachable` marks the payload as synthetic/untrustworthy: consumers (the
// top bar's barrier chip, Runs' no-barrier blocker) keep their last-known
// state instead of repainting from these made-up fields.
const READY_FALLBACK: SetupStatus = {
  unreachable: true,
  ready: true,
  checks: [],
  auth: { mode: "local", local_loopback: true },
  runner: { driver: "none", confinement_classes: [] },
  providers: [],
  secrets: { present: [], github_app: false },
  age_key: { durable: false },
  has_runs: false,
  platform: { os: "", wsl: false },
};

export const setup = {
  // GET /api/v1/setup/status — first-run readiness snapshot (see types.ts for
  // the frozen contract). 404 (endpoint not built yet) or any other non-ok
  // response, or a thrown network error, degrades to READY_FALLBACK so the
  // Getting-started wizard never auto-opens against a build that can't answer
  // it. A 401 is NOT swallowed here — it's already thrown by wfetch (which
  // routes it through onUnauthorized first), so it propagates as usual.
  async getSetupStatus(): Promise<SetupStatus> {
    try {
      const res = await wfetch("/setup/status", { method: "GET" });
      if (!res.ok) return READY_FALLBACK;
      return (await res.json()) as SetupStatus;
    } catch (e) {
      if (e instanceof HttpError && e.status === 401) throw e;
      return READY_FALLBACK;
    }
  },
};
