/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A cold load whose mount probe cannot reach the daemon (a 5xx, a dropped connection) parks on the sign-in
// gate. It must ask again on a backoff and enter the console once the daemon answers; a 401 and a 403 are
// real answers and are never retried. core is mocked (as in App.attention.test.tsx) so each probe answer is
// scripted: "unreachable" is a transient failure (a 5xx, a 429, a network error), "forbidden" a final 4xx.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

type Answer = "authed" | "unauthed" | "unreachable" | "forbidden";
const probes = vi.hoisted(() => ({ results: [] as Answer[], calls: 0 }));
const next = () => {
  probes.calls++;
  const a = probes.results.shift() ?? "unreachable";
  return {
    probe: a === "forbidden" ? ("unreachable" as const) : a,
    reason: "",
    transient: a === "unreachable",
  };
};

vi.mock("./lib/api/core", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./lib/api/core")>();
  return {
    ...actual,
    probeAuthWithReason: vi.fn(async () => next()),
  };
});
vi.mock("./lib/api/runs", () => ({
  runs: { listRunsFiltered: vi.fn().mockResolvedValue({ runs: [], truncated: false, hiddenOlder: 0, hiddenKilled: 0 }) },
}));
vi.mock("./lib/api/health", () => ({
  health: {
    whoami: vi.fn().mockResolvedValue({ principal: "operator", method: "token", operator: true, role: "admin", identity_provider: "embedded" }),
    health: vi.fn().mockResolvedValue({}),
    readyz: vi.fn().mockResolvedValue({ status: "ok" }),
    logout: vi.fn().mockResolvedValue(true),
  },
}));
vi.mock("./lib/use-poll", async (importOriginal) => ({
  ...(await importOriginal<typeof import("./lib/use-poll")>()),
  usePoll: () => {},
}));
// The shell's Needs you notices poll on their own interval, which is not this test's backoff.
vi.mock("./components/wardyn/needs-you-notices", () => ({ NeedsYouNotices: () => null }));

import App from "./App";

const gate = () => screen.queryByRole("heading", { name: "Sign in" });

async function mount() {
  render(
    <MemoryRouter initialEntries={["/admin"]}>
      <App />
    </MemoryRouter>,
  );
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
}
const tick = (ms: number) => act(async () => { await vi.advanceTimersByTimeAsync(ms); });

describe("a cold load that cannot reach the daemon", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    probes.results = [];
    probes.calls = 0;
  });
  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it("re-probes on a 1s, 2s, 4s, 8s, then 15s backoff and enters the console once one answers", async () => {
    probes.results = ["unreachable", "unreachable", "unreachable", "unreachable", "unreachable", "unreachable", "authed"];
    await mount();
    expect(gate()).toBeInTheDocument();
    expect(probes.calls).toBe(1);
    for (const [wait, calls] of [[1000, 2], [2000, 3], [4000, 4], [8000, 5], [15000, 6]] as const) {
      await tick(wait - 1);
      expect(probes.calls).toBe(calls - 1);
      await tick(1);
      expect(probes.calls).toBe(calls);
    }
    await tick(15000);
    expect(probes.calls).toBe(7);
    expect(gate()).not.toBeInTheDocument();
    // Auth resolved: nothing is left to fire.
    expect(vi.getTimerCount()).toBe(0);
  });

  it("stops at unmount with no timer left", async () => {
    await mount();
    expect(vi.getTimerCount()).toBeGreaterThan(0);
    cleanup();
    expect(vi.getTimerCount()).toBe(0);
    await tick(60000);
    expect(probes.calls).toBe(1);
  });

  it("stops when the person types into the gate", async () => {
    await mount();
    act(() => {
      document.dispatchEvent(new Event("input", { bubbles: true }));
    });
    expect(vi.getTimerCount()).toBe(0);
    await tick(60000);
    expect(probes.calls).toBe(1);
  });

  it("a focus re-probe and a due timer never stack: one probe at a time", async () => {
    let release: () => void = () => {};
    const core = await import("./lib/api/core");
    vi.mocked(core.probeAuthWithReason).mockImplementationOnce(async () => {
      probes.calls++;
      return { probe: "unreachable", reason: "", transient: true };
    });
    vi.mocked(core.probeAuthWithReason).mockImplementationOnce(
      () => new Promise((r) => { probes.calls++; release = () => r({ probe: "unreachable", reason: "", transient: true }); }),
    );
    await mount();
    expect(probes.calls).toBe(1);
    act(() => {
      window.dispatchEvent(new Event("focus"));
    });
    expect(probes.calls).toBe(2);
    await tick(1000);
    expect(probes.calls).toBe(2);
    release();
    await tick(1000);
    expect(probes.calls).toBe(3);
  });

  it("a focus re-probe that gets a final answer ends the retries", async () => {
    probes.results = ["unreachable", "unauthed"];
    await mount();
    act(() => {
      window.dispatchEvent(new Event("focus"));
    });
    await tick(0);
    expect(vi.getTimerCount()).toBe(0);
    await tick(60000);
    expect(probes.calls).toBe(2);
    expect(gate()).toBeInTheDocument();
  });

  it("neg: a 403 on the mount probe keeps the gate and is never retried", async () => {
    probes.results = ["forbidden"];
    await mount();
    expect(gate()).toBeInTheDocument();
    expect(vi.getTimerCount()).toBe(0);
    await tick(120000);
    expect(probes.calls).toBe(1);
    expect(gate()).toBeInTheDocument();
  });

  it("neg: a 401 on the mount probe keeps the gate and is never retried", async () => {
    probes.results = ["unauthed"];
    await mount();
    expect(gate()).toBeInTheDocument();
    expect(vi.getTimerCount()).toBe(0);
    await tick(120000);
    expect(probes.calls).toBe(1);
    expect(gate()).toBeInTheDocument();
  });

  it("a 401 on a retry ends the retries and keeps the gate", async () => {
    probes.results = ["unreachable", "unauthed"];
    await mount();
    await tick(1000);
    expect(probes.calls).toBe(2);
    await tick(120000);
    expect(probes.calls).toBe(2);
    expect(gate()).toBeInTheDocument();
    expect(vi.getTimerCount()).toBe(0);
  });
});
