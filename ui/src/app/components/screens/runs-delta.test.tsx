/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Runs board between its full reads: a poll that already holds an answer asks only for the live runs and the runs
// that ended past its cut, and shows them with the older ended runs it holds.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter, useNavigate } from "react-router-dom";
import type { AgentRun } from "../../lib/types";
import type { RunsListFilteredResult } from "../../lib/api/runs";

const listRunsFilteredMock = vi.fn();
vi.mock("../../lib/api/runs", () => ({
  runs: {
    listRunsFiltered: (...a: unknown[]) => listRunsFilteredMock(...a),
    getAdminProxyWindow: vi.fn().mockResolvedValue({ release: "", window: [], outside: [] }),
    getAdminRunCapacity: vi.fn().mockReturnValue(new Promise(() => {})),
  },
}));
const getSetupStatusMock = vi.fn();
vi.mock("../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) },
}));
vi.mock("../../lib/use-workspace-list", () => ({
  useWorkspaceList: () => ({ workspaces: [], loading: false, error: false, reload: vi.fn() }),
}));

import { RunsScreen } from "./runs";
import { boardCut } from "./runs/board-delta";
import { baseStatus } from "../../lib/test-fixtures";

const ago = (seconds: number) => new Date(Date.now() - seconds * 1000).toISOString();
const live = (id: string, title: string): AgentRun =>
  ({
    id,
    title,
    created_at: ago(3600),
    updated_at: ago(1),
    created_by: "me",
    agent: "claude-code",
    repo: "acme/widgets",
    task: title,
    confinement_class: "CC2",
    state: "RUNNING",
    spiffe_id: "spiffe://x",
    runner_target: "docker",
    placement: "",
  }) as AgentRun;
const ended = (id: string, title: string, secondsAgo: number): AgentRun =>
  ({ ...live(id, title), state: "COMPLETED", ended_at: ago(secondsAgo) }) as AgentRun;
const answer = (runs: AgentRun[], over: Partial<RunsListFilteredResult> = {}): RunsListFilteredResult => ({
  runs,
  truncated: false,
  hiddenOlder: 0,
  hiddenKilled: 0,
  countsRead: false,
  ...over,
});

const LIVE = live("live", "Live run");
const E5 = ended("e5", "Ended five seconds ago", 5);
const E60 = ended("e60", "Ended one minute ago", 60);
const E600 = ended("e600", "Ended ten minutes ago", 600);

function GoToEnded() {
  const nav = useNavigate();
  return <button onClick={() => nav("/runs?status=ended")}>go ended</button>;
}

const advance = (ms: number) =>
  act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
const sent = (i: number) => listRunsFilteredMock.mock.calls[i][0] as { skipCounts?: boolean; endedAfter?: string };

beforeEach(() => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  listRunsFilteredMock.mockReset();
  getSetupStatusMock.mockReset();
  getSetupStatusMock.mockResolvedValue(baseStatus({ has_runs: true }));
});
afterEach(() => vi.useRealTimers());

describe("RunsScreen — the delta poll", () => {
  it("reads whole, then asks for the live runs and the newly ended ones, and keeps the older ended runs it holds", async () => {
    const first = answer([LIVE, E5, E60, E600], { countsRead: true });
    const cut = boardCut(first);
    listRunsFilteredMock.mockImplementation((f: { skipCounts?: boolean; endedAfter?: string }) =>
      Promise.resolve(
        f.endedAfter ? answer([LIVE, E5, E60], { endedAfter: f.endedAfter }) : first,
      ),
    );
    render(
      <MemoryRouter initialEntries={["/runs"]}>
        <RunsScreen />
        <GoToEnded />
      </MemoryRouter>,
    );
    await advance(0);
    expect(screen.getAllByText("Ended ten minutes ago").length).toBeGreaterThan(0);
    expect(sent(0).endedAfter).toBeUndefined();

    // The 3 s poll sends the cut of the first answer, and the board still shows the run the delta left out.
    await advance(3500);
    expect(sent(1).skipCounts).toBe(true);
    expect(sent(1).endedAfter).toBe(cut);
    expect(screen.getAllByText("Ended five seconds ago").length).toBeGreaterThan(0);
    expect(screen.getAllByText("Ended ten minutes ago").length).toBeGreaterThan(0);

    // After a minute the counts are read again, whole.
    await advance(62_000);
    const wholeReads = listRunsFilteredMock.mock.calls.map((c) => c[0] as { skipCounts?: boolean; endedAfter?: string }).filter((f) => f.skipCounts === false);
    expect(wholeReads.length).toBeGreaterThan(1);
    expect(wholeReads.every((f) => f.endedAfter === undefined)).toBe(true);

    // A filter change reads whole.
    const before = listRunsFilteredMock.mock.calls.length;
    fireEvent.click(screen.getByText("go ended"));
    await advance(100);
    const after = listRunsFilteredMock.mock.calls.slice(before).map((c) => c[0] as { skipCounts?: boolean; endedAfter?: string; status?: string });
    expect(after.length).toBeGreaterThan(0);
    expect(after[0].status).toBe("ended");
    expect(after[0].endedAfter).toBeUndefined();
    expect(after[0].skipCounts).toBeFalsy();
  });
});
