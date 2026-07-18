/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import type { SetupStatus, Workspace, WorkspaceStatus } from "../../../lib/types";
import { deriveReadiness } from "../onboarding/intro";
import {
  DEMO_STEP_IDS,
  PHASES,
  STEP_HEADING,
  STEP_LABEL,
  STEP_ORDER,
  siteConfigBadge,
  stepBadges,
  stepDone,
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

describe("credentials — always Optional, never done (honesty law)", () => {
  it("stays Optional/false even when a git credential is present", () => {
    const status = baseStatus({ secrets: { present: ["git-pat-github-com"], github_app: true } });
    const readiness = deriveReadiness(status);
    expect(stepBadges(status, readiness, [], null).credentials).toEqual({
      text: "Optional",
      tone: "neutral",
    });
    expect(stepDone(status, readiness, [], null).credentials).toBe(false);
  });
});

describe("demos — advisory in the pure fns (per-demo checkmark is applied at the orchestrator)", () => {
  it("each demo step is Optional/false regardless of runs — the launched signal is per-browser, not in status", () => {
    const status = baseStatus({ has_runs: true });
    const readiness = deriveReadiness(status);
    for (const id of DEMO_STEP_IDS) {
      expect(stepBadges(status, readiness, [], null)[id]).toEqual({ text: "Optional", tone: "neutral" });
      expect(stepDone(status, readiness, [], null)[id]).toBe(false);
    }
  });
});

describe("environment badge", () => {
  it("reads amber 'Needs setup' (not ready-toned) when zero barriers are ready", () => {
    const status = baseStatus({ runner: { driver: "docker", confinement_classes: [] } });
    const readiness = deriveReadiness(status);
    expect(stepBadges(status, readiness, [], null).environment).toEqual({
      text: "Needs setup",
      tone: "warning",
    });
    expect(stepDone(status, readiness, [], null).environment).toBe(false);
  });
});

describe("workspaces badge", () => {
  it("shows 'Ready · 2 onboarded' and done=true with two ready workspaces", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    const workspaces = [ws("w1", "ready"), ws("w2", "ready")];
    expect(stepBadges(status, readiness, workspaces, null).workspaces).toEqual({
      text: "Ready · 2 onboarded",
      tone: "success",
    });
    expect(stepDone(status, readiness, workspaces, null).workspaces).toBe(true);
  });

  it("shows an info-tone 'In progress' and done=false with only a pending workspace", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    const workspaces = [ws("w1", "pending_scan")];
    expect(stepBadges(status, readiness, workspaces, null).workspaces).toEqual({
      text: "In progress",
      tone: "info",
    });
    expect(stepDone(status, readiness, workspaces, null).workspaces).toBe(false);
  });

  it("shows 'Optional' with no workspaces at all", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    expect(stepBadges(status, readiness, [], null).workspaces).toEqual({
      text: "Optional",
      tone: "neutral",
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
    expect(stepBadges(status, readiness, [], null).environment.tone).toBe("success");
    expect(stepDone(status, readiness, [], null).environment).toBe(true);
    // review still rolls the failure up.
    expect(stepDone(status, readiness, [], null).review).toBe(false);
  });
});

describe("corporate done — same SiteConfig predicate as the badge", () => {
  it("badge and rail dot agree: done flips exactly when the step's field is set", () => {
    const status = baseStatus();
    const r = deriveReadiness(status);
    expect(stepDone(status, r, [], null).host_proxy).toBe(false);
    expect(stepDone(status, r, [], null).scm_provider).toBe(false);
    expect(stepDone(status, r, [], null).artifact_repo).toBe(false);

    const cfg = {
      upstream_proxy_secret_ref: "corp-proxy",
      scm_hosts: ["github.example.com"],
      artifact_overrides: { npm: { base_url: "https://artifactory.example.com/npm" } },
    };
    const done = stepDone(status, r, [], cfg);
    expect(done.host_proxy).toBe(true);
    expect(done.scm_provider).toBe(true);
    expect(done.artifact_repo).toBe(true);
    expect(siteConfigBadge(cfg, "host_proxy").text).toBe("Configured");
  });
});

describe("host_proxy — Configured only once the SiteConfig field is set", () => {
  it("reads Optional with no SiteConfig", () => {
    expect(siteConfigBadge(null, "host_proxy")).toEqual({ text: "Optional", tone: "neutral" });
    expect(siteConfigBadge({}, "host_proxy")).toEqual({ text: "Optional", tone: "neutral" });
  });

  it("reads Configured once upstream_proxy_secret_ref is set", () => {
    expect(siteConfigBadge({ upstream_proxy_secret_ref: "corp-proxy" }, "host_proxy")).toEqual({
      text: "Configured",
      tone: "success",
    });
  });
});

describe("frozen contract — ids, labels, headings, order", () => {
  it("pins the frozen step ids and labels (e2e clicks `Next: {label}`)", () => {
    expect(Object.entries(STEP_LABEL)).toEqual([
      ["environment", "Environment"],
      ["provider", "Model/Harness Provider"],
      // The four Demos sub-steps — labels come from the demo catalog titles.
      ["sealed-box", "The sealed box"],
      ["fail-then-approve", "Fail, then approve"],
      ["held-at-the-door", "Held at the door"],
      ["lines-that-cant-be-crossed", "Lines that can't be crossed"],
      ["host_proxy", "Host Proxy"],
      ["scm_provider", "SCM Provider"],
      ["artifact_repo", "Artifact Redirect"],
      ["workspaces", "Workspaces"],
      ["credentials", "Credentials"],
      ["review", "Review"],
      ["launch", "Launch"],
    ]);
    expect(STEP_HEADING.environment).toBe("Pick your barrier");
  });

  it("pins STEP_ORDER to the phase walk (demos + corporate network before your work)", () => {
    expect(STEP_ORDER).toEqual([
      "environment",
      "provider",
      "sealed-box",
      "fail-then-approve",
      "held-at-the-door",
      "lines-that-cant-be-crossed",
      "host_proxy",
      "artifact_repo",
      "scm_provider",
      "workspaces",
      "credentials",
      "review",
      "launch",
    ]);
    expect(PHASES.flatMap((p) => p.steps)).toEqual(STEP_ORDER);
    // The four Demos sub-steps ARE the demos phase, in catalog order.
    expect(PHASES.find((p) => p.id === "demos")?.steps).toEqual([...DEMO_STEP_IDS]);
  });
});

describe("workspaces badge — honest partial counts", () => {
  it("reads 'Ready · 1 of 2 onboarded' when one of two is still importing", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    const workspaces = [ws("a", "ready"), ws("b", "pending_scan")];
    expect(stepBadges(status, readiness, workspaces, null).workspaces).toEqual({
      text: "Ready · 1 of 2 onboarded",
      tone: "success",
    });
  });
});

describe("review/launch essentials gate — barrier alone never claims launch-readiness", () => {
  it("barrier-only host: review 'Review what's left', launch 'Set up the essentials first'", () => {
    // backend ready=true is barrier-only (setup.go); no model connected.
    const status = baseStatus({ ready: true });
    const r = deriveReadiness(status);
    const badges = stepBadges(status, r, [], null);
    expect(badges.review).toEqual({ text: "Review what's left", tone: "neutral" });
    expect(badges.launch).toEqual({ text: "Set up the essentials first", tone: "neutral" });
    expect(stepDone(status, r, [], null).review).toBe(false);
  });

  it("barrier + connected model: the success texts + review done", () => {
    const status = baseStatus({
      ready: true,
      providers: [{ tool: "claude", installed: true, logged_in: true }],
    });
    const r = deriveReadiness(status);
    const badges = stepBadges(status, r, [], null);
    expect(badges.review).toEqual({ text: "All essentials ready", tone: "success" });
    expect(badges.launch).toEqual({ text: "Ready to launch", tone: "success" });
    expect(stepDone(status, r, [], null).review).toBe(true);
  });
});
