/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { AgentRun, Recording } from "../../lib/types";

// The screen has no "list all recordings" endpoint to call — it lists every
// run, then probes api.getRecording(run.id) per run and keeps only the ones
// that resolve with a cast. MEDIUM behaviors pinned here (re-expressed
// against that client-synthesized library, which replaced the old
// single-run picker):
//  - a listRuns() rejection renders its own distinct, retryable error and
//    never fires any per-run checks.
//  - a per-run getRecording() 404 (resolves undefined) is silently "no
//    recording" — no card, no failure count.
//  - a per-run getRecording() REJECTION is a real check failure: it must
//    surface a retryable "N runs couldn't be checked" banner, not vanish.
//  - "no runs at all", "every run checked but none recorded", and "filters
//    hid everything" are three distinct empty states with their own CTAs.

const listRunsMock = vi.fn();
const getRecordingMock = vi.fn();
vi.mock("../../lib/api", () => ({
  api: {
    listRuns: () => listRunsMock(),
    getRecording: (...a: unknown[]) => getRecordingMock(...a),
  },
}));

// asciinema-player is heavy / DOM-driven; stub the player so we can assert
// which recording it received via a data attribute.
vi.mock("../wardyn/terminal-player", () => ({
  TerminalPlayer: ({ recording }: { recording: Recording }) => (
    <div data-testid="player" data-run={recording.run_id} />
  ),
}));

import { RecordingScreen } from "./recording";

function run(id: string, overrides: Partial<AgentRun> = {}): AgentRun {
  return {
    id,
    created_at: "2026-06-01T00:00:00.000Z",
    updated_at: "2026-06-01T00:00:00.000Z",
    created_by: "op",
    agent: "claude-code",
    repo: "acme/widgets",
    task: `task-${id}`,
    confinement_class: "CC1",
    state: "COMPLETED",
    spiffe_id: `spiffe://wardyn/${id}`,
    runner_target: "docker",
    ...overrides,
  } as AgentRun;
}

function rec(id: string): Recording {
  return { run_id: id, header: { version: 2, width: 80, height: 24 }, events: [], cast: "x" };
}

function renderScreen() {
  return render(
    <MemoryRouter>
      <RecordingScreen />
    </MemoryRouter>,
  );
}

describe("RecordingScreen", () => {
  beforeEach(() => {
    listRunsMock.mockReset();
    getRecordingMock.mockReset();
  });

  it("renders a distinct, retryable error when listRuns() fails, and never fires per-run checks", async () => {
    listRunsMock.mockRejectedValue(new Error("boom"));
    renderScreen();

    await waitFor(() =>
      expect(screen.getByText(/couldn't load the list of runs/i)).toBeInTheDocument(),
    );
    expect(screen.getByRole("button", { name: /retry/i })).toBeInTheDocument();
    expect(screen.queryByText(/none of your runs have a recording yet/i)).not.toBeInTheDocument();
    expect(getRecordingMock).not.toHaveBeenCalled();
  });

  it("shows the true-empty state (no runs at all) with a 'Go to Runs' CTA", async () => {
    listRunsMock.mockResolvedValue([]);
    renderScreen();

    await screen.findByText(/recordings appear once a run's terminal session is captured/i);
    expect(screen.getByRole("link", { name: /go to runs/i })).toBeInTheDocument();
    expect(getRecordingMock).not.toHaveBeenCalled();
  });

  it("builds a card only for runs whose recording resolves — a 404 is silently 'no recording', a rejection is a real check failure", async () => {
    listRunsMock.mockResolvedValue([
      run("run_1", { task: "fix the leak" }),
      run("run_2", { task: "add retries" }),
      run("run_3", { task: "bump deps" }),
    ]);
    getRecordingMock.mockImplementation((id: string) => {
      if (id === "run_1") return Promise.resolve(rec("run_1"));
      if (id === "run_2") return Promise.resolve(undefined); // 404 => no recording
      return Promise.reject(new Error("HTTP 500")); // run_3: real fetch error
    });
    renderScreen();

    await screen.findByText("fix the leak");
    expect(screen.queryByText("add retries")).not.toBeInTheDocument();
    expect(screen.queryByText("bump deps")).not.toBeInTheDocument();

    await waitFor(() =>
      expect(screen.getByText(/1 run couldn't be checked for a recording/i)).toBeInTheDocument(),
    );
    expect(screen.getByRole("button", { name: /retry/i })).toBeInTheDocument();
  });

  it("shows the 'none recorded' empty state once every run is checked and none has a recording", async () => {
    listRunsMock.mockResolvedValue([run("run_1"), run("run_2")]);
    getRecordingMock.mockResolvedValue(undefined);
    renderScreen();

    await screen.findByText(/none of your runs have a recording yet/i);
    expect(screen.getByRole("link", { name: /go to runs/i })).toBeInTheDocument();
  });

  it("filters down to a 'no recordings match' empty state, and Clear filters restores the library", async () => {
    const runs = Array.from({ length: 5 }, (_, i) => run(`run_${i}`, { task: `task number ${i}` }));
    listRunsMock.mockResolvedValue(runs);
    getRecordingMock.mockImplementation((id: string) => Promise.resolve(rec(id)));
    renderScreen();

    await screen.findByText("task number 0");
    const search = screen.getByPlaceholderText(/search tasks, repos, run ids/i);
    fireEvent.change(search, { target: { value: "zzz-no-match" } });

    await screen.findByText(/no recordings match these filters/i);
    expect(screen.queryByText("task number 0")).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /clear filters/i }));
    await screen.findByText("task number 0");
  });

  it("opens the replay dialog with that run's recording when its card is clicked", async () => {
    listRunsMock.mockResolvedValue([run("run_1", { task: "ship the fix" })]);
    getRecordingMock.mockResolvedValue(rec("run_1"));
    renderScreen();

    fireEvent.click(await screen.findByText("ship the fix"));

    await waitFor(() => expect(screen.getByTestId("player")).toHaveAttribute("data-run", "run_1"));
  });

  it("gives a KILLED run the same enforcement badge (RunStatusBadge) runs.tsx/run-detail.tsx use — U120", async () => {
    listRunsMock.mockResolvedValue([run("run_1", { task: "escape attempt", state: "KILLED" })]);
    getRecordingMock.mockResolvedValue(rec("run_1"));
    renderScreen();

    await screen.findByText("escape attempt");
    // RunStatusBadge reserves solid bg-danger EXCLUSIVELY for Killed; the plain
    // RunStateBadge this card used to call renders Killed as an ordinary
    // bg-danger-subtle dot chip (no bg-danger token at all).
    expect(screen.getByText("Killed")).toHaveClass("bg-danger");
  });

  // A11y: the card is a plain <div onClick>, invisible to keyboard/screen-reader
  // users without role="button" + tabIndex + a key handler. getByRole("button")
  // only resolves the card at all once role="button" is present.
  it("is keyboard-reachable: getByRole('button') resolves the card, and Enter fires onPlay", async () => {
    listRunsMock.mockResolvedValue([run("run_1", { task: "ship the fix" })]);
    getRecordingMock.mockResolvedValue(rec("run_1"));
    renderScreen();

    await screen.findByText("ship the fix");
    // The header's "Refresh" button is also role="button" — name the card by
    // its task text to resolve it specifically.
    const card = screen.getByRole("button", { name: /ship the fix/i });
    fireEvent.keyDown(card, { key: "Enter" });

    await waitFor(() => expect(screen.getByTestId("player")).toHaveAttribute("data-run", "run_1"));
  });

  it("is keyboard-reachable: Space also fires onPlay", async () => {
    listRunsMock.mockResolvedValue([run("run_1", { task: "ship the fix" })]);
    getRecordingMock.mockResolvedValue(rec("run_1"));
    renderScreen();

    await screen.findByText("ship the fix");
    const card = screen.getByRole("button", { name: /ship the fix/i });
    fireEvent.keyDown(card, { key: " " });

    await waitFor(() => expect(screen.getByTestId("player")).toHaveAttribute("data-run", "run_1"));
  });
});
