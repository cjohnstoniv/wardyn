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
// A check time as /healthz sends it (RFC 3339, UTC), minutes past a fixed past hour: display data only.
const checkedAt = (minute: number) => new Date(Date.UTC(2020, 0, 1, 1, minute)).toISOString();

afterEach(() => {
  vi.useRealTimers();
});

// The canary re-checks the cluster while a console stays open: the posture follows the health poll,
// verdict and check time from the same answer, both ways and on a new check of the same verdict.
it("follows the confinement posture through the health poll, verdict and check time together", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  const answer = (network_policy: Health["network_policy"], network_policy_checked_at: string): Health =>
    ({ status: "ok", runner: "k8s", network_policy, network_policy_checked_at }) as Health;
  vi.mocked(health.health).mockResolvedValue(answer("enforced", checkedAt(0)));
  const { result } = renderHook(useMeta);
  await waitFor(() => expect(result.current[0].networkPolicy).toBe("enforced"));
  expect(result.current[0].networkPolicyCheckedAt).toBe(checkedAt(0));

  const steps: [Health, string, string][] = [
    [answer("refused", checkedAt(10)), "refused", checkedAt(10)],
    [answer("enforced", checkedAt(20)), "enforced", checkedAt(20)],
    [answer("enforced", checkedAt(30)), "enforced", checkedAt(30)],
    // An outage answers {}: the last posture stays, the unreachable banner says why.
    [{} as Health, "enforced", checkedAt(30)],
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

// A failed health read (health() answers {}) is no posture: the first one leaves the poll to learn the
// runner, verdict and time together, and a later one, a retry's included, keeps the last.
it("learns the posture after a failed first read and keeps it through a retry's outage", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  const refused = { status: "ok", runner: "k8s", network_policy: "refused", network_policy_checked_at: checkedAt(10) } as Health;
  vi.mocked(health.health).mockResolvedValue({} as Health);
  const { result } = renderHook(useMeta);
  await waitFor(() => expect(result.current[0].resolved).toBe(true));
  expect(result.current[0].runner).toBe("");

  vi.mocked(health.health).mockResolvedValue(refused);
  await act(async () => {
    await vi.advanceTimersByTimeAsync(POSTURE_POLL_MS);
  });
  expect(result.current[0]).toMatchObject({ runner: "k8s", networkPolicy: "refused", networkPolicyCheckedAt: checkedAt(10) });

  vi.mocked(health.health).mockResolvedValue({} as Health);
  const revision = result.current[0].identityRevision;
  act(() => result.current[1]()); // the identity retry re-reads /healthz too
  await waitFor(() => expect(result.current[0].identityRevision).toBe(revision + 1));
  expect(result.current[0]).toMatchObject({ runner: "k8s", networkPolicy: "refused", networkPolicyCheckedAt: checkedAt(10) });

  vi.mocked(health.health).mockResolvedValue({ ...refused, network_policy: "enforced", network_policy_checked_at: checkedAt(20) });
  await act(async () => {
    await vi.advanceTimersByTimeAsync(POSTURE_POLL_MS);
  });
  expect(result.current[0]).toMatchObject({ runner: "k8s", networkPolicy: "enforced", networkPolicyCheckedAt: checkedAt(20) });
});

// The verdict's source rides the same answer: a Docker control plane carries a connected Kubernetes
// runner's verdict with network_policy_source "runner", and a later answer without it clears it.
it("carries the verdict's source with the posture", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  const fromRunner = { status: "ok", runner: "docker", network_policy: "refused", network_policy_checked_at: checkedAt(10), network_policy_source: "runner" } as Health;
  vi.mocked(health.health).mockResolvedValue(fromRunner);
  const { result } = renderHook(useMeta);
  await waitFor(() => expect(result.current[0].networkPolicySource).toBe("runner"));
  expect(result.current[0]).toMatchObject({ runner: "docker", networkPolicy: "refused", networkPolicyCheckedAt: checkedAt(10) });

  vi.mocked(health.health).mockResolvedValue({ status: "ok", runner: "docker" } as Health);
  await act(async () => {
    await vi.advanceTimersByTimeAsync(POSTURE_POLL_MS);
  });
  expect(result.current[0]).toMatchObject({ networkPolicy: "", networkPolicySource: "" });
});
