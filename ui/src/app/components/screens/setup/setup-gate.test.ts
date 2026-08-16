/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, beforeEach } from "vitest";
import {
  dismissSetup,
  setupDismissed,
  integrationsSkipped,
  markIntegrationsSkipped,
  loadVisitedSteps,
  markStepVisited,
} from "./setup-gate";

// setupGateActive (the mandatory first-run redirect) was deleted along with
// App.tsx's RequireSetupComplete — /setup is a route an operator chooses to
// visit now, no gate to test. What's left is the funnel's own per-browser
// state, still real localStorage flags.
describe("setup-gate — the funnel's own per-browser state (no hard gate any more)", () => {
  beforeEach(() => localStorage.clear());

  it("setupDismissed()/dismissSetup() round-trip through localStorage", () => {
    expect(setupDismissed()).toBe(false);
    dismissSetup();
    expect(setupDismissed()).toBe(true);
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
