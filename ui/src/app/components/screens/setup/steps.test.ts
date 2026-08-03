/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import type { EgressRedirect, SetupStatus, Workspace, WorkspaceStatus } from "../../../lib/types";
import type { ProxyTestResult } from "../../../lib/api/health";
import { T } from "../../../lib/integrations";
import { deriveReadiness } from "../onboarding/intro";
import {
  DEMO_STEP_IDS,
  OPTIONAL_STEPS,
  PHASES,
  STEP_HEADING,
  STEP_LABEL,
  STEP_ORDER,
  corpNetworkGate,
  stepBadges,
  stepDone,
  type CorpNetworkState,
} from "./steps";
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

  it("corp_network is required, not optional — proof of internet access gates Next", () => {
    expect(OPTIONAL_STEPS.has("corp_network")).toBe(false);
  });
});

describe("corp_network gate — corpNetworkGate drives the note beside Next, the badge, and stepDone (the mock's corpGate ladder)", () => {
  const unset: CorpNetworkState = {
    proxyConfigured: false,
    proxyDetected: false,
    redirectCount: 0,
    egressVisited: false,
    redirectProbes: {},
  };
  const reached: ProxyTestResult = { state: "reached", detail: "Reached … — payloads matched", via: "proxy" };
  const reachedDirect: ProxyTestResult = { state: "reached", detail: "Reached … directly — payloads matched", via: "direct" };
  const blocked: ProxyTestResult = { state: "blocked", detail: "connection refused" };
  const intercepted: ProxyTestResult = { state: "blocked", detail: "something answered", intercepted: true };
  const customPass: ProxyTestResult = { state: "reached", detail: "The request completed", custom: true };
  const noRunner: ProxyTestResult = { state: "no_runner", detail: "no runner configured" };
  const bypass: ProxyTestResult = { state: "bypass", detail: "reachable directly too" };

  it("reads 'Untested'/off before any probe has run (the default when corpNetwork is omitted)", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    expect(stepBadges(status, readiness, [], 0).corp_network).toEqual({ text: "Untested", tone: "warning" });
    expect(stepDone(status, readiness, [], 0).corp_network).toBe(false);
    expect(corpNetworkGate(unset, [])).toEqual({ on: false, reason: T.GATE_UNTESTED, tone: "warning" });
  });

  it("a detected-but-unconfigured proxy reads 'Detected — not configured' pre-probe (evidence, amber)", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    const corpNetwork = { ...unset, proxyDetected: true };
    expect(stepBadges(status, readiness, [], 0, corpNetwork).corp_network).toEqual({
      text: "Detected — not configured",
      tone: "warning",
    });
  });

  it("reads amber 'Blocked' once the connectivity probe fails, and blocks Next with the gate sentence", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    const corpNetwork = { ...unset, proxyProbe: blocked };
    expect(stepBadges(status, readiness, [], 0, corpNetwork).corp_network).toEqual({ text: "Blocked", tone: "warning" });
    expect(stepDone(status, readiness, [], 0, corpNetwork).corp_network).toBe(false);
    expect(corpNetworkGate(corpNetwork, [])).toEqual({ on: false, reason: T.GATE_BLOCKED, tone: "warning" });
  });

  it("intercepted is a blocked FLAVOR rendered apart — its own badge text and its own gate instruction", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    const corpNetwork = { ...unset, proxyProbe: intercepted };
    expect(stepBadges(status, readiness, [], 0, corpNetwork).corp_network).toEqual({
      text: "Blocked · intercepted",
      tone: "warning",
    });
    expect(corpNetworkGate(corpNetwork, [])).toEqual({ on: false, reason: T.GATE_INTERCEPTED, tone: "warning" });
    expect(stepDone(status, readiness, [], 0, corpNetwork).corp_network).toBe(false);
  });

  it("no_runner UNLOCKS Next with a neutral standing note but never earns the checkmark — nothing was proven", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    const corpNetwork = { ...unset, proxyProbe: noRunner };
    expect(stepBadges(status, readiness, [], 0, corpNetwork).corp_network).toEqual({
      text: "Untested · no runner",
      tone: "neutral",
    });
    expect(corpNetworkGate(corpNetwork, [])).toEqual({ on: true, reason: T.NORUNNER_NOTE, tone: "neutral" });
    // done = gate.on && reached (the mock's own rule): the operator may
    // continue, but the step never claims a proof Wardyn could not collect.
    expect(stepDone(status, readiness, [], 0, corpNetwork).corp_network).toBe(false);
    // The bypass holds even with unconfigured/untested redirects sitting there —
    // no_runner means NOTHING on this host can be probed, egress included.
    const redirects: EgressRedirect[] = [{ from: "https://registry.npmjs.org", to: "https://mirror.corp.internal" }];
    expect(corpNetworkGate(corpNetwork, redirects).on).toBe(true);
  });

  it("reached but the Egress tab was never OPENED still blocks — even at zero redirects, and before any row proofs", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    const corpNetwork = { ...unset, proxyProbe: reached };
    expect(stepDone(status, readiness, [], 0, corpNetwork).corp_network).toBe(false);
    expect(corpNetworkGate(corpNetwork, [])).toEqual({ on: false, reason: T.GATE_EGRESS_UNSEEN, tone: "warning" });
    // The visit requirement comes BEFORE per-row proofs in the ladder: rows
    // derived from legacy config can exist without the tab ever being seen.
    const redirects: EgressRedirect[] = [{ from: "https://registry.npmjs.org", to: "https://mirror.corp.internal" }];
    expect(corpNetworkGate(corpNetwork, redirects).reason).toBe(T.GATE_EGRESS_UNSEEN);
    // The badge stays the connectivity FACT meanwhile (the mock's corpBadge):
    // the gate note and the egress tab's own dot carry the remaining work.
    expect(stepBadges(status, readiness, [], 0, corpNetwork).corp_network).toEqual({
      text: "Reached · proxy",
      tone: "success",
    });
  });

  it("reached (direct) + egress visited + zero redirects reads 'Reached · direct' and is done", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    const corpNetwork = { ...unset, proxyProbe: reachedDirect, egressVisited: true };
    expect(stepBadges(status, readiness, [], 0, corpNetwork).corp_network).toEqual({
      text: "Reached · direct",
      tone: "success",
    });
    expect(corpNetworkGate(corpNetwork, [])).toEqual({ on: true });
    expect(stepDone(status, readiness, [], 0, corpNetwork).corp_network).toBe(true);
  });

  it("a custom-endpoint pass unlocks with its standing note and the weaker info badge — never the success treatment", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    const corpNetwork = { ...unset, proxyProbe: customPass, egressVisited: true };
    expect(stepBadges(status, readiness, [], 0, corpNetwork).corp_network).toEqual({
      text: "Reached · custom endpoint",
      tone: "info",
    });
    expect(corpNetworkGate(corpNetwork, [])).toEqual({ on: true, reason: T.GATE_CUSTOM_ON, tone: "neutral" });
    // A custom pass IS reached — it earns the checkmark; the tone and the
    // standing note carry the weaker footing.
    expect(stepDone(status, readiness, [], 0, corpNetwork).corp_network).toBe(true);
  });

  it("a redirect that exists but was never tested blocks with the generic prove-them sentence — egressVisited doesn't substitute for a real test", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    const corpNetwork = { ...unset, proxyProbe: reached, egressVisited: true, redirectCount: 1 };
    const redirects: EgressRedirect[] = [{ from: "https://registry.npmjs.org", to: "https://mirror.corp.internal" }];
    expect(stepDone(status, readiness, [], 0, corpNetwork, redirects).corp_network).toBe(false);
    expect(corpNetworkGate(corpNetwork, redirects)).toEqual({
      on: false,
      reason: T.GATE_EGRESS_UNTESTED,
      tone: "warning",
    });
  });

  it("failing rows are NAMED, all at once — a bypassed and a blocked redirect both read as rows to fix", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    const corpNetwork = {
      ...unset,
      proxyProbe: reached,
      egressVisited: true,
      redirectCount: 2,
      redirectProbes: { "https://registry.npmjs.org": bypass, "https://pypi.org/simple": blocked },
    };
    const redirects: EgressRedirect[] = [
      { from: "https://registry.npmjs.org", to: "https://mirror.corp.internal" },
      { from: "https://pypi.org/simple", to: "https://mirror.corp.internal/pypi" },
    ];
    expect(stepDone(status, readiness, [], 0, corpNetwork, redirects).corp_network).toBe(false);
    expect(corpNetworkGate(corpNetwork, redirects).reason).toBe(
      "Fix https://registry.npmjs.org and https://pypi.org/simple above — every configured redirect must prove reached before this step hands off. Or remove the rows.",
    );
  });

  // The mock's own self-contradiction (prose says "proxy + 3 redirects" but
  // renders a 4-entry fixture) — the badge must derive the count from the
  // data, never a fixed word, so this reads "+ 4 redirects" here.
  it("derives the redirect count from the data — Reached · proxy + 4 redirects once every one reaches, not a stale '3'", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    const froms = ["a.example.com", "b.example.com", "c.example.com", "d.example.com"];
    const redirects: EgressRedirect[] = froms.map((from) => ({ from, to: `${from}.mirror.corp.internal` }));
    const corpNetwork: CorpNetworkState = {
      ...unset,
      proxyProbe: reached,
      proxyConfigured: true,
      egressVisited: true,
      redirectCount: 4,
      redirectProbes: Object.fromEntries(froms.map((f) => [f, reached])),
    };
    expect(stepBadges(status, readiness, [], 0, corpNetwork).corp_network).toEqual({
      text: "Reached · proxy + 4 redirects",
      tone: "success",
    });
    expect(stepDone(status, readiness, [], 0, corpNetwork, redirects).corp_network).toBe(true);
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
