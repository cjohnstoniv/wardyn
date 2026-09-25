/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// F13 (#1065): buildSpec — a governed command whose target already carries a
// real base image needs no agent. Split into its own file (not appended to
// wizard-spec.test.ts, which sits at the 1000-line file-size cap — see
// scripts/check-file-size.sh) rather than trimmed to fit.
import { describe, it, expect } from "vitest";
import { buildSpec, initialWizardState } from "./wizard-types";
import { makeWorkspace } from "../../../../test/factories";

// A command run whose target already carries a real base image — an
// explicit BYOI image, or a selected workspace with one — needs no agent:
// task_mode=exec runs no harness, so naming one there was a hidden formality
// that also fed the managed-subscription eligibility test for a harness the
// run never starts (agentRequirementError's own doc, server-side). Adopted
// from the independent 0.8 review's console command-form case
// (review-tests-rerun.patch), which found `agent: claude-code` still riding
// the exec body.
describe("buildSpec — a governed command omits agent when its target already carries a base image", () => {
  it("omits agent for a command run on an image-backed workspace", () => {
    const ws = makeWorkspace({ id: "ws-1", base_image: { kind: "registry", image: "ubuntu:24.04" } });
    const { run } = buildSpec(
      { ...initialWizardState(), runType: "command", task: "echo hi", workspaces: [{ workspaceId: "ws-1" }] },
      [ws],
    );
    expect(run.task_mode).toBe("exec");
    expect(run.agent).toBeUndefined();
  });

  it("omits agent for a command run naming an explicit BYOI image", () => {
    const { run } = buildSpec({ ...initialWizardState(), runType: "command", task: "echo hi", image: "ubuntu:24.04" });
    expect(run.agent).toBeUndefined();
  });

  it("still sends agent for a command run with no image and no image-backed workspace", () => {
    const { run } = buildSpec({ ...initialWizardState(), runType: "command", task: "echo hi" });
    expect(run.agent).toBe(initialWizardState().agent);
  });

  it("still sends agent for a command run on a workspace with no base image (recommended default)", () => {
    const ws = makeWorkspace({ id: "ws-1", base_image: { kind: "recommended" } });
    const { run } = buildSpec(
      { ...initialWizardState(), runType: "command", task: "echo hi", workspaces: [{ workspaceId: "ws-1" }] },
      [ws],
    );
    expect(run.agent).toBe(initialWizardState().agent);
  });

  it("still sends agent for an ordinary agent run, even with an image-backed workspace selected", () => {
    const ws = makeWorkspace({ id: "ws-1", base_image: { kind: "registry", image: "ubuntu:24.04" } });
    const { run } = buildSpec(
      { ...initialWizardState(), runType: "agent", workspaces: [{ workspaceId: "ws-1" }] },
      [ws],
    );
    expect(run.agent).toBe(initialWizardState().agent);
  });
});
