/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// B9 (#540, packet MP-D): a launch that fails after New Run has unmounted hands
// its failure back for the shell strip — the SERVER's sentence only, never this
// screen's own fallback.
import * as React from "react";
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, renderHook } from "@testing-library/react";
import type { PreflightResult } from "../../../lib/types";
import { MemoryRouter } from "react-router-dom";

const createRun = vi.fn();
vi.mock("../../../lib/api/runs", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../../lib/api/runs")>();
  return { ...actual, runs: { ...actual.runs, createRun: (...a: unknown[]) => createRun(...a) } };
});

import { useLaunch } from "./use-launch";
import { initialWizardState } from "./wizard-types";
import { HttpError } from "../../../lib/api/core";
import * as coreModule from "../../../lib/api/core";
import * as runsModule from "../../../lib/api/runs";

function mountLaunch() {
  return renderHook(
    () =>
      useLaunch({
        state: initialWizardState("CC1", { selectedPolicyId: "p1" }),
        workspaces: [],
        useSaved: true,
        ccTouched: false,
        merged: null,
        autoCheck: { local: false, backendArm: true, modelArm: true },
        doorOpen: false,
      }),
    { wrapper: ({ children }: { children: React.ReactNode }) => <MemoryRouter>{children}</MemoryRouter> },
  );
}

/** Launch, unmount the screen while the request is in flight, then fail it. */
async function failAfterUnmount(error: unknown): Promise<string | void> {
  let reject: (e: unknown) => void = () => {};
  createRun.mockReturnValue(new Promise((_, r) => (reject = r)));
  const { result, unmount } = mountLaunch();
  const pending = result.current.launch();
  unmount();
  reject(error);
  return pending;
}

beforeEach(() => {
  createRun.mockReset();
});

describe("useLaunch — a failure after the screen is gone (B9)", () => {
  it("hands back the server's sentence verbatim", async () => {
    const sentence = "This run's model provider is Corp gateway, and no token is available for it.";
    await expect(failAfterUnmount(new HttpError(422, sentence))).resolves.toBe(sentence);
  });

  it("hands back nothing when the server gave no sentence — never the client fallback", async () => {
    await expect(failAfterUnmount(new Error(""))).resolves.toBeUndefined();
  });

  it("while the screen is still mounted, returns nothing: the rail shows it", async () => {
    createRun.mockRejectedValue(new HttpError(422, "refused"));
    const { result } = mountLaunch();
    await expect(result.current.launch()).resolves.toBeUndefined();
  });
});

// pf-pf3: automatic preflight and the fresh-refusal block.
describe("useLaunch — automatic preflight", () => {
  type Auto = { local: boolean; backendArm: boolean; modelArm: boolean };
  const ON: Auto = { local: true, backendArm: true, modelArm: true };
  const preflightRun = vi.fn();
  const MS = 800;

  function mountAuto(init: { task: string; auto?: Auto; doorOpen?: boolean } = { task: "a" }) {
    return renderHook(
      (p: { task: string; auto: Auto; doorOpen: boolean }) =>
        useLaunch({
          state: initialWizardState("CC1", { selectedPolicyId: "p1", task: p.task }),
          workspaces: [],
          useSaved: true,
          ccTouched: false,
          merged: null,
          autoCheck: p.auto,
          doorOpen: p.doorOpen,
        }),
      {
        initialProps: { task: init.task, auto: init.auto ?? ON, doorOpen: !!init.doorOpen },
        wrapper: ({ children }: { children: React.ReactNode }) => <MemoryRouter>{children}</MemoryRouter>,
      },
    );
  }
  const tick = (ms: number) =>
    act(async () => {
      await vi.advanceTimersByTimeAsync(ms);
    });
  const setup = (kind: string, status: string): PreflightResult => ({
    enforced_confinement_class: "CC1",
    setup_items: [{ id: kind, kind, label: kind, required_by: "run", status }],
  });

  beforeEach(() => {
    vi.useFakeTimers();
    preflightRun.mockReset();
    preflightRun.mockResolvedValue(setup("backend", "ready"));
    vi.spyOn(runsModule.runs, "preflightRun").mockImplementation((...a) => preflightRun(...a));
    coreModule.setSignedOutHold(false);
  });
  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
    coreModule.setSignedOutHold(false);
  });

  it("a burst of edits sends one request, after the debounce", async () => {
    const { rerender } = mountAuto({ task: "a" });
    for (const t of ["ab", "abc", "abcd"]) {
      rerender({ task: t, auto: ON, doorOpen: false });
      await tick(200);
    }
    expect(preflightRun).not.toHaveBeenCalled();
    await tick(MS);
    expect(preflightRun).toHaveBeenCalledTimes(1);
  });

  it("an edit aborts the in-flight check and its answer is never shown", async () => {
    let resolve1: (r: PreflightResult) => void = () => {};
    preflightRun.mockImplementationOnce(() => new Promise((r) => (resolve1 = r)));
    const { result, rerender } = mountAuto({ task: "a" });
    await tick(MS);
    const signal = preflightRun.mock.calls[0][1] as AbortSignal;
    expect(signal.aborted).toBe(false);
    rerender({ task: "b", auto: ON, doorOpen: false });
    expect(signal.aborted).toBe(true);
    await act(async () => resolve1(setup("backend", "missing")));
    expect(result.current.preflightResult).toBeNull();
    expect(result.current.preflightBlock).toBe(false);
  });

  it("does not fire with a local problem, nor while the console is signed out", async () => {
    mountAuto({ task: "a", auto: { ...ON, local: false } });
    await tick(MS * 2);
    expect(preflightRun).not.toHaveBeenCalled();
    coreModule.setSignedOutHold(true);
    mountAuto({ task: "z" });
    await tick(MS * 2);
    expect(preflightRun).not.toHaveBeenCalled();
  });

  async function refused(status: number, reason: string) {
    preflightRun.mockRejectedValueOnce(new HttpError(status, "nope", reason));
    const m = mountAuto({ task: "a" });
    await tick(MS);
    return m;
  }

  it("a 4xx blocks Launch, expires at 60s, and a body change clears it", async () => {
    const { result, rerender } = await refused(422, "workspace_unavailable");
    expect(result.current.preflightBlock).toBe(true);
    await tick(58_000);
    expect(result.current.preflightBlock).toBe(true);
    await tick(2_500);
    expect(result.current.preflightBlock).toBe(false);
    preflightRun.mockRejectedValueOnce(new HttpError(422, "again", "x"));
    await tick(60_000);
    rerender({ task: "other", auto: ON, doorOpen: false });
    expect(result.current.preflightBlock).toBe(false);
  });

  it.each([
    ["model_credential", 422],
    ["host_capacity_refused", 503],
    ["preflight_rate_limited", 429],
  ])("%s (%i) never blocks", async (reason, status) => {
    const { result } = await refused(status, reason);
    expect(result.current.preflightBlock).toBe(false);
    if (status === 429) expect(result.current.preflightError).toBeNull();
    expect(result.current.preflightNotChecked).toBe(status === 429);
  });

  it.each(["backend", "llm_access"])("a missing %s row blocks, then expires at 60s", async (kind) => {
    preflightRun.mockResolvedValueOnce(setup(kind, "missing"));
    const { result } = mountAuto({ task: "a" });
    await tick(MS);
    expect(result.current.preflightBlock).toBe(true);
    await tick(60_500);
    expect(result.current.preflightBlock).toBe(false);
  });

  it("a focus re-check of a blocked body keeps Launch held while in flight, and re-checks a folded block", async () => {
    preflightRun.mockResolvedValueOnce(setup("backend", "missing"));
    const { result } = mountAuto({ task: "a" });
    await tick(MS);
    expect(result.current.preflightBlock).toBe(true);
    let resolve2: (r: PreflightResult) => void = () => {};
    preflightRun.mockImplementationOnce(() => new Promise((r) => (resolve2 = r)));
    act(() => {
      window.dispatchEvent(new Event("focus"));
    });
    await tick(MS);
    expect(preflightRun).toHaveBeenCalledTimes(2);
    expect(result.current.preflighting).toBe(true);
    expect(result.current.preflightBlock).toBe(true);
    await act(async () => resolve2(setup("backend", "ready")));
    expect(result.current.preflightBlock).toBe(false);
  });

  it("a focus during an in-flight check does not double-send", async () => {
    preflightRun.mockImplementationOnce(() => new Promise(() => {}));
    mountAuto({ task: "a" });
    await tick(MS);
    act(() => {
      window.dispatchEvent(new Event("focus"));
    });
    await tick(MS);
    expect(preflightRun).toHaveBeenCalledTimes(1);
  });

  it("re-checks after the sign-in door closes", async () => {
    const { rerender } = mountAuto({ task: "a", doorOpen: true });
    await tick(MS);
    const n = preflightRun.mock.calls.length;
    rerender({ task: "a", auto: ON, doorOpen: false });
    await tick(MS);
    expect(preflightRun.mock.calls.length).toBe(n + 1);
  });
});
