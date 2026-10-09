/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { act, renderHook, waitFor } from "@testing-library/react";
import { beforeEach, expect, it, vi } from "vitest";
import { health, type Me } from "../../lib/api/health";
import { getAuthGeneration, notifyAuthChange } from "../../lib/api/core";
import { useMeta } from "./use-shell-meta";

const me = { principal: "alice", operator: false, security_operator: false, role: "user" } as Me;
function deferred() {
  let resolve!: (value: Me | null) => void;
  const promise = new Promise<Me | null>((done) => { resolve = done; });
  return { promise, resolve };
}
beforeEach(() => {
  vi.restoreAllMocks();
  vi.spyOn(health, "health").mockResolvedValue({ trust_domain: "test", identity_provider: "test" } as Awaited<ReturnType<typeof health.health>>);
  vi.spyOn(health, "whoami").mockResolvedValue(me);
});

it("keeps identity unresolved after a failed /me while preserving the shell's tier defaults", async () => {
  vi.mocked(health.whoami).mockResolvedValue(null);
  const { result } = renderHook(useMeta);
  expect(result.current[0].identityResolved).toBe(false);
  await waitFor(() => expect(result.current[0].resolved).toBe(true));
  expect(result.current[0]).toMatchObject({ identityResolved: false, operator: true, securityOperator: true });
});

it("an accepted identity retires an older mount read, including a late success", async () => {
  const old = deferred();
  vi.mocked(health.whoami).mockReturnValueOnce(old.promise);
  const { result } = renderHook(useMeta);
  act(() => result.current[2](me));
  await act(async () => old.resolve({ ...me, principal: "old-owner" }));
  expect(result.current[0].principal).toBe("alice");
  expect(result.current[0].identityResolved).toBe(true);
});

it("same-principal adoption and identical reloads each advance the confirmed revision", async () => {
  const { result } = renderHook(useMeta);
  await waitFor(() => expect(result.current[0].identityResolved).toBe(true));
  const first = result.current[0].identityRevision;
  act(() => result.current[2](me));
  expect(result.current[0].identityRevision).toBe(first + 1);
  act(() => result.current[1]());
  await waitFor(() => expect(result.current[0].identityRevision).toBe(first + 2));
  expect(result.current[0].authGeneration).toBe(getAuthGeneration());
});

it("an auth change prevents an in-flight /me from confirming the new generation", async () => {
  const old = deferred();
  vi.mocked(health.whoami).mockReturnValueOnce(old.promise);
  const { result } = renderHook(useMeta);
  act(() => notifyAuthChange());
  await act(async () => old.resolve(me));
  expect(result.current[0].identityResolved).toBe(false);
  act(() => result.current[1]());
  await waitFor(() => expect(result.current[0].identityResolved).toBe(true));
  expect(result.current[0].authGeneration).toBe(getAuthGeneration());
});

it("an adopted answer keeps the generation it confirmed across a batched auth change", async () => {
  const { result } = renderHook(useMeta);
  await waitFor(() => expect(result.current[0].identityResolved).toBe(true));
  const confirmed = getAuthGeneration();
  const next = deferred();
  vi.mocked(health.whoami).mockReturnValueOnce(next.promise);
  act(() => {
    result.current[1]();
    result.current[2](me);
    notifyAuthChange();
  });
  expect(result.current[0].authGeneration).toBe(confirmed);
  expect(result.current[0].authGeneration).not.toBe(getAuthGeneration());
  await act(async () => next.resolve(me));
  expect(result.current[0].authGeneration).toBe(getAuthGeneration());
});
