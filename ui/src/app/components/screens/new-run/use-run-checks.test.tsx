/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { getAuthGeneration, HttpError, notifyAuthChange, setSignedOutHold } from "../../../lib/api/core";
import { runs } from "../../../lib/api/runs";
import * as previewApi from "../../../lib/api/policy-preview";
import type { PolicyPreviewResult } from "../../../lib/types/policy-preview";
import { retryAfterDelay, useRunChecks } from "./use-run-checks";

const preflight = vi.fn(), preview = vi.fn();
const ready = { enforced_confinement_class: "CC1", setup_items: [] };
const effective: PolicyPreviewResult = { spec: { allowed_domains: [], first_use_approval: "deny_with_review", min_confinement_class: "CC1" }, source: { kind: "inline" }, provisional: true, redacted: false, warnings: [], pending: ["credential_liveness"], repository_access: [] };
const tick = (ms = 800) => act(() => vi.advanceTimersByTimeAsync(ms));
type Params = Parameters<typeof useRunChecks>[0];
function params(over: Partial<Params> = {}): Params {
  return {
    input: { task: "task", inline_policy: effective.spec }, body: "body", selectionKey: "custom",
    identity: { principal: "alice", resolved: true, revision: 1, authGeneration: getAuthGeneration() },
    externalRevision: "1", sourceRefreshPending: false,
    autoCheck: { local: true, backendArm: true, modelArm: true }, doorOpen: false, adoDoorOpen: false,
    ...over,
  };
}
function deferred<T>() {
  let resolve!: (value: T) => void, reject!: (error: unknown) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

beforeEach(() => {
  vi.useFakeTimers();
  preflight.mockReset().mockResolvedValue(ready);
  preview.mockReset().mockResolvedValue(effective);
  vi.spyOn(runs, "preflightRun").mockImplementation(preflight);
  vi.spyOn(previewApi, "previewRunPolicy").mockImplementation(preview);
  setSignedOutHold(false);
});
afterEach(() => { vi.restoreAllMocks(); vi.useRealTimers(); setSignedOutHold(false); });

it("coalesces a burst into one pair and does not read again for equivalent authored bytes", async () => {
  const p = params();
  const { rerender } = renderHook(useRunChecks, { initialProps: p });
  await tick(400);
  rerender({ ...p, body: "edited" });
  await tick(799);
  expect(preview).not.toHaveBeenCalled();
  await tick(1);
  expect(preview).toHaveBeenCalledTimes(1);
  expect(preflight).toHaveBeenCalledTimes(1);
  rerender({ ...p, body: "edited" });
  await tick();
  expect(preview).toHaveBeenCalledTimes(1);
  expect(preflight).toHaveBeenCalledTimes(1);
});

it("previews a locally blocked draft and only preflights when the gate opens", async () => {
  const p = params({ autoCheck: { local: false, backendArm: true, modelArm: true } });
  const { result, rerender } = renderHook(useRunChecks, { initialProps: p });
  await tick();
  expect(preview).toHaveBeenCalledTimes(1);
  expect(preflight).not.toHaveBeenCalled();
  expect(result.current.preview.result?.pending).toEqual(["credential_liveness"]);
  rerender({ ...p, autoCheck: { ...p.autoCheck, local: true } });
  await tick();
  expect(preview).toHaveBeenCalledTimes(1);
  expect(preflight).toHaveBeenCalledTimes(1);
});

it("manual preflight consumes queued automatic work without a second pair", async () => {
  const { result } = renderHook(useRunChecks, { initialProps: params() });
  await act(() => result.current.preflight());
  expect(preview).toHaveBeenCalledTimes(1);
  expect(preflight).toHaveBeenCalledTimes(1);
  await tick();
  expect(preview).toHaveBeenCalledTimes(1);
  expect(preflight).toHaveBeenCalledTimes(1);
});

it("ignores abort-resistant answers and finally handlers across a body round trip", async () => {
  const first = deferred<PolicyPreviewResult>(), second = deferred<PolicyPreviewResult>();
  preview.mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise);
  const p = params();
  const { result, rerender } = renderHook(useRunChecks, { initialProps: p });
  await tick();
  const signal = preview.mock.calls[0][1] as AbortSignal;
  rerender({ ...p, body: "other" });
  rerender(p);
  expect(result.current.preview.current).toBe(false);
  expect(signal.aborted).toBe(true);
  await tick();
  await act(async () => first.resolve(effective));
  expect(result.current.preview.busy).toBe(true);
  expect(result.current.preview.result).toBeNull();
  await act(async () => second.resolve({ ...effective, warnings: ["new answer"] }));
  expect(result.current.preview.result?.warnings).toEqual(["new answer"]);
});

it("does not poll on expiry or fresh focus; stale focus coalesces once", async () => {
  const { result } = renderHook(useRunChecks, { initialProps: params() });
  await tick();
  act(() => window.dispatchEvent(new Event("focus")));
  await tick();
  expect(preview).toHaveBeenCalledTimes(1);
  await tick(60_000);
  expect(result.current.preview.fresh).toBe(false);
  expect(preview).toHaveBeenCalledTimes(1);
  act(() => { window.dispatchEvent(new Event("focus")); window.dispatchEvent(new Event("focus")); });
  await tick();
  expect(preview).toHaveBeenCalledTimes(2);
  expect(preflight).toHaveBeenCalledTimes(2);
});

it("retries preview once after Retry-After without repeating preflight, then requires an explicit retry", async () => {
  preview.mockRejectedValue(new HttpError(429, "limited", "", "", "", "", undefined, "2"));
  const { result } = renderHook(useRunChecks, { initialProps: params() });
  await tick();
  expect(result.current.preview).toMatchObject({ result: null, current: false, fresh: false });
  await tick(1999);
  expect(preview).toHaveBeenCalledTimes(1);
  await tick(1);
  expect(preview).toHaveBeenCalledTimes(2);
  expect(preflight).toHaveBeenCalledTimes(1);
  await tick(60_000);
  act(() => window.dispatchEvent(new Event("focus")));
  await tick();
  expect(preview).toHaveBeenCalledTimes(2);
  await act(() => result.current.preview.retry());
  expect(preview).toHaveBeenCalledTimes(3);
});

it.each([undefined, "nonsense", "-1", "2147483648"])("does not retry an unusable Retry-After %s", async (header) => {
  preview.mockRejectedValue(new HttpError(429, "limited", "", "", "", "", undefined, header));
  renderHook(useRunChecks, { initialProps: params() });
  await tick();
  await tick(60_000);
  act(() => window.dispatchEvent(new Event("focus")));
  await tick();
  expect(preview).toHaveBeenCalledTimes(1);
});

it.each(["invalid", "auth", "unmount"])("cancels a scheduled retry after %s", async (change) => {
  preview.mockRejectedValue(new HttpError(429, "limited", "", "", "", "", undefined, "2"));
  const p = params();
  const { rerender, unmount } = renderHook(useRunChecks, { initialProps: p });
  await tick();
  if (change === "invalid") rerender({ ...p, input: null, body: null });
  else if (change === "auth") act(() => notifyAuthChange());
  else unmount();
  await tick(3000);
  expect(preview).toHaveBeenCalledTimes(1);
});

it("retires auth facts immediately and waits for confirmed same-principal adoption", async () => {
  const old = deferred<PolicyPreviewResult>();
  preview.mockReturnValueOnce(old.promise);
  const p = params();
  const { result, rerender } = renderHook(useRunChecks, { initialProps: p });
  await tick();
  act(() => notifyAuthChange());
  await act(async () => old.resolve(effective));
  await tick();
  expect(result.current.preview.result).toBeNull();
  expect(preview).toHaveBeenCalledTimes(1);
  rerender({ ...p, identity: { ...p.identity, revision: 2, authGeneration: getAuthGeneration() } });
  await tick();
  expect(preview).toHaveBeenCalledTimes(2);
  expect(preflight).toHaveBeenCalledTimes(2);
});

it("holds both reads during sign-out and source refresh, including an identical refetch", async () => {
  const p = params();
  const { result, rerender } = renderHook(useRunChecks, { initialProps: p });
  setSignedOutHold(true);
  await tick();
  expect(preview).not.toHaveBeenCalled();
  setSignedOutHold(false);
  rerender({ ...p, externalRevision: "2", sourceRefreshPending: true });
  await tick();
  expect(preview).not.toHaveBeenCalled();
  rerender({ ...p, externalRevision: "2" });
  await tick();
  expect(preview).toHaveBeenCalledTimes(1);
  expect(result.current.preview.current).toBe(true);
  rerender({ ...p, externalRevision: "3", sourceRefreshPending: true });
  expect(result.current.preview.current).toBe(false);
  rerender({ ...p, externalRevision: "3" });
  await tick();
  expect(preview).toHaveBeenCalledTimes(2);
});

it.each(["doorOpen", "adoDoorOpen"] as const)("refreshes once after %s closes", async (door) => {
  const p = params();
  const { rerender } = renderHook(useRunChecks, { initialProps: p });
  await tick();
  rerender({ ...p, [door]: true });
  rerender(p);
  await tick();
  expect(preview).toHaveBeenCalledTimes(2);
  expect(preflight).toHaveBeenCalledTimes(2);
});

it("accepts bounded seconds and HTTP dates without guessing at other strings", () => {
  const now = Date.parse("Wed, 07 Oct 2026 12:00:00 GMT");
  expect(retryAfterDelay("0", now)).toBe(0);
  expect(retryAfterDelay("Wed, 07 Oct 2026 12:00:02 GMT", now)).toBe(2000);
  expect(retryAfterDelay("2026-10-07", now)).toBeNull();
});

it.each(["preview", "preflight"] as const)("expires both results when %s completes first, without polling", async (first) => {
  const previewReply = deferred<PolicyPreviewResult>(), preflightReply = deferred<typeof ready>();
  preview.mockReturnValueOnce(previewReply.promise);
  preflight.mockReturnValueOnce(preflightReply.promise);
  const { result } = renderHook(useRunChecks, { initialProps: params() });
  const finish = (kind: typeof first) => act(async () => {
    if (kind === "preview") previewReply.resolve(effective);
    else preflightReply.reject(new HttpError(403, "policy refuses this run"));
  });
  await tick(1000);
  await finish(first);
  await tick(1000);
  await finish(first === "preview" ? "preflight" : "preview");
  expect(result.current.preflightBlock).toBe(true);
  expect(result.current.preview.fresh).toBe(true);

  await tick(59_001);
  expect(result.current.preview.fresh).toBe(first !== "preview");
  expect(result.current.preflightFresh).toBe(first !== "preflight");
  expect(result.current.preflightBlock).toBe(first !== "preflight");
  await tick(1000);
  expect(result.current.preview.fresh).toBe(false);
  expect(result.current.preflightFresh).toBe(false);
  expect(result.current.preflightBlock).toBe(false);
  expect(preview).toHaveBeenCalledTimes(1);
  expect(preflight).toHaveBeenCalledTimes(1);
});

it("keeps the last good preview stale through an explicit retry and both rate limits", async () => {
  const { result } = renderHook(useRunChecks, { initialProps: params() });
  await tick();
  expect(result.current.preview).toMatchObject({ result: effective, current: true, fresh: true });
  await tick(5000);
  const retryReply = deferred<PolicyPreviewResult>();
  const limited = new HttpError(429, "limited", "", "", "", "", undefined, "2");
  preview.mockReturnValueOnce(retryReply.promise).mockRejectedValue(limited);
  let retry!: Promise<void>;
  act(() => { retry = result.current.preview.retry(); });
  expect(result.current.preview).toMatchObject({ result: effective, busy: true, current: false, fresh: false });
  await act(async () => { retryReply.reject(limited); await retry; });
  expect(result.current.preview).toMatchObject({ result: effective, error: limited, busy: false, current: false, fresh: false });
  await tick(1999);
  expect(preview).toHaveBeenCalledTimes(2);
  await tick(1);
  expect(preview).toHaveBeenCalledTimes(3);
  expect(result.current.preview).toMatchObject({ result: effective, error: limited, busy: false, current: false, fresh: false });
  await tick(60_000);
  act(() => window.dispatchEvent(new Event("focus")));
  await tick();
  expect(preview).toHaveBeenCalledTimes(3);
  expect(result.current.preview).toMatchObject({ result: effective, current: false, fresh: false });

  preview.mockResolvedValueOnce({ ...effective, warnings: ["new answer"] });
  await act(() => result.current.preview.retry());
  expect(result.current.preview).toMatchObject({ current: true, fresh: true, error: null });
  expect(result.current.preview.result?.warnings).toEqual(["new answer"]);
});

it.each(["body", "auth", "forbidden"])("discards a rate-limited prior preview after %s changes its eligibility", async (change) => {
  const p = params();
  const { result, rerender } = renderHook(useRunChecks, { initialProps: p });
  await tick();
  preview.mockRejectedValueOnce(new HttpError(429, "limited"));
  await act(() => result.current.preview.retry());
  expect(result.current.preview.result).toEqual(effective);
  if (change === "body") rerender({ ...p, body: "changed" });
  else if (change === "auth") act(() => notifyAuthChange());
  else {
    preview.mockRejectedValueOnce(new HttpError(403, "forbidden"));
    await act(() => result.current.preview.retry());
  }
  expect(result.current.preview).toMatchObject({ result: null, current: false, fresh: false });
});
