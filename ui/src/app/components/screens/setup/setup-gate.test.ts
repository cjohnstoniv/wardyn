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
  markMemberGettingStartedSeen,
  setupGateActive,
} from "./setup-gate";

// Two gates with different jobs: firstRunLanding steers "/" alone and is
// dismissable, while setupGateActive is the real one — it redirects every route
// into the funnel while the daemon reports a failing or degraded setup check.
// The per-browser flags below belong to the first and deliberately have no say
// in the second (a flag outlives the install it describes).
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

describe("firstRunLanding — a member takes a different rule than the admin", () => {
  beforeEach(() => localStorage.clear());

  it("an undismissed member opens on their own Getting Started", () => {
    expect(firstRunLanding({ has_runs: false }, "member")).toBe("/setup");
  });

  it("markMemberGettingStartedSeen() retires the redirect for that member", () => {
    markMemberGettingStartedSeen();
    expect(firstRunLanding({ has_runs: false }, "member")).toBe("/runs");
  });

  it("an unreachable daemon lands a member on Runs, not the tour", () => {
    expect(firstRunLanding({ unreachable: true, has_runs: false }, "member")).toBe("/runs");
  });

  // Negative control: has_runs is a GLOBAL server signal (someone else's
  // runs). A member's own landing must consult only their own per-browser
  // flag, never this field.
  it("a member with global has_runs:true still opens on their own Getting Started", () => {
    expect(firstRunLanding({ has_runs: true }, "member")).toBe("/setup");
  });
});

// ------------------------------------------------------------
// setupGateActive — the server-derived hard gate.
// ------------------------------------------------------------
// The bar is the DAEMON's own severity vocabulary, so a new check participates
// without touching this file. `info` never gates: setup_checks.go reserves it
// for permanent/optional rows, and the absent-model-provider row is exactly
// that — Wardyn governs non-agent runs, so a model is optional.
describe("setupGateActive", () => {
  const ok = { status: "ok" as const };
  const info = { status: "info" as const };
  const warn = { status: "warn" as const };
  const fail = { status: "fail" as const };

  it("does not gate when every check is ok or info", () => {
    expect(setupGateActive({ checks: [ok, info, ok] })).toBe(false);
  });

  it("gates on a warn, and on a fail", () => {
    expect(setupGateActive({ checks: [ok, warn] })).toBe(true);
    expect(setupGateActive({ checks: [ok, fail] })).toBe(true);
  });

  it("never gates a member — their checks are redacted, so the gate would have no exit", () => {
    expect(setupGateActive({ checks: [fail] }, "member")).toBe(false);
    expect(setupGateActive({ checks: [fail] }, "admin")).toBe(true);
  });

  it("never gates an unreachable daemon — a synthetic status proves nothing", () => {
    expect(setupGateActive({ unreachable: true, checks: [fail] })).toBe(false);
  });

  it("never gates on absent evidence (no checks, or the field missing)", () => {
    expect(setupGateActive({ checks: [] })).toBe(false);
    expect(setupGateActive({})).toBe(false);
  });

  // The bug that motivated server-derivation: a browser flag outlives the
  // install it describes, so a wiped database must still re-gate everywhere.
  it("ignores the per-browser dismiss flag entirely", () => {
    dismissSetup();
    expect(setupDismissed()).toBe(true);
    expect(setupGateActive({ checks: [warn] })).toBe(true);
  });
});
