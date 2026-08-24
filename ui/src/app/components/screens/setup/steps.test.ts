/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import type { EgressRedirect, SetupStatus, Workspace, WorkspaceStatus } from "../../../lib/types";
import type { ProxyTestResult } from "../../../lib/api/health";
import { T } from "../../../lib/integrations";
import { deriveReadiness } from "../../../lib/readiness";
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
  stepOrder,
  type CorpNetworkState,
} from "./steps";
import { DEMOS } from "../demos/demo-catalog";
import { baseStatus as sharedBaseStatus } from "../../../lib/test-fixtures";

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
  it("shows 'Ready · 2 onboarded' and done=true with two usable workspaces", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    const workspaces = [ws("w1", "scanned"), ws("w2", "scanned")];
    expect(stepBadges(status, readiness, workspaces, 0).workspaces).toEqual({
      text: "Ready · 2 onboarded",
      tone: "success",
    });
    expect(stepDone(status, readiness, workspaces, 0).workspaces).toBe(true);
  });

  // The regression that made the product look permanently unfinished: the
  // terminal-success status moved from `ready` to `scanned`, and this badge
  // kept comparing against the literal `ready`. A workspace that had finished
  // scanning could never earn its checkmark. isUsable is the one predicate.
  it("counts a `scanned` workspace as done via the isUsable predicate", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    const workspaces = [ws("w1", "scanned"), ws("w2", "scanned")];
    expect(stepBadges(status, readiness, workspaces, 0).workspaces).toEqual({
      text: "Ready · 2 onboarded",
      tone: "success",
    });
    expect(stepDone(status, readiness, workspaces, 0).workspaces).toBe(true);
  });

  // The regression this replaces: "In progress" was shown for anything not yet
  // scanned, and 0.5 removed the scan — so the rail read "Workspaces · In
  // progress" forever, beside a workspace a run could attach immediately. A
  // legacy pending_scan row (migration 0036 heals them) counts as onboarded.
  it("counts a legacy pending_scan workspace as onboarded — there is no in-progress state", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    const workspaces = [ws("w1", "pending_scan")];
    expect(stepBadges(status, readiness, workspaces, 0).workspaces).toEqual({
      text: "Ready · 1 onboarded",
      tone: "success",
    });
    expect(stepDone(status, readiness, workspaces, 0).workspaces).toBe(true);
  });

  it("shows 'Optional' with no workspaces at all", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    expect(stepBadges(status, readiness, [], 0).workspaces).toEqual({
      text: "Optional",
      tone: "neutral",
    });
  });

  // The "N of M" split existed to report scan progress across a mixed list.
  // Every onboarded workspace is attachable now, so the count is just the count.
  it("counts every onboarded workspace, whatever legacy status it carries", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    const workspaces = [ws("a", "scanned"), ws("b", "pending_scan")];
    expect(stepBadges(status, readiness, workspaces, 0).workspaces).toEqual({
      text: "Ready · 2 onboarded",
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
      ["corp_network", "Network"],
      ["integrations", "Secrets"],
      // Every demo sub-step — labels come from the demo catalog titles.
      ["sealed-box", "The sealed box"],
      ["fail-then-approve", "Fail, then approve"],
      ["held-at-the-door", "Held at the door"],
      ["lines-that-cant-be-crossed", "Lines that can't be crossed"],
      ["agent-in-the-box", "The agent in the box"],
      ["record-a-policy", "Record a policy"],
      ["once-or-for-good", "Once, or for good"],
      ["write-only-by-design", "Write-only, even for you"],
      ["key-never-in-the-box", "The key that never enters the box"],
      ["authorized-not-issued", "Authorized, not issued"],
      ["rest-api-token", "A bearer token for a real API"],
      ["pat-stdout-only", "A PAT that only ever exists in a pipe"],
      ["ssh-briefly-resident", "The one that touches disk — briefly"],
      ["github-app-broker", "A token the sandbox never even sees"],
      ["sts-fail-closed", "No identity, no credential"],
      // "Your work" is just the one workspace step — the tier-1/2 library
      // steps (Directories & repos, Base images) retired with
      // sources-library.tsx/image-catalog.tsx.
      ["workspaces", "Workspaces"],
      ["review", "Review"],
    ]);
    expect(STEP_HEADING.environment).toBe("Pick your barrier");
    expect(STEP_HEADING.corp_network).toBe("Network");
    expect(STEP_HEADING.integrations).toBe("Secrets");
  });

  it("pins STEP_ORDER to the phase walk (essentials -> egress demos -> secrets demos -> your work -> finish)", () => {
    expect(STEP_ORDER).toEqual([
      "environment",
      "corp_network",
      "integrations",
      "sealed-box",
      "fail-then-approve",
      "held-at-the-door",
      "lines-that-cant-be-crossed",
      "agent-in-the-box",
      "record-a-policy",
      "once-or-for-good",
      "write-only-by-design",
      "key-never-in-the-box",
      "authorized-not-issued",
      "rest-api-token",
      "pat-stdout-only",
      "ssh-briefly-resident",
      "github-app-broker",
      "sts-fail-closed",
      "workspaces",
      "review",
    ]);
    expect(STEP_ORDER).toHaveLength(20);
    expect(PHASES.flatMap((p) => p.steps)).toEqual(STEP_ORDER);
    // Getting Started is the ONE demos surface: every catalog demo is a
    // sub-step, in catalog order, split into the two sections by `Demo.section`
    // alone — never a hand-kept list that can drift from the catalog.
    expect(DEMO_STEP_IDS).toEqual(DEMOS.map((d) => d.id));
    expect([
      ...(PHASES.find((p) => p.id === "demos_egress")?.steps ?? []),
      ...(PHASES.find((p) => p.id === "demos_secrets")?.steps ?? []),
    ]).toEqual([...DEMO_STEP_IDS]);
    expect(PHASES.find((p) => p.id === "demos_secrets")?.label).toBe("Secrets demos");
    // write-only-by-design leads the secrets section: it is how the operator
    // stores the secret every granted demo below it gates on.
    expect(PHASES.find((p) => p.id === "demos_secrets")?.steps[0]).toBe("write-only-by-design");
  });

  it("corp_network is required, not optional — proof of internet access gates Next", () => {
    expect(OPTIONAL_STEPS.has("corp_network")).toBe(false);
  });
});

// stepOrder(status) — the WALK, as opposed to STEP_ORDER's full contract.
describe("stepOrder — conditional demo steps drop out of the walk when unmet", () => {
  const secretName = DEMOS.find((d) => d.needsSecret)!.needsSecret!;
  const withSecret = (names: string[]) => baseStatus({ secrets: { present: names, github_app: false } });

  it("null status (pre-load) returns the FULL order — the ?step= initializer validates against it", () => {
    expect(stepOrder(null)).toEqual(STEP_ORDER);
  });

  it("drops the needsModel step without a model and the needsSecret steps without the secret", () => {
    const order = stepOrder(baseStatus());
    expect(order).not.toContain("agent-in-the-box");
    expect(order).not.toContain("key-never-in-the-box");
    expect(order).not.toContain("authorized-not-issued");
    // The unconditional demos, and everything outside the demos, always stay.
    expect(order).toContain("write-only-by-design");
    expect(order).toContain("record-a-policy");
    // A filter, not a re-order: what survives keeps STEP_ORDER's sequence.
    expect(order).toEqual(STEP_ORDER.filter((id) => order.includes(id)));
  });

  it("restores the granted secrets demos once the secret is stored", () => {
    const order = stepOrder(withSecret([secretName]));
    expect(order).toContain("key-never-in-the-box");
    expect(order).toContain("authorized-not-issued");
    // …and still not the harness demo: a secret is not a model.
    expect(order).not.toContain("agent-in-the-box");
  });

  it("a different secret name doesn't unlock them — the ref is by NAME", () => {
    expect(stepOrder(withSecret(["some-other-key"]))).not.toContain("key-never-in-the-box");
  });

  it("restores the harness demo once a model is connected", () => {
    const order = stepOrder(
      baseStatus({ providers: [{ tool: "claude", installed: true, logged_in: true, auth_mode: "subscription" }] }),
    );
    expect(order).toContain("agent-in-the-box");
  });
});

describe("corp_network gate — corpNetworkGate drives the footer (head/reason/action), the badge, and stepDone", () => {
  const unset: CorpNetworkState = {
    proxyConfigured: false,
    proxyDetected: false,
    redirectCount: 0,
    probeRunning: false,
    customDraft: "",
    redirectProbes: {},
  };
  const reached: ProxyTestResult = { state: "reached", detail: "Reached … — payloads matched", via: "proxy" };
  const reachedDirect: ProxyTestResult = { state: "reached", detail: "Reached … directly — payloads matched", via: "direct" };
  const blocked: ProxyTestResult = { state: "blocked", detail: "connection refused" };
  const intercepted: ProxyTestResult = { state: "blocked", detail: "something answered", intercepted: true };
  const customPass: ProxyTestResult = { state: "reached", detail: "The request completed", custom: true };
  const noRunner: ProxyTestResult = { state: "no_runner", detail: "no runner configured" };
  const bypass: ProxyTestResult = { state: "bypass", detail: "reachable directly too" };

  it("reads 'Untested'/off with the probe action before any probe has run (the default when corpNetwork is omitted)", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    expect(stepBadges(status, readiness, [], 0).corp_network).toEqual({ text: "Untested", tone: "warning" });
    expect(stepDone(status, readiness, [], 0).corp_network).toBe(false);
    expect(corpNetworkGate(unset, [])).toEqual({
      on: false,
      head: T.GATE_HEAD_UNTESTED,
      reason: T.GATE_UNTESTED,
      tone: "warning",
      action: { label: "Test connectivity", kind: "probe" },
    });
  });

  it("a probe in flight locks the gate with its own neutral head — and the badge reads Testing…", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    const corpNetwork = { ...unset, probeRunning: true };
    expect(corpNetworkGate(corpNetwork, [])).toEqual({ on: false, head: T.GATE_HEAD_RUNNING, reason: T.GATE_RUNNING, tone: "neutral" });
    expect(stepBadges(status, readiness, [], 0, corpNetwork).corp_network).toEqual({ text: "Testing…", tone: "info" });
  });

  it("a typed custom URL relabels the probe action to 'Test this URL' — the one launch point names what it will try", () => {
    const corpNetwork = { ...unset, proxyProbe: blocked, customDraft: "https://intranet.corp/health" };
    expect(corpNetworkGate(corpNetwork, []).action).toEqual({ label: "Test this URL", kind: "probe_custom" });
    // Whitespace alone is not a URL.
    expect(corpNetworkGate({ ...corpNetwork, customDraft: "   " }, []).action).toEqual({
      label: "Test connectivity",
      kind: "probe",
    });
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

  it("blocked locks with its head + sentence + the probe action", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    const corpNetwork = { ...unset, proxyProbe: blocked };
    expect(stepBadges(status, readiness, [], 0, corpNetwork).corp_network).toEqual({ text: "Blocked", tone: "warning" });
    expect(stepDone(status, readiness, [], 0, corpNetwork).corp_network).toBe(false);
    expect(corpNetworkGate(corpNetwork, [])).toEqual({
      on: false,
      head: T.GATE_HEAD_BLOCKED,
      reason: T.GATE_BLOCKED,
      tone: "warning",
      action: { label: "Test connectivity", kind: "probe" },
    });
  });

  it("intercepted is a blocked FLAVOR rendered apart — its own badge text, head, and instruction", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    const corpNetwork = { ...unset, proxyProbe: intercepted };
    expect(stepBadges(status, readiness, [], 0, corpNetwork).corp_network).toEqual({
      text: "Blocked · intercepted",
      tone: "warning",
    });
    expect(corpNetworkGate(corpNetwork, [])).toEqual({
      on: false,
      head: T.GATE_HEAD_INTERCEPTED,
      reason: T.GATE_INTERCEPTED,
      tone: "warning",
      action: { label: "Test connectivity", kind: "probe" },
    });
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
    expect(corpNetworkGate(corpNetwork, [])).toEqual({ on: true, head: T.GATE_HEAD_NORUNNER, reason: T.NORUNNER_NOTE, tone: "neutral" });
    // done = gate.on && reached (the mock's own rule): the operator may
    // continue, but the step never claims a proof Wardyn could not collect.
    expect(stepDone(status, readiness, [], 0, corpNetwork).corp_network).toBe(false);
    // The bypass holds even with unconfigured/untested redirects sitting there —
    // no_runner means NOTHING on this host can be probed, egress included.
    const redirects: EgressRedirect[] = [{ from: "https://registry.npmjs.org", to: "https://mirror.corp.internal" }];
    expect(corpNetworkGate(corpNetwork, redirects).on).toBe(true);
  });

  it("reached + zero redirects is DONE outright — no tab detour; nothing here must be configured", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    const corpNetwork = { ...unset, proxyProbe: reachedDirect };
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
    const corpNetwork = { ...unset, proxyProbe: customPass };
    expect(stepBadges(status, readiness, [], 0, corpNetwork).corp_network).toEqual({
      text: "Reached · custom endpoint",
      tone: "info",
    });
    expect(corpNetworkGate(corpNetwork, [])).toEqual({ on: true, head: T.GATE_HEAD_CUSTOM_ON, reason: T.GATE_CUSTOM_ON, tone: "neutral" });
    // A custom pass IS reached — it earns the checkmark; the tone and the
    // standing note carry the weaker footing.
    expect(stepDone(status, readiness, [], 0, corpNetwork).corp_network).toBe(true);
  });

  it("an untested redirect blocks with the prove-them sentence and a Test-the-redirect action (plural label at 2+)", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    const corpNetwork = { ...unset, proxyProbe: reached, redirectCount: 1 };
    const one: EgressRedirect[] = [{ from: "https://registry.npmjs.org", to: "https://mirror.corp.internal" }];
    expect(stepDone(status, readiness, [], 0, corpNetwork, one).corp_network).toBe(false);
    expect(corpNetworkGate(corpNetwork, one)).toEqual({
      on: false,
      head: T.GATE_HEAD_EGRESS_UNTESTED,
      reason: T.GATE_EGRESS_UNTESTED,
      tone: "warning",
      action: { label: "Test the redirect", kind: "test_redirects" },
    });
    const two: EgressRedirect[] = [...one, { from: "https://pypi.org/simple", to: "https://mirror.corp.internal/pypi" }];
    expect(corpNetworkGate(corpNetwork, two).action).toEqual({ label: "Test all redirects", kind: "test_redirects" });
  });

  it("failing rows are NAMED, all at once, with an Open-Egress action — a bypassed and a blocked redirect both count", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    const corpNetwork = {
      ...unset,
      proxyProbe: reached,
      redirectCount: 2,
      redirectProbes: { "https://registry.npmjs.org": bypass, "https://pypi.org/simple": blocked },
    };
    const redirects: EgressRedirect[] = [
      { from: "https://registry.npmjs.org", to: "https://mirror.corp.internal" },
      { from: "https://pypi.org/simple", to: "https://mirror.corp.internal/pypi" },
    ];
    expect(stepDone(status, readiness, [], 0, corpNetwork, redirects).corp_network).toBe(false);
    const g = corpNetworkGate(corpNetwork, redirects);
    expect(g.head).toBe(T.GATE_HEAD_EGRESS_FAILING);
    expect(g.reason).toBe(
      "Fix https://registry.npmjs.org and https://pypi.org/simple above — every configured redirect must prove reached before this step hands off. Or remove the rows.",
    );
    expect(g.action).toEqual({ label: "Open Egress redirection", kind: "open_egress" });
    // UI-SETUP-6: the badge counting configured (not proven) redirects would
    // read "Reached · proxy + 2 redirects" in success tone right beside a
    // gate that's locked on those exact two rows — an operator scanning the
    // rail (the rail's whole job) would conclude the opposite of the truth.
    // No "+ N redirects" clause at all until every configured row proves it.
    expect(stepBadges(status, readiness, [], 0, corpNetwork, redirects).corp_network).toEqual({
      text: "Reached · proxy",
      tone: "success",
    });
  });

  // UI-SETUP-13: off the egress tab, "Open Egress redirection" navigates
  // there and is worth offering. Already on it, the identical click does
  // nothing — no navigation, no state change — because the operator is
  // looking straight at the rows the reason names; the panel's own "Test
  // all"/remove/edit controls are the real recovery.
  it("the failing-redirect action drops out once the egress tab is already showing — no dead click", () => {
    const corpNetwork = {
      ...unset,
      proxyProbe: reached,
      redirectCount: 2,
      redirectProbes: { "https://registry.npmjs.org": bypass, "https://pypi.org/simple": blocked },
    };
    const redirects: EgressRedirect[] = [
      { from: "https://registry.npmjs.org", to: "https://mirror.corp.internal" },
      { from: "https://pypi.org/simple", to: "https://mirror.corp.internal/pypi" },
    ];
    const onEgress = corpNetworkGate(corpNetwork, redirects, "egress");
    expect(onEgress.head).toBe(T.GATE_HEAD_EGRESS_FAILING);
    expect(onEgress.action).toBeUndefined();
    // On the proxy tab (or the default, untouched call) the real fix-it
    // navigation still renders.
    expect(corpNetworkGate(corpNetwork, redirects, "proxy").action).toEqual({
      label: "Open Egress redirection",
      kind: "open_egress",
    });
    expect(corpNetworkGate(corpNetwork, redirects).action).toEqual({
      label: "Open Egress redirection",
      kind: "open_egress",
    });
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
      redirectCount: 4,
      redirectProbes: Object.fromEntries(froms.map((f) => [f, reached])),
    };
    expect(stepBadges(status, readiness, [], 0, corpNetwork, redirects).corp_network).toEqual({
      text: "Reached · proxy + 4 redirects",
      tone: "success",
    });
    expect(stepDone(status, readiness, [], 0, corpNetwork, redirects).corp_network).toBe(true);
  });
});

describe("review gate — the BARRIER is the only hard requirement; a model is optional", () => {
  it("barrier-only host (no model): review already claims launch-readiness", () => {
    // backend ready=true is barrier-only (setup.go); no model connected — and that
    // is enough: a plain governed run / interactive run needs no model.
    const status = baseStatus({ ready: true });
    const r = deriveReadiness(status);
    const badges = stepBadges(status, r, [], 0);
    expect(badges.review).toEqual({ text: "Ready to launch", tone: "success" });
    expect(stepDone(status, r, [], 0).review).toBe(true);
  });

  it("no barrier: review nudges to set up the barrier first (the one requirement)", () => {
    const status = baseStatus({ ready: false, runner: { driver: "none", confinement_classes: [] } });
    const r = deriveReadiness(status);
    const badges = stepBadges(status, r, [], 0);
    expect(badges.review).toEqual({ text: "Set up the barrier first", tone: "neutral" });
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
    expect(stepDone(status, r, [], 1).review).toBe(true);
  });

  it("integrations step is Optional (neutral), never a 'Needs setup' warning, with nothing connected", () => {
    const status = baseStatus({ ready: true });
    const r = deriveReadiness(status);
    expect(stepBadges(status, r, [], 0).integrations).toEqual({ text: "Optional", tone: "neutral" });
  });
});
