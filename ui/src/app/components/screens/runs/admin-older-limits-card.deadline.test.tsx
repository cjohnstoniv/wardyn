/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The bulk restart's per-batch deadline, through the REAL wfetch and the real runs api: only fetch is
// replaced, by a server that answers after a set time and honours the request's abort signal, with
// fake timers standing in for the clock (AbortSignal.timeout is pointed at them).
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, fireEvent, render, screen } from "@testing-library/react";
import { OperatorProvider } from "../../wardyn/operator-context";
import { AdminOlderLimitsCard } from "./admin-older-limits-card";

const toastErrorMock = vi.fn();
vi.mock("sonner", () => ({ toast: { error: (...a: unknown[]) => toastErrorMock(...a) } }));

const json = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

let deadlines: number[];
let restartCalls: string[][];

/** answer(ids) returns [delayMs, response] for one restart request. */
function stubServer(ids: string[], answer: (batch: string[], n: number) => [number, Response]) {
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string, init: RequestInit = {}) => {
      if (String(url).includes("/admin/runs/proxy-window")) {
        return Promise.resolve(
          json(200, { release: "0.9.0", window: ["0.9"], outside: ids.map((run_id) => ({ run_id, created_by: "u", state: "RUNNING", proxy_release: "0.6" })) }),
        );
      }
      const batch = (JSON.parse(String(init.body)) as { run_ids: string[] }).run_ids;
      restartCalls.push(batch);
      const [delay, res] = answer(batch, restartCalls.length);
      return new Promise<Response>((resolve, reject) => {
        const t = setTimeout(() => resolve(res), delay);
        init.signal?.addEventListener("abort", () => {
          clearTimeout(t);
          reject((init.signal as AbortSignal & { reason?: unknown }).reason);
        });
      });
    }),
  );
}

async function openAndRestart() {
  render(
    <OperatorProvider operator>
      <AdminOlderLimitsCard />
    </OperatorProvider>,
  );
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  fireEvent.click(screen.getByRole("button", { name: "Restart with current limits" }));
}
const advance = (ms: number) =>
  act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });

beforeEach(() => {
  vi.clearAllMocks();
  vi.useFakeTimers();
  deadlines = [];
  restartCalls = [];
  vi.spyOn(AbortSignal, "timeout").mockImplementation((ms: number) => {
    deadlines.push(ms);
    const c = new AbortController();
    setTimeout(() => c.abort(new DOMException("timeout", "TimeoutError")), ms);
    return c.signal;
  });
});
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

const ok = (batch: string[]) => json(200, { results: batch.map((run_id) => ({ run_id, ok: true })) });

describe("AdminOlderLimitsCard — the batch deadline, through the real wfetch", () => {
  it("a one-run batch that answers after 45s shows its result", async () => {
    stubServer(["r1"], (b) => [45_000, ok(b)]);
    await openAndRestart();
    await advance(45_000);
    expect(screen.getByText(/r1: Restarted/)).toBeInTheDocument();
    expect(toastErrorMock).not.toHaveBeenCalled();
    expect(Math.max(...deadlines.slice(-3))).toBeGreaterThanOrEqual(60_000);
  });

  it("a five-run batch that answers late but inside its budget shows its results", async () => {
    const ids = ["r0", "r1", "r2", "r3", "r4"];
    stubServer(ids, (b) => [4 * 60_000, ok(b)]);
    await openAndRestart();
    await advance(4 * 60_000);
    expect(screen.getAllByText(/Restarted/)).toHaveLength(5);
    expect(toastErrorMock).not.toHaveBeenCalled();
  });

  it("a failing later batch still leaves the earlier batch's results", async () => {
    const ids = Array.from({ length: 7 }, (_, i) => `r${i}`);
    stubServer(ids, (b, n) => (n === 1 ? [1_000, ok(b)] : [1_000, json(500, { error: "boom" })]));
    await openAndRestart();
    await advance(5_000);
    expect(restartCalls.map((b) => b.length)).toEqual([5, 2]);
    expect(toastErrorMock).toHaveBeenCalled();
    expect(screen.getAllByText(/Restarted/)).toHaveLength(5);
  });
});
