/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, beforeEach } from "vitest";
import { setupGateActive, dismissSetup } from "./setup-gate";
import { baseStatus } from "./test-fixtures";

describe("setupGateActive — the HARD first-run gate", () => {
  beforeEach(() => localStorage.clear());

  it("gates a fresh local console with no runs", () => {
    expect(setupGateActive(baseStatus())).toBe(true);
  });

  it("does NOT hard-gate a console with runs when a probe says not ready", () => {
    // A transient daemon blip flips ready:false; an operator with existing runs
    // must not be yanked out of the whole console by it.
    expect(setupGateActive(baseStatus({ has_runs: true, ready: false }))).toBe(false);
  });

  it("clears once dismissed", () => {
    dismissSetup();
    expect(setupGateActive(baseStatus())).toBe(false);
  });

  it("never gates on SSO or an unreachable daemon", () => {
    expect(setupGateActive(baseStatus({ auth: { mode: "sso", local_loopback: false } }))).toBe(false);
    expect(setupGateActive(baseStatus({ unreachable: true }))).toBe(false);
  });
});
