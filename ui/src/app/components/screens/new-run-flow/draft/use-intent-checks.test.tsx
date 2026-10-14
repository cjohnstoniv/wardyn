/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { getAuthGeneration, HttpError, setSignedOutHold } from "../../../../lib/api/core";
import { runs } from "../../../../lib/api/runs";
import type { PreflightResult } from "../../../../lib/types";
import type { PolicyPreviewResult } from "../../../../lib/types/policy-preview";
import { intentToWire } from "./intent-to-wire";
import { launchGateOpen, PREFLIGHT_FRESH_MS } from "./launch-gate";
import { useIntentChecks } from "./use-intent-checks";

const preflight = vi.fn(), preview = vi.fn();
const ready: PreflightResult = { enforced_confinement_class: "CC1", setup_items: [] };
const effective: PolicyPreviewResult = {
  spec: { allowed_domains: [], first_use_approval: "deny_with_review", min_confinement_class: "CC1" }, source: { kind: "inline" },
  provisional: true, redacted: false, warnings: [], pending: [], repository_access: [],
};
const tick = (ms = 800) => act(() => vi.advanceTimersByTimeAsync(ms));
const BODY = intentToWire({ shape: "agent_task", ends_after_sec: 600, inline_policy: { allowed_domains: [] } }).body;
type Params = Parameters<typeof useIntentChecks>[0];
const params = (over: Partial<Params> = {}): Params => ({
  body: BODY, selectionKey: "custom",
  identity: { principal: "alice", resolved: true, revision: 1, authGeneration: getAuthGeneration() },
  externalRevision: "1", sourceRefreshPending: false,
  autoCheck: { local: true, backendArm: true, modelArm: true }, doorOpen: false, adoDoorOpen: false,
  ...over,
});
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((yes) => { resolve = yes; });
  return { promise, resolve };
}

beforeEach(() => {
  vi.useFakeTimers();
  preflight.mockReset().mockResolvedValue(ready);
  preview.mockReset().mockResolvedValue(effective);
  vi.spyOn(runs, "preflightRunWire").mockImplementation(preflight);
  vi.spyOn(runs, "previewRunPolicyWire").mockImplementation(preview);
  setSignedOutHold(false);
});
afterEach(() => { vi.restoreAllMocks(); vi.useRealTimers(); });

it("sends the serialized body itself to both reads, after the debounce", async () => {
  renderHook(useIntentChecks, { initialProps: params() });
  await tick(799);
  expect(preflight).not.toHaveBeenCalled();
  await tick(1);
  expect(preflight).toHaveBeenCalledWith(BODY, expect.any(AbortSignal));
  expect(preview).toHaveBeenCalledWith(BODY, expect.any(AbortSignal));
  expect(preflight.mock.calls[0][0]).toMatchObject({ ends_after_sec: 600, policy_overlay: { allowed_domains: [] }, mode_contract: "0.9" });
});

it("sends nothing for a draft with local issues (a null body)", async () => {
  renderHook(useIntentChecks, { initialProps: params({ body: null }) });
  await tick(5000);
  expect(preflight).not.toHaveBeenCalled();
  expect(preview).not.toHaveBeenCalled();
});

it("dedupes an equal body: a new object with the same content keeps its generation and reads nothing again", async () => {
  const p = params();
  const { result, rerender } = renderHook(useIntentChecks, { initialProps: p });
  await tick();
  const generation = result.current.generation;
  rerender({ ...p, body: { ...BODY } });
  await tick();
  expect(result.current.generation).toBe(generation);
  expect(preflight).toHaveBeenCalledTimes(1);
  expect(result.current.preflightRead).toMatchObject({ generation });
});

it("a changed body is a new generation: the old answer no longer counts and Launch stays shut until the new one", async () => {
  const p = params();
  const { result, rerender } = renderHook(useIntentChecks, { initialProps: p });
  await tick();
  const first = result.current.generation;
  expect(result.current.preflightRead?.generation).toBe(first);
  const next = deferred<PreflightResult>();
  preflight.mockReturnValueOnce(next.promise);
  rerender({ ...p, body: { ...BODY, no_end: true } });
  expect(result.current.generation).toBe(first + 1);
  expect(result.current.preflightRead).toBeNull();
  const gate = () => launchGateOpen({ blockers: 0, preflight: result.current.preflightRead, generation: result.current.generation, now: Date.now() });
  expect(gate()).toBe(false);
  await tick();
  await act(async () => next.resolve(ready));
  expect(result.current.preflightRead?.generation).toBe(first + 1);
  expect(gate()).toBe(true);
});

it("a model credential refusal is an answer that leaves the gate open; any other refusal is none", async () => {
  preflight.mockRejectedValueOnce(new HttpError(422, "Your AWS sign-in no longer works.", "model_credential"));
  const p = params();
  const { result, rerender } = renderHook(useIntentChecks, { initialProps: p });
  const gate = () => launchGateOpen({ blockers: 0, preflight: result.current.preflightRead, generation: result.current.generation, now: Date.now() });
  await tick();
  expect(result.current.preflightRefusal).not.toBeNull();
  expect(gate()).toBe(true);
  preflight.mockRejectedValueOnce(new HttpError(422, "The namespace quota is spent.", "namespace_quota_exceeded"));
  rerender({ ...p, body: { ...BODY, no_end: true } });
  await tick();
  expect(result.current.preflightError).toBe("The namespace quota is spent.");
  expect(gate()).toBe(false);
});

it("drops a late answer for a body that is no longer the draft's", async () => {
  const late = deferred<PreflightResult>();
  preflight.mockReturnValueOnce(late.promise);
  const p = params();
  const { result, rerender } = renderHook(useIntentChecks, { initialProps: p });
  await tick();
  rerender({ ...p, body: { ...BODY, no_end: true } });
  await act(async () => late.resolve({ ...ready, enforced_confinement_class: "CC3" }));
  expect(result.current.preflightResult).toBeNull();
  expect(result.current.preflightRead).toBeNull();
});

it("an answer is fresh for under a minute, then the gate shuts without a new read", async () => {
  const { result } = renderHook(useIntentChecks, { initialProps: params() });
  await tick();
  expect(result.current.preflightFresh).toBe(true);
  const read = result.current.preflightRead;
  const gate = (now: number) => launchGateOpen({ blockers: 0, preflight: read, generation: result.current.generation, now });
  expect(gate(Date.now())).toBe(true);
  await tick(PREFLIGHT_FRESH_MS + 1);
  expect(result.current.preflightFresh).toBe(false);
  expect(gate(Date.now())).toBe(false);
  expect(preflight).toHaveBeenCalledTimes(1);
});
