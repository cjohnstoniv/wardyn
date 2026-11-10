/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { act, renderHook, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { health, type Me } from "../../lib/api/health";
import { getAuthGeneration, notifyAuthChange } from "../../lib/api/core";
import { noteSessionExpiry } from "../../lib/api/session-activity";
import { POSTURE_POLL_MS, useMeta } from "./use-shell-meta";

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

it("follows the session expiry the server names, so a renewal moves the banner's deadline", async () => {
  vi.mocked(health.whoami).mockResolvedValue({ ...me, session_expires_at: new Date(Date.now() + 600_000).toISOString() });
  const { result } = renderHook(useMeta);
  await waitFor(() => expect(result.current[0].identityResolved).toBe(true));
  const renewed = new Date(Date.now() + 3_600_000);
  act(() => noteSessionExpiry(renewed.toISOString()));
  expect(result.current[0].sessionExpiresAt?.getTime()).toBe(renewed.getTime());
});

it("a /me body never moves the deadline back past an expiry a header already named", async () => {
  const named = new Date(Date.now() + 7_200_000);
  noteSessionExpiry(named.toISOString());
  vi.mocked(health.whoami).mockResolvedValue({ ...me, session_expires_at: new Date(Date.now() + 120_000).toISOString() });
  const { result } = renderHook(useMeta);
  await waitFor(() => expect(result.current[0].identityResolved).toBe(true));
  expect(result.current[0].sessionExpiresAt?.getTime()).toBe(named.getTime());
});

type Health = Awaited<ReturnType<typeof health.health>>;

afterEach(() => {
  vi.useRealTimers();
});

// The canary re-checks the cluster while a console stays open: the posture follows the health poll,
// verdict and check time from the same answer, both ways and on a new check of the same verdict.
it("follows the confinement posture through the health poll, verdict and check time together", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  const answer = (network_policy: Health["network_policy"], network_policy_checked_at: string): Health =>
    ({ status: "ok", runner: "k8s", network_policy, network_policy_checked_at }) as Health;
  vi.mocked(health.health).mockResolvedValue(answer("enforced", "2026-11-10T01:00:00Z"));
  const { result } = renderHook(useMeta);
  await waitFor(() => expect(result.current[0].networkPolicy).toBe("enforced"));
  expect(result.current[0].networkPolicyCheckedAt).toBe("2026-11-10T01:00:00Z");

  const steps: [Health, string, string][] = [
    [answer("refused", "2026-11-10T01:10:00Z"), "refused", "2026-11-10T01:10:00Z"],
    [answer("enforced", "2026-11-10T01:20:00Z"), "enforced", "2026-11-10T01:20:00Z"],
    [answer("enforced", "2026-11-10T01:30:00Z"), "enforced", "2026-11-10T01:30:00Z"],
    // An outage answers {}: the last posture stays, the unreachable banner says why.
    [{} as Health, "enforced", "2026-11-10T01:30:00Z"],
  ];
  for (const [h, verdict, at] of steps) {
    vi.mocked(health.health).mockResolvedValue(h);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(POSTURE_POLL_MS);
    });
    expect(result.current[0].networkPolicy).toBe(verdict);
    expect(result.current[0].networkPolicyCheckedAt).toBe(at);
  }
});
