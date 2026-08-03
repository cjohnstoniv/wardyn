/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import type { SetupStatus, Workspace, WorkspaceStatus } from "../../../lib/types";
import { deriveReadiness } from "../onboarding/intro";
import { DEMO_STEP_IDS, OPTIONAL_STEPS, PHASES, STEP_HEADING, STEP_LABEL, STEP_ORDER, stepBadges, stepDone } from "./steps";
import { baseStatus as sharedBaseStatus } from "./test-fixtures";

// This suite's own pin is CC1-only compatibility (no CC2/CC3), trimmed to only
// what these pure functions read.
function baseStatus(overrides: Partial<SetupStatus> = {}): SetupStatus {
  return sharedBaseStatus({
    runner: { driver: "docker", confinement_classes: ["CC1"] },
    ...overrides,
  });
}

function ws(id: string, status: WorkspaceStatus): Workspace {
  return {
    id,
    name: id,
    kind: "local_dir",
    source: `/tmp/${id}`,
    status,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  };
}

describe("integrations badge — Optional -> Ready · N connected (Skipped is an orchestrator override)", () => {
  it("reads Optional/false with zero connected integrations", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    expect(stepBadges(status, readiness, [], 0).integrations).toEqual({
      text: "Optional",
      tone: "neutral",
    });
    expect(stepDone(status, readiness, [], 0).integrations).toBe(false);
  });

  it("reads 'Ready · N connected' and done=true once the count is > 0", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    expect(stepBadges(status, readiness, [], 3).integrations).toEqual({
      text: "Ready · 3 connected",
      tone: "success",
    });
    expect(stepDone(status, readiness, [], 3).integrations).toBe(true);
  });
});

describe("demos — advisory in the pure fns (per-demo checkmark is applied at the orchestrator)", () => {
  it("each demo step is Optional/false regardless of runs — the launched signal is per-browser, not in status", () => {
    const status = baseStatus({ has_runs: true });
    const readiness = deriveReadiness(status);
    for (const id of DEMO_STEP_IDS) {
      expect(stepBadges(status, readiness, [], 0)[id]).toEqual({ text: "Optional", tone: "neutral" });
      expect(stepDone(status, readiness, [], 0)[id]).toBe(false);
    }
  });
});

describe("environment badge", () => {
  it("reads amber 'Needs setup' (not ready-toned) when zero barriers are ready", () => {
    const status = baseStatus({ runner: { driver: "docker", confinement_classes: [] } });
    const readiness = deriveReadiness(status);
    expect(stepBadges(status, readiness, [], 0).environment).toEqual({
      text: "Needs setup",
      tone: "warning",
    });
    expect(stepDone(status, readiness, [], 0).environment).toBe(false);
  });
});

describe("workspaces badge", () => {
  it("shows 'Ready · 2 onboarded' and done=true with two ready workspaces", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    const workspaces = [ws("w1", "ready"), ws("w2", "ready")];
    expect(stepBadges(status, readiness, workspaces, 0).workspaces).toEqual({
      text: "Ready · 2 onboarded",
      tone: "success",
    });
    expect(stepDone(status, readiness, workspaces, 0).workspaces).toBe(true);
  });

  // The regression that made the product look permanently unfinished: the
  // terminal-success status moved from `ready` to `scanned`, and this badge
  // kept comparing against the literal `ready`. A workspace that had finished
  // scanning could never earn its checkmark. Both spellings must count — the
  // current one and the legacy rows written before the collapse.
  it("counts a `scanned` workspace as done, not just the legacy `ready`", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    const workspaces = [ws("w1", "scanned"), ws("w2", "ready")];
    expect(stepBadges(status, readiness, workspaces, 0).workspaces).toEqual({
      text: "Ready · 2 onboarded",
      tone: "success",
    });
    expect(stepDone(status, readiness, workspaces, 0).workspaces).toBe(true);
  });

  it("shows an info-tone 'In progress' and done=false with only a pending workspace", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    const workspaces = [ws("w1", "pending_scan")];
    expect(stepBadges(status, readiness, workspaces, 0).workspaces).toEqual({
      text: "In progress",
      tone: "info",
    });
    expect(stepDone(status, readiness, workspaces, 0).workspaces).toBe(false);
  });

  it("shows 'Optional' with no workspaces at all", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    expect(stepBadges(status, readiness, [], 0).workspaces).toEqual({
      text: "Optional",
      tone: "neutral",
    });
  });

  it("reads 'Ready · 1 of 2 onboarded' when one of two is still importing", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    const workspaces = [ws("a", "ready"), ws("b", "pending_scan")];
    expect(stepBadges(status, readiness, workspaces, 0).workspaces).toEqual({
      text: "Ready · 1 of 2 onboarded",
      tone: "success",
    });
  });
});

describe("environment done — barrier-only, matching its badge", () => {
  it("stays done under an unrelated failing check (Review owns the whole-checks rollup)", () => {
    const status = baseStatus({
      checks: [{ id: "runner", label: "Sandbox runner", status: "fail", detail: "no runner" }],
    });
    const readiness = deriveReadiness(status);
    // Badge and dot read the same barrier-only signal — no green badge with a
    // blank dot.
    expect(stepBadges(status, readiness, [], 0).environment.tone).toBe("success");
    expect(stepDone(status, readiness, [], 0).environment).toBe(true);
    // review still rolls the failure up.
    expect(stepDone(status, readiness, [], 0).review).toBe(false);
  });
});

describe("frozen contract — ids, labels, headings, order", () => {
  it("pins the frozen step ids and labels (e2e clicks `Next: {label}`)", () => {
    expect(Object.entries(STEP_LABEL)).toEqual([
      ["environment", "Environment"],
      ["corp_network", "Corporate network"],
      ["integrations", "Integrations"],
      // The four Demos sub-steps — labels come from the demo catalog titles.
      ["sealed-box", "The sealed box"],
      ["fail-then-approve", "Fail, then approve"],
      ["held-at-the-door", "Held at the door"],
      ["lines-that-cant-be-crossed", "Lines that can't be crossed"],
      ["workspaces", "Workspaces"],
      ["review", "Review"],
      ["launch", "Launch"],
    ]);
    expect(STEP_HEADING.environment).toBe("Pick your barrier");
    expect(STEP_HEADING.corp_network).toBe("Corporate network");
    expect(STEP_HEADING.integrations).toBe("Connect what's outside Wardyn");
  });

  // 9 -> 10: Corporate network comes BACK as its own step, right BEFORE
  // Integrations — see steps.ts's PHASES comment for why the order itself
  // (not a banner) is the fix for "blocked network reads as bad credential".
  it("pins STEP_ORDER to the phase walk (essentials -> demos -> your work -> finish)", () => {
    expect(STEP_ORDER).toEqual([
      "environment",
      "corp_network",
      "integrations",
      "sealed-box",
      "fail-then-approve",
      "held-at-the-door",
      "lines-that-cant-be-crossed",
      "workspaces",
      "review",
      "launch",
    ]);
    expect(STEP_ORDER).toHaveLength(10);
    expect(PHASES.flatMap((p) => p.steps)).toEqual(STEP_ORDER);
    // The four Demos sub-steps ARE the demos phase, in catalog order.
    expect(PHASES.find((p) => p.id === "demos")?.steps).toEqual([...DEMO_STEP_IDS]);
  });

  it("corp_network is optional", () => {
    expect(OPTIONAL_STEPS.has("corp_network")).toBe(true);
  });
});

describe("corp_network badge — the 4-rung ladder (Optional -> Skipped is an orchestrator override, same as integrations)", () => {
  const unset = { proxyConfigured: false, proxyDetected: false, redirectCount: 0 };

  it("reads Optional/false with nothing detected and nothing configured (the default when corpNetwork is omitted)", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    expect(stepBadges(status, readiness, [], 0).corp_network).toEqual({ text: "Optional", tone: "neutral" });
    expect(stepDone(status, readiness, [], 0).corp_network).toBe(false);
  });

  it("reads amber 'Detected — not configured' once a proxy was detected but nothing is saved", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    const corpNetwork = { ...unset, proxyDetected: true };
    expect(stepBadges(status, readiness, [], 0, corpNetwork).corp_network).toEqual({
      text: "Detected — not configured",
      tone: "warning",
    });
    expect(stepDone(status, readiness, [], 0, corpNetwork).corp_network).toBe(false);
  });

  it("reads 'Ready · proxy' once the proxy is configured, even with zero redirects", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    const corpNetwork = { ...unset, proxyConfigured: true };
    expect(stepBadges(status, readiness, [], 0, corpNetwork).corp_network).toEqual({ text: "Ready · proxy", tone: "success" });
    expect(stepDone(status, readiness, [], 0, corpNetwork).corp_network).toBe(true);
  });

  it("reads 'Ready · N redirects' (no 'proxy') when only redirects are configured", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    const corpNetwork = { ...unset, redirectCount: 1 };
    expect(stepBadges(status, readiness, [], 0, corpNetwork).corp_network).toEqual({ text: "Ready · 1 redirect", tone: "success" });
    expect(stepDone(status, readiness, [], 0, corpNetwork).corp_network).toBe(true);
  });

  // The mock's own self-contradiction (prose says "proxy + 3 redirects" but
  // renders a 4-entry fixture) — the badge must derive the count from the
  // data, never a fixed word, so this reads "+ 4 redirects" here.
  it("derives the redirect count from the data — proxy + 4 redirects, not a stale '3'", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    const corpNetwork = { proxyConfigured: true, proxyDetected: false, redirectCount: 4 };
    expect(stepBadges(status, readiness, [], 0, corpNetwork).corp_network).toEqual({
      text: "Ready · proxy + 4 redirects",
      tone: "success",
    });
  });

  it("a configured proxy wins over mere detection (Ready, not the amber Detected line)", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    const corpNetwork = { proxyConfigured: true, proxyDetected: true, redirectCount: 0 };
    expect(stepBadges(status, readiness, [], 0, corpNetwork).corp_network.tone).toBe("success");
  });
});

describe("review/launch gate — the BARRIER is the only hard requirement; a model is optional", () => {
  it("barrier-only host (no model): review + launch already claim launch-readiness", () => {
    // backend ready=true is barrier-only (setup.go); no model connected — and that
    // is enough: a plain governed run / interactive run needs no model.
    const status = baseStatus({ ready: true });
    const r = deriveReadiness(status);
    const badges = stepBadges(status, r, [], 0);
    expect(badges.review).toEqual({ text: "Ready to launch", tone: "success" });
    expect(badges.launch).toEqual({ text: "Ready to launch", tone: "success" });
    expect(stepDone(status, r, [], 0).review).toBe(true);
  });

  it("no barrier: review/launch nudge to set up the barrier first (the one requirement)", () => {
    const status = baseStatus({ ready: false, runner: { driver: "none", confinement_classes: [] } });
    const r = deriveReadiness(status);
    const badges = stepBadges(status, r, [], 0);
    expect(badges.review).toEqual({ text: "Set up the barrier first", tone: "neutral" });
    expect(badges.launch).toEqual({ text: "Set up the barrier first", tone: "neutral" });
    expect(stepDone(status, r, [], 0).review).toBe(false);
  });

  it("a connected model is a bonus, not a gate: same success texts as barrier-only", () => {
    const status = baseStatus({
      ready: true,
      secrets: { present: ["anthropic-api-key"], github_app: false },
    });
    const r = deriveReadiness(status);
    const badges = stepBadges(status, r, [], 1);
    expect(badges.review).toEqual({ text: "Ready to launch", tone: "success" });
    expect(badges.launch).toEqual({ text: "Ready to launch", tone: "success" });
    expect(stepDone(status, r, [], 1).review).toBe(true);
  });

  it("integrations step is Optional (neutral), never a 'Needs setup' warning, with nothing connected", () => {
    const status = baseStatus({ ready: true });
    const r = deriveReadiness(status);
    expect(stepBadges(status, r, [], 0).integrations).toEqual({ text: "Optional", tone: "neutral" });
  });
});
