/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import type { SetupStatus, Workspace, WorkspaceStatus } from "../../../lib/types";
import { deriveReadiness } from "../onboarding/intro";
import {
  PHASES,
  SETUP_STEPS,
  STEP_HEADING,
  STEP_ORDER,
  siteConfigBadge,
  stepBadges,
  stepDone,
} from "./steps";

// Minimal SetupStatus fixture — shape copied from setup-screen.test.tsx's
// baseStatus(), trimmed to only what these pure functions read.
function baseStatus(overrides: Partial<SetupStatus> = {}): SetupStatus {
  return {
    ready: false,
    checks: [],
    auth: { mode: "local", local_loopback: true },
    runner: { driver: "docker", confinement_classes: ["CC1"] },
    composer: { enabled: false, backends: [] },
    providers: [{ tool: "claude", installed: true, logged_in: false }],
    secrets: { present: [], github_app: false },
    age_key: { durable: false },
    has_runs: false,
    platform: { os: "linux", wsl: false, kvm: true },
    ...overrides,
  };
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
    expect(stepDone(status, readiness, []).credentials).toBe(false);
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
    expect(stepDone(status, readiness, []).environment).toBe(false);
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
    expect(stepDone(status, readiness, workspaces).workspaces).toBe(true);
  });

  it("shows an info-tone 'In progress' and done=false with only a pending workspace", () => {
    const status = baseStatus();
    const readiness = deriveReadiness(status);
    const workspaces = [ws("w1", "pending_scan")];
    expect(stepBadges(status, readiness, workspaces, null).workspaces).toEqual({
      text: "In progress",
      tone: "info",
    });
    expect(stepDone(status, readiness, workspaces).workspaces).toBe(false);
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
    expect(SETUP_STEPS.map((s) => [s.id, s.label])).toEqual([
      ["environment", "Environment"],
      ["provider", "Model/Harness Provider"],
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

  it("pins STEP_ORDER to the phase walk (scm_provider follows artifact_repo)", () => {
    expect(STEP_ORDER).toEqual([
      "environment",
      "provider",
      "host_proxy",
      "artifact_repo",
      "scm_provider",
      "workspaces",
      "credentials",
      "review",
      "launch",
    ]);
    expect(PHASES.flatMap((p) => p.steps)).toEqual(STEP_ORDER);
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
    expect(stepDone(status, r, []).review).toBe(false);
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
    expect(stepDone(status, r, []).review).toBe(true);
  });
});
