/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, beforeEach } from "vitest";
import {
  clearStaleVisitFlags,
  clearStaleVisitFlagsOnce,
  resetStaleFlagsCheckForTests,
  dismissSetup,
  setupDismissed,
  firstRunLanding,
  integrationsSkipped,
  markIntegrationsSkipped,
  loadVisitedSteps,
  markStepVisited,
  setupGateActive,
  gateAlreadyFired,
  markGateFired,
  resetGateForTests,
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
    expect(firstRunLanding({ unreachable: true, has_runs: false })).toBe(
      "/runs",
    );
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

  // Neither flag gates anything — their only job is the rail's "Skipped"
  // badge — so a mark left by a PREVIOUS install on this browser is a false
  // green on a fresh one that never actually visited that step.
  it("clearStaleVisitFlags: a fresh, never-onboarded, never-run install clears both stale flags", () => {
    markIntegrationsSkipped();
    markStepVisited("corp_network");
    expect(clearStaleVisitFlags({ onboarding_complete: false, has_runs: false })).toBe(true);
    expect(integrationsSkipped()).toBe(false);
    expect(loadVisitedSteps()).toEqual([]);
  });

  // Negative control: has_runs:true means this browser's marks describe THIS
  // install's real history — must survive.
  it("clearStaleVisitFlags negative control: has_runs:true keeps both flags", () => {
    markIntegrationsSkipped();
    markStepVisited("corp_network");
    expect(clearStaleVisitFlags({ onboarding_complete: false, has_runs: true })).toBe(false);
    expect(integrationsSkipped()).toBe(true);
    expect(loadVisitedSteps()).toEqual(["corp_network"]);
  });

  // Negative control: an onboarded install (has_runs may still be false —
  // demos/governed commands need no run) also keeps both flags.
  it("clearStaleVisitFlags negative control: onboarding_complete:true keeps both flags", () => {
    markIntegrationsSkipped();
    expect(clearStaleVisitFlags({ onboarding_complete: true, has_runs: false })).toBe(false);
    expect(integrationsSkipped()).toBe(true);
  });
});

// The PRODUCTION path (setup-screen.tsx calls clearStaleVisitFlagsOnce,
// never the unlatched clearStaleVisitFlags above) — pinned separately so its
// once-per-load semantics actually has coverage.
describe("clearStaleVisitFlagsOnce — the module-level once-per-load latch (L3)", () => {
  beforeEach(() => {
    localStorage.clear();
    resetStaleFlagsCheckForTests();
  });

  it("clears on the first call within a load, and does nothing on the second", () => {
    markIntegrationsSkipped();
    const fresh = { onboarding_complete: false, has_runs: false };
    expect(clearStaleVisitFlagsOnce(fresh)).toBe(true);
    expect(integrationsSkipped()).toBe(false);

    // A mark set AFTER the first call — the ordinary in-session case, and
    // the whole point of the once-per-load latch — must survive the second
    // call within the same load, even though the condition (`fresh`) is
    // unchanged.
    markIntegrationsSkipped();
    expect(clearStaleVisitFlagsOnce(fresh)).toBe(false);
    expect(integrationsSkipped()).toBe(true);
  });

  it("resetStaleFlagsCheckForTests re-arms the latch, mirroring a real reload", () => {
    const fresh = { onboarding_complete: false, has_runs: false };
    expect(clearStaleVisitFlagsOnce(fresh)).toBe(true);
    expect(clearStaleVisitFlagsOnce(fresh)).toBe(false);
    resetStaleFlagsCheckForTests();
    markIntegrationsSkipped();
    expect(clearStaleVisitFlagsOnce(fresh)).toBe(true);
    expect(integrationsSkipped()).toBe(false);
  });
});

describe("firstRunLanding — a member takes a different rule than the admin", () => {
  beforeEach(() => localStorage.clear());

  // A member always lands on THEIR page; it is theirs to leave. The old
  // per-browser "seen" flag is gone: its one stated purpose (surviving a
  // pruning of the member's runs) guards a scenario this codebase cannot
  // produce, while its live failure mode — a shared browser handing member B
  // member A's mark — was real. Done-states come from the page's own
  // creator-scoped listRuns, not from this decision.
  it("a member opens on their own Getting Started", () => {
    expect(firstRunLanding({ has_runs: false }, "user")).toBe("/setup");
  });

  it("an unreachable daemon lands a member on Runs, not the tour", () => {
    expect(
      firstRunLanding({ unreachable: true, has_runs: false }, "user"),
    ).toBe("/runs");
  });

  // Negative control: has_runs is a GLOBAL server signal (someone else's
  // runs). A member's landing never consults it.
  it("a member with global has_runs:true still opens on their own Getting Started", () => {
    expect(firstRunLanding({ has_runs: true }, "user")).toBe("/setup");
  });
});

describe("firstRunLanding — the INSTALL's onboarding mark decides for an admin", () => {
  beforeEach(() => localStorage.clear());

  it("a server-onboarded install lands on Runs, whatever this browser thinks", () => {
    // No local flag set — a browser that has never been here before.
    expect(
      firstRunLanding({ has_runs: false, onboarding_complete: true }),
    ).toBe("/runs");
  });

  it("a server-NOT-onboarded install opens the tour even if this browser dismissed one before", () => {
    // The origin-scoped-flag bug, pinned as a spec: a stale local dismiss must
    // not skip a fresh install's tour once the server says not-onboarded.
    dismissSetup();
    expect(
      firstRunLanding({ has_runs: false, onboarding_complete: false }),
    ).toBe("/setup");
  });

  it("an older daemon (field absent) falls back to the legacy browser flag", () => {
    expect(firstRunLanding({ has_runs: false })).toBe("/setup");
    dismissSetup();
    expect(firstRunLanding({ has_runs: false })).toBe("/runs");
  });
});

// setupGateActive — the server-derived hard gate.
//
// 0.7.8: the bar is the DAEMON's own `blocking` flag (SetupCheck.Blocking,
// internal/api/setup_checks.go), not a status/id guess made here. A `warn`/
// `fail` grade is necessary but NOT sufficient — most of the checklist can
// carry either without ever confiscating the console.
describe("setupGateActive", () => {
  const ok = { id: "runner", status: "ok" as const };
  const info = { id: "env_builder", status: "info" as const };
  // A row graded warn/fail but NOT marked blocking — most of the checklist,
  // post-0.7.8: optional rows, and rows read through the CALLER's own
  // credential (llm_provider, bedrock_provider, harness_credential,
  // harness_credential_aws) all look exactly like this on the wire.
  const warnNonBlocking = { id: "site_config", status: "warn" as const };
  const failNonBlocking = { id: "k8s_egress_containment", status: "fail" as const };
  // A row the daemon marked blocking. Not necessarily one of the three real
  // ids that ever carry it (runner/confinement_floor/sso_rbac) — the function
  // trusts the flag alone, so the fixture only needs to carry it.
  const blocking = { id: "runner", status: "fail" as const, blocking: true };

  // The gate reads the daemon's `blocking` flag alone — never a hardcoded id
  // list. An id list is exactly the trap this avoids: enumerating a family of
  // ids that happen to matter today misses the next one a daemon adds
  // (llm_provider, then bedrock_provider, then harness_credential_aws would
  // each need their own addition). There is no id list left to outgrow: a
  // row gates on `blocking`, whatever its id.
  it("never gates on a row the daemon did not mark blocking, however it is graded", () => {
    expect(setupGateActive({ checks: [ok, warnNonBlocking] })).toBe(false);
    expect(setupGateActive({ checks: [ok, failNonBlocking] })).toBe(false);
    expect(setupGateActive({ checks: [warnNonBlocking, failNonBlocking] })).toBe(false);
    // The exact field-report shape: a lapsed admin AWS SSO session, graded
    // warn, carries no `blocking` — the daemon never sets it on this id.
    expect(setupGateActive({ checks: [{ id: "harness_credential_aws", status: "warn" }] })).toBe(false);
  });

  it("gates on a row the daemon marked blocking", () => {
    expect(setupGateActive({ checks: [ok, blocking] })).toBe(true);
    // A non-blocking warn/fail beside it changes nothing — one blocking row
    // is sufficient regardless of what else is on the list.
    expect(setupGateActive({ checks: [warnNonBlocking, blocking] })).toBe(true);
  });

  it("does not gate when every check is ok or info", () => {
    expect(setupGateActive({ checks: [ok, info, ok] })).toBe(false);
  });

  it("never gates a member — their checks are redacted, so the gate would have no exit", () => {
    expect(setupGateActive({ checks: [blocking] }, "user")).toBe(false);
    expect(setupGateActive({ checks: [blocking] }, "admin")).toBe(true);
  });

  it("never gates an unreachable daemon — a synthetic status proves nothing", () => {
    expect(setupGateActive({ unreachable: true, checks: [blocking] })).toBe(false);
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
    expect(setupGateActive({ checks: [blocking] })).toBe(true);
  });

  // "A place you go, not a wall you are trapped behind": once the INSTALL has
  // been through onboarding, a later failure informs — banner, badges, the
  // funnel a click away — but never confiscates the console. Without this, a
  // runner dying on day 30 would wall an operator out of Audit and Runs, and
  // a deliberately runner-less deployment (the e2e harness is one) could
  // never leave the funnel at all.
  it("never gates an install that has completed onboarding", () => {
    expect(setupGateActive({ checks: [blocking], onboarding_complete: true })).toBe(
      false,
    );
    expect(setupGateActive({ checks: [warnNonBlocking], onboarding_complete: true })).toBe(
      false,
    );
    expect(
      setupGateActive({ checks: [blocking], onboarding_complete: false }),
    ).toBe(true);
  });
});

// The gate fires once per page LOAD — an access lands a gated install in the
// funnel, but the funnel's own affordances (People's "Open Permissions") must
// then be able to navigate to gated routes without being bounced back to step
// one. Module-level on purpose: /setup lives outside the gate's route wrapper,
// so component state would be lost exactly when it matters.
describe("gate-once-per-load", () => {
  beforeEach(() => resetGateForTests());

  it("arms once and stays fired for the rest of the load", () => {
    expect(gateAlreadyFired()).toBe(false);
    markGateFired();
    expect(gateAlreadyFired()).toBe(true);
  });
});
