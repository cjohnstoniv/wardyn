/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, beforeEach } from "vitest";
import {
  dismissSetup,
  setupDismissed,
  firstRunLanding,
  integrationsSkipped,
  markIntegrationsSkipped,
  loadVisitedSteps,
  markStepVisited,
} from "./setup-gate";

// setupGateActive (the mandatory redirect of EVERY route to /setup) was deleted
// along with App.tsx's RequireSetupComplete — there is no hard gate to test.
// What's left is the funnel's own per-browser state plus firstRunLanding, which
// steers "/" alone and traps nobody.
describe("setup-gate — the funnel's own per-browser state (no hard gate any more)", () => {
  beforeEach(() => localStorage.clear());

  it("setupDismissed()/dismissSetup() round-trip through localStorage", () => {
    expect(setupDismissed()).toBe(false);
    dismissSetup();
    expect(setupDismissed()).toBe(true);
  });

  // The owner's report this fixes: a first `make setup` landed on an empty Runs
  // board, so the guided tour read as deleted even though every route reached it.
  it("a fresh, reachable install that has never finished the funnel opens on the tour", () => {
    expect(firstRunLanding({ has_runs: false })).toBe("/setup");
  });

  it("an install that has run something goes straight to Runs — the tour is done teaching", () => {
    expect(firstRunLanding({ has_runs: true })).toBe("/runs");
  });

  it("one finish-or-skip retires the redirect for good, even on a still-empty install", () => {
    dismissSetup();
    expect(firstRunLanding({ has_runs: false })).toBe("/runs");
  });

  // READY_FALLBACK answers has_runs:false when the daemon is unreachable, which
  // would otherwise send a broken backend into a tour reading un-ready at every
  // step instead of to Runs, where AppShell renders the unreachable banner.
  it("an unreachable daemon lands on Runs, not the tour", () => {
    expect(firstRunLanding({ unreachable: true, has_runs: false })).toBe("/runs");
  });

  it("integrationsSkipped()/markIntegrationsSkipped() round-trip through localStorage", () => {
    expect(integrationsSkipped()).toBe(false);
    markIntegrationsSkipped();
    expect(integrationsSkipped()).toBe(true);
  });

  it("loadVisitedSteps()/markStepVisited() accumulate a de-duplicated set", () => {
    expect(loadVisitedSteps()).toEqual([]);
    markStepVisited("environment");
    markStepVisited("corp_network");
    markStepVisited("environment"); // no duplicate
    expect(loadVisitedSteps().sort()).toEqual(["corp_network", "environment"]);
  });
});
