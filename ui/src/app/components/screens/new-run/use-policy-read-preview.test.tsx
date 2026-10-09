/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { HttpError } from "../../../lib/api/core";
import type { PolicyPreviewResult } from "../../../lib/types/policy-preview";
import { usePolicyReadPreview, type PolicyPreviewRead } from "./use-policy-read-preview";

const answer = (host: string): PolicyPreviewResult => ({
  spec: { allowed_domains: [host], first_use_approval: "deny_with_review", min_confinement_class: "CC1" },
  source: { kind: "inline" },
  provisional: true,
  redacted: false,
  warnings: [],
  pending: [],
  repository_access: [],
});
const none: PolicyPreviewRead = { result: null, error: null, busy: false, fresh: false };
const limitedFor = (retryAfter: string | undefined) => Object.assign(new HttpError(429, "limited"), { retryAfter });

interface Props {
  read: PolicyPreviewRead;
  scope: string;
  held: boolean;
}
const mount = (initial: Props) => renderHook((p: Props) => usePolicyReadPreview(p.read, p.scope, p.held), { initialProps: initial });

beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

it("passes a current answer through as it is", () => {
  const first = answer("a.example");
  const { result } = mount({ read: { ...none, result: first, fresh: true }, scope: "alice/custom", held: false });
  expect(result.current).toMatchObject({ result: first, fresh: true, busy: false, error: null, rateLimitSeconds: null });
});

it("keeps the last answer while the next is on its way, never as fresh", () => {
  const first = answer("a.example");
  const { result, rerender } = mount({ read: { ...none, result: first, fresh: true }, scope: "alice/custom", held: false });
  rerender({ read: { ...none, busy: true }, scope: "alice/custom", held: false });
  expect(result.current).toMatchObject({ result: first, fresh: false, busy: true });
});

it("never offers an answer to another person or another policy choice", () => {
  const first = answer("a.example");
  const { result, rerender } = mount({ read: { ...none, result: first, fresh: true }, scope: "alice/custom", held: false });
  rerender({ read: none, scope: "alice/default", held: false });
  expect(result.current.result).toBeNull();
  rerender({ read: none, scope: "bob/custom", held: false });
  expect(result.current.result).toBeNull();
  // Not on the way back either: the answer on file is whoever read last.
  rerender({ read: { ...none, result: answer("b.example") }, scope: "bob/custom", held: false });
  rerender({ read: none, scope: "alice/custom", held: false });
  expect(result.current.result).toBeNull();
});

it("a source that does not parse shows only the earlier answer: not fresh, not busy, no error of its own", () => {
  const first = answer("a.example");
  const { result, rerender } = mount({ read: { ...none, result: first, fresh: true }, scope: "alice/custom", held: false });
  rerender({ read: { ...none, busy: true, error: new Error("late") }, scope: "alice/custom", held: true });
  expect(result.current).toEqual({ result: first, busy: false, fresh: false });

  const fresh = mount({ read: none, scope: "alice/custom", held: true });
  expect(fresh.result.current).toEqual({ result: null, busy: false, fresh: false });
});

it("a refused read keeps the server's sentence", () => {
  const { result } = mount({ read: { ...none, error: new HttpError(403, "policy: not readable by you") }, scope: "alice/custom", held: false });
  expect(result.current).toMatchObject({ error: "policy: not readable by you", rateLimitSeconds: null });
});

it("a rate-limited read counts down its Retry-After and stops at one second", async () => {
  const first = answer("a.example");
  const { result, rerender } = mount({ read: { ...none, result: first, fresh: true }, scope: "alice/custom", held: false });
  rerender({ read: { ...none, result: first, error: limitedFor("3") }, scope: "alice/custom", held: false });
  expect(result.current).toMatchObject({ result: first, fresh: false, error: null, rateLimitSeconds: 3 });
  await act(() => vi.advanceTimersByTimeAsync(1000));
  expect(result.current.rateLimitSeconds).toBe(2);
  await act(() => vi.advanceTimersByTimeAsync(5000));
  expect(result.current.rateLimitSeconds).toBe(1);
});

it("a rate limit that names no wait keeps the server's sentence instead of guessing one", () => {
  const { result } = mount({ read: { ...none, error: limitedFor(undefined) }, scope: "alice/custom", held: false });
  expect(result.current).toMatchObject({ error: "limited", rateLimitSeconds: null });
});
