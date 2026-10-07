/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { runs } from "../../../lib/api/runs";
import { HttpError, setSignedOutHold } from "../../../lib/api/core";
import type { PreflightResult } from "../../../lib/types";
import { makeWorkspace } from "../../../../test/factories";
import { useLaunch, PREFLIGHT_DEBOUNCE_MS, type UseLaunchParams } from "./use-launch";
import { initialWizardState } from "./wizard-types";
import { mergeRunSelections } from "./wizard-spec";

const workspaces = [
  makeWorkspace({ id: "ws-a", kind: "local_dir", source: "/data/a" }),
  makeWorkspace({ id: "ws-b", kind: "repo", source: "team/b" }),
  makeWorkspace({ id: "ws-c", kind: "local_dir", source: "/data/c" }),
];
const ready: PreflightResult = { enforced_confinement_class: "CC1", setup_items: [] };
const createRun = vi.fn();
const preflightRun = vi.fn();

function params(over: Partial<UseLaunchParams> = {}): UseLaunchParams {
  const state = over.state ?? initialWizardState("CC1", {
    selectedPolicyId: "p1",
    workspaces: [{ workspaceId: "ws-a" }],
  });
  return {
    state,
    workspaces,
    policyMode: "saved",
    ccTouched: false,
    merged: mergeRunSelections({ allowed_domains: [], first_use_approval: "deny_with_review", min_confinement_class: "CC1" }, state, workspaces),
    autoCheck: { local: true, backendArm: true, modelArm: true },
    doorOpen: false,
    ...over,
  };
}

function mount(initialProps = params()) {
  return renderHook((p: UseLaunchParams) => useLaunch(p), {
    initialProps,
    wrapper: ({ children }: { children: React.ReactNode }) => <MemoryRouter>{children}</MemoryRouter>,
  });
}

const tick = () => act(() => vi.advanceTimersByTimeAsync(PREFLIGHT_DEBOUNCE_MS));

beforeEach(() => {
  vi.useFakeTimers();
  createRun.mockReset().mockResolvedValue({ id: "run-1" });
  preflightRun.mockReset().mockResolvedValue(ready);
  vi.spyOn(runs, "createRun").mockImplementation((...args) => createRun(...args));
  vi.spyOn(runs, "preflightRun").mockImplementation((...args) => preflightRun(...args));
  setSignedOutHold(false);
});

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
  setSignedOutHold(false);
});

describe("useLaunch — reference policy attachments", () => {
  it.each(["default", "saved"] as const)("%s refuses launch and both checks with extra attachments", async (policyMode) => {
    const state = initialWizardState("CC1", {
      selectedPolicyId: "p1",
      workspaces: [{ workspaceId: "ws-a" }, { workspaceId: "ws-b" }],
    });
    const original = structuredClone(state);
    const { result } = mount(params({ policyMode, state }));
    expect(result.current.currentBody).toBeNull();
    await act(() => result.current.launch());
    await act(() => result.current.preflight());
    await tick();
    expect(createRun).not.toHaveBeenCalled();
    expect(preflightRun).not.toHaveBeenCalled();
    expect(result.current.launching).toBe(false);
    expect(result.current.preflighting).toBe(false);
    expect(result.current.error).toBeNull();
    expect(result.current.preflightError).toBeNull();
    expect(state).toEqual(original);
  });

  it("a missing saved selection never submits the dormant custom document", async () => {
    const { result } = mount(params({ state: initialWizardState() }));
    expect(result.current.currentBody).toBeNull();
    await act(() => result.current.launch());
    await act(() => result.current.preflight());
    await tick();
    expect(createRun).not.toHaveBeenCalled();
    expect(preflightRun).not.toHaveBeenCalled();
  });

  it("a manual check of an invalid custom document resolves without getting stuck busy", async () => {
    const { result } = mount(params({ policyMode: "custom", merged: null }));
    await act(async () => {
      await expect(result.current.preflight()).resolves.toBeUndefined();
    });
    await tick();
    expect(preflightRun).not.toHaveBeenCalled();
    expect(result.current.preflighting).toBe(false);
    expect(result.current.preflightError).toBeNull();
  });

  it.each(["default", "saved"] as const)("switching %s to custom retains every attachment and ignores the dormant saved ID", async (policyMode) => {
    const state = initialWizardState("CC1", {
      selectedPolicyId: "p1",
      workspaces: [{ workspaceId: "ws-a", readOnly: true }, { workspaceId: "ws-b", enabledOptional: ["read:team/b"] }],
    });
    const { result, rerender } = mount(params({ policyMode, state }));
    await tick();
    expect(preflightRun).not.toHaveBeenCalled();
    rerender(params({ policyMode: "custom", state }));
    await act(() => result.current.preflight());
    await act(() => result.current.launch());
    const body = createRun.mock.calls[0][0] as Record<string, unknown>;
    expect(body).toEqual(preflightRun.mock.calls[0][0]);
    expect(body).not.toHaveProperty("policy_id");
    expect(body.inline_policy).toMatchObject({
      workspace_mounts: [{ source: "/data/a", read_only: true }],
      workspace_repos: [{ repo: "team/b" }],
    });
    expect(body.workspaces).toEqual([
      { workspace_id: "ws-a", read_only: true },
      { workspace_id: "ws-b", enabled_optional: ["read:team/b"] },
    ]);
  });
});

describe("useLaunch — attachment and mode request ownership", () => {
  it.each(["success", "refusal"] as const)("an old %s cannot return after a blocked attachment round trip", async (answer) => {
    let resolve!: (value: PreflightResult) => void;
    let reject!: (error: unknown) => void;
    preflightRun.mockImplementationOnce(() => new Promise<PreflightResult>((yes, no) => {
      resolve = yes;
      reject = no;
    }));
    const original = params();
    const { result, rerender } = mount(original);
    await tick();
    const signal = preflightRun.mock.calls[0][1] as AbortSignal;
    rerender(params({ state: { ...original.state, workspaces: [{ workspaceId: "ws-a" }, { workspaceId: "ws-b" }] } }));
    expect(signal.aborted).toBe(true);
    expect(result.current.currentBody).toBeNull();
    expect(result.current.preflighting).toBe(false);
    await tick();
    expect(preflightRun).toHaveBeenCalledTimes(1);
    rerender(original);
    await tick();
    expect(preflightRun).toHaveBeenCalledTimes(2);
    expect(result.current.preflightResult).toEqual(ready);
    await act(async () => {
      if (answer === "success") resolve({ ...ready, enforced_confinement_class: "CC3" });
      else reject(new HttpError(422, "old refusal", "model_credential"));
    });
    expect(result.current.preflightResult).toEqual(ready);
    expect(result.current.preflightError).toBeNull();
    expect(result.current.preflightRefusal).toBeNull();
    expect(result.current.preflightBlock).toBe(false);
  });

  it("changing reference modes invalidates a pending check even after returning to the same body", async () => {
    let settle!: (value: PreflightResult) => void;
    preflightRun.mockImplementationOnce(() => new Promise<PreflightResult>((resolve) => { settle = resolve; }));
    const original = params();
    const { result, rerender } = mount(original);
    await tick();
    const signal = preflightRun.mock.calls[0][1] as AbortSignal;
    rerender(params({ policyMode: "default" }));
    rerender(original);
    expect(signal.aborted).toBe(true);
    await act(async () => settle({ ...ready, enforced_confinement_class: "CC3" }));
    expect(result.current.preflightIsCurrent).toBe(false);
    expect(result.current.preflightResult).toBeNull();
    await tick();
    expect(preflightRun).toHaveBeenCalledTimes(2);
    expect(result.current.preflightResult).toEqual(ready);
  });

  it.each(["mode", "attachments"] as const)("a completed verdict does not revive after a %s round trip", async (change) => {
    const original = params();
    const { result, rerender } = mount(original);
    await tick();
    expect(result.current.preflightIsCurrent).toBe(true);
    rerender(change === "mode"
      ? params({ policyMode: "default" })
      : params({ state: { ...original.state, workspaces: [{ workspaceId: "ws-a" }, { workspaceId: "ws-b" }] } }));
    expect(result.current.preflightIsCurrent).toBe(false);
    rerender(original);
    expect(result.current.preflightIsCurrent).toBe(false);
    expect(result.current.preflightFresh).toBe(false);
    expect(result.current.preflightRefusal).toBeNull();
  });

  it("a body-equivalent attachment change aborts the old check and schedules a fresh one", async () => {
    let settle!: (value: PreflightResult) => void;
    preflightRun.mockImplementationOnce(() => new Promise<PreflightResult>((resolve) => { settle = resolve; }));
    const original = params();
    const { result, rerender } = mount(original);
    const body = result.current.currentBody;
    await tick();
    const signal = preflightRun.mock.calls[0][1] as AbortSignal;
    rerender(params({ state: { ...original.state, workspaces: [{ workspaceId: "ws-a", target: "/other" }] } }));
    expect(result.current.currentBody).toEqual(body);
    expect(signal.aborted).toBe(true);
    await tick();
    expect(preflightRun).toHaveBeenCalledTimes(2);
    await act(async () => settle({ ...ready, enforced_confinement_class: "CC3" }));
    expect(result.current.preflightResult).toEqual(ready);
  });
});
