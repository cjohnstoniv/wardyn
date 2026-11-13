/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import * as React from "react";
import { MemoryRouter, useLocation } from "react-router-dom";
import type { AuditEvent } from "../../lib/types";
import { aheadByHours } from "../../lib/test-clock";


const listAuditMock = vi.fn();
const getRunMock = vi.fn();
const listRunsMock = vi.fn();
vi.mock("../../lib/api/audit", async (importOriginal) => ({
  audit: {
    listAudit: (...a: unknown[]) => listAuditMock(...a),
  },
  // The real reading of a row's call id, not a stub.
  gatewayCallId: (await importOriginal<typeof import("../../lib/api/audit")>()).gatewayCallId,
}));
vi.mock("../../lib/api/runs", () => ({
  runs: {
    getRun: (...a: unknown[]) => getRunMock(...a),
    listRuns: (...a: unknown[]) => listRunsMock(...a),
  },
}));
const healthMock = vi.fn();
vi.mock("../../lib/api/health", () => ({
  health: {
    health: () => healthMock(),
    whoami: () => Promise.resolve(null),
  },
}));

import { AuditScreen } from "./audit";

function ev(partial: Partial<AuditEvent> & { id: string }): AuditEvent {
  return {
    time: aheadByHours(-1),
    actor_type: "agent",
    actor: "spiffe://wardyn/agent",
    action: "tool.call",
    outcome: "success",
    ...partial,
  } as AuditEvent;
}

// The screen renders a real react-router <Link> (Open run, in the drill
// banner) — wrap it in a MemoryRouter so that mounts outside the app's real
// BrowserRouter (main.tsx) without throwing.
function renderScreen() {
  return render(
    <MemoryRouter>
      <AuditScreen />
    </MemoryRouter>,
  );
}

// R4-F132: the live-tail deliberately swallows a failed tick to keep the last
// good view, so `status` stays "ready" forever — and the green pulsing
// "Live · appending" chip rode on `status` alone. A control plane that died
// after the first read therefore left a confident liveness claim over a frozen
// trail. CONSOLE-RULES §10: a result verb needs a real result behind it.
describe("AuditScreen — the liveness chip stops claiming a feed that stopped answering", () => {
  beforeEach(() => {
    listAuditMock.mockReset();
    listAuditMock.mockResolvedValue([]);
    getRunMock.mockReset().mockResolvedValue(undefined);
    healthMock.mockReset().mockResolvedValue({});
  });
  afterEach(() => vi.useRealTimers());

  // Mock packet M4 (approved 2026-10-03), surface D: a field whose key was
  // destroyed reads Erased, muted, with the explanation as its title.
  it("renders an erased target and an erased data field as Erased, never the wire value", async () => {
    listAuditMock.mockResolvedValue([
      ev({
        id: "e1",
        action: "person.create",
        actor_type: "human",
        actor: "sec@corp.example",
        target: "[erased]",
        data: { email: "[erased]", note: "kept" },
      }),
    ]);
    renderScreen();
    expect(await screen.findByText("Added a person — Erased")).toBeInTheDocument();
    const field = screen.getByText("email: Erased");
    expect(field).toHaveAttribute("title", "Erased on request. The event and its place in the log remain.");
    expect(field).toHaveClass("text-muted-foreground");
    expect(screen.queryByText(/\[erased\]/)).toBeNull();
    expect(screen.queryByText(/note:/)).toBeNull();
  });

  it("drops the chip once a poll stops answering, and brings it back when the feed recovers", async () => {
    // Fake timers BEFORE the render: usePoll's setInterval has to be the fake
    // one, or advancing time never reaches a tick.
    vi.useFakeTimers();
    listAuditMock.mockResolvedValueOnce([ev({ id: "e1", run_id: "run_111", action: "egress.allow" })]);
    renderScreen();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(screen.getByText("Live · appending")).toBeInTheDocument();

    // Every later read 503s — the harness's `auditdead` case.
    listAuditMock.mockRejectedValue(new Error("503"));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(6000);
    });
    expect(screen.queryByText("Live · appending")).toBeNull();
    expect(screen.getByText("Not updating — retrying")).toBeInTheDocument();
    // The last good view is still there — the fix removes the CLAIM, not the data.
    expect(screen.getByText("1 event")).toBeInTheDocument();

    // ...and it is not a one-way latch: a feed that comes back is live again.
    listAuditMock.mockReset();
    listAuditMock.mockResolvedValue([ev({ id: "e1", run_id: "run_111", action: "egress.allow" })]);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(6000);
    });
    expect(screen.getByText("Live · appending")).toBeInTheDocument();
  });

  // A read slower than the poll interval must still paint: the next tick waits for it instead of overtaking it,
  // because the request-id guard drops every answer that is not the newest request's.
  it("a read slower than the poll interval still paints, and the chip stays live", async () => {
    vi.useFakeTimers();
    listAuditMock.mockResolvedValueOnce([ev({ id: "e1", run_id: "run_111", action: "egress.allow" })]);
    renderScreen();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(screen.getByText("1 event")).toBeInTheDocument();

    // Each later read takes 7 s, longer than the 5 s poll.
    listAuditMock.mockImplementation(
      () =>
        new Promise((resolve) =>
          setTimeout(
            () => resolve([ev({ id: "e1", run_id: "run_111", action: "egress.allow" }), ev({ id: "e2", run_id: "run_111", action: "egress.deny" })]),
            7000,
          ),
        ),
    );
    await act(async () => {
      await vi.advanceTimersByTimeAsync(13000);
    });
    expect(screen.getByText("2 events")).toBeInTheDocument();
    expect(screen.getByText("Live · appending")).toBeInTheDocument();
  });
});

// One request generation guards everything a read writes: a read the run filter has moved past must not mark the
// list ready, live, failed or not updating under the new filter.
describe("AuditScreen — a read for a run filter that has since changed", () => {
  beforeEach(() => {
    listAuditMock.mockReset();
    getRunMock.mockReset().mockResolvedValue(undefined);
    healthMock.mockReset().mockResolvedValue({});
  });
  afterEach(() => vi.useRealTimers());

  const renderAt = (path: string) =>
    render(
      <MemoryRouter initialEntries={[path]}>
        <AuditScreen />
      </MemoryRouter>,
    );

  it("does not mark the list ready or live while the new filter's own read is still out", async () => {
    let answerRun: ((v: AuditEvent[]) => void) | undefined;
    listAuditMock.mockImplementation((runId?: string) => (runId ? new Promise((done) => (answerRun = done)) : new Promise(() => {})));
    renderAt("/audit?run_id=run-A");
    await waitFor(() => expect(listAuditMock).toHaveBeenCalledWith("run-A", { newest: true }));
    fireEvent.click(await screen.findByRole("button", { name: "Show all events" }));
    await waitFor(() => expect(listAuditMock).toHaveBeenLastCalledWith(undefined));
    await act(async () => answerRun?.([ev({ id: "a1", run_id: "run-A", action: "run.a.only" })]));
    expect(screen.queryByText("Live · appending")).toBeNull();
    expect(screen.queryByText("run.a.only")).toBeNull();
  });

  it("does not put an error over the new filter's list when it fails late", async () => {
    let failRun: ((e: Error) => void) | undefined;
    listAuditMock.mockImplementation((runId?: string) =>
      runId ? new Promise((_, reject) => (failRun = reject)) : Promise.resolve([ev({ id: "g1", action: "global.only" })]),
    );
    renderAt("/audit?run_id=run-A");
    await waitFor(() => expect(listAuditMock).toHaveBeenCalledWith("run-A", { newest: true }));
    fireEvent.click(await screen.findByRole("button", { name: "Show all events" }));
    expect(await screen.findByText("global.only")).toBeInTheDocument();
    await act(async () => failRun?.(new Error("503")));
    expect(screen.getByText("global.only")).toBeInTheDocument();
    expect(screen.getByText("Live · appending")).toBeInTheDocument();
  });

  it("a refocus read that overtakes the new filter's first read still leaves the list shown", async () => {
    // The tab is looked at again in the gap after the filter change commits and before the screen re-renders as
    // loading: that refocus read supersedes the new filter's first read.
    function RefocusOnFilterChange() {
      const { search } = useLocation();
      const first = React.useRef(true);
      React.useEffect(() => {
        if (first.current) first.current = false;
        else document.dispatchEvent(new Event("visibilitychange"));
      }, [search]);
      return null;
    }
    listAuditMock.mockImplementation((runId?: string) =>
      Promise.resolve(runId ? [ev({ id: "a1", run_id: "run-A", action: "run.a.only" })] : [ev({ id: "g1", action: "global.only" })]),
    );
    render(
      <MemoryRouter initialEntries={["/audit?run_id=run-A"]}>
        <AuditScreen />
        <RefocusOnFilterChange />
      </MemoryRouter>,
    );
    expect(await screen.findByText("run.a.only")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Show all events" }));
    expect(await screen.findByText("global.only")).toBeInTheDocument();
    expect(listAuditMock).toHaveBeenCalledTimes(3);
  });

  it("a poll tick still out for the old filter does not mark the new list as not updating when it fails", async () => {
    vi.useFakeTimers();
    listAuditMock.mockResolvedValueOnce([ev({ id: "a1", run_id: "run-A", action: "run.a.only" })]);
    renderAt("/audit?run_id=run-A");
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(screen.getByText("Live · appending")).toBeInTheDocument();
    let failTick: ((e: Error) => void) | undefined;
    listAuditMock.mockImplementation((runId?: string) =>
      runId ? new Promise((_, reject) => (failTick = reject)) : Promise.resolve([ev({ id: "g1", action: "global.only" })]),
    );
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5000);
    });
    expect(failTick).toBeDefined();
    fireEvent.click(screen.getByRole("button", { name: "Show all events" }));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(screen.getByText("global.only")).toBeInTheDocument();
    await act(async () => failTick?.(new Error("503")));
    expect(screen.queryByText("Not updating — retrying")).toBeNull();
    expect(screen.getByText("Live · appending")).toBeInTheDocument();
  });
});

describe("AuditScreen — a refocus read that overtakes the new filter's first read and fails", () => {
  it("shows the load error with its Retry, not the skeleton for good", async () => {
    getRunMock.mockReset().mockResolvedValue(undefined);
    healthMock.mockReset().mockResolvedValue({});
    function RefocusOnFilterChange() {
      const { search } = useLocation();
      const first = React.useRef(true);
      React.useEffect(() => {
        if (first.current) first.current = false;
        else document.dispatchEvent(new Event("visibilitychange"));
      }, [search]);
      return null;
    }
    listAuditMock.mockReset().mockImplementation((runId?: string) =>
      runId ? Promise.resolve([ev({ id: "a1", run_id: "run-A", action: "run.a.only" })]) : Promise.reject(new Error("503")),
    );
    render(
      <MemoryRouter initialEntries={["/audit?run_id=run-A"]}>
        <AuditScreen />
        <RefocusOnFilterChange />
      </MemoryRouter>,
    );
    expect(await screen.findByText("run.a.only")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Show all events" }));
    await waitFor(() => expect(listAuditMock).toHaveBeenCalledTimes(3));
    expect(await screen.findByRole("button", { name: "Retry" })).toBeInTheDocument();
  });
});
