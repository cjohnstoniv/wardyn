/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { AgentRun, Recording } from "../../lib/types";

// R4-F077: the screen used to have no "list all recordings" endpoint to call
// — it listed every run, then probed api.probeRecording(run.id) PER RUN and
// kept only the ones that resolved with a cast (downloading the WHOLE
// document just to answer "does one exist, how big, how long" — 39.8 MB
// measured for 200 runs). has_recording / recording_bytes /
// recording_duration_sec are now DERIVED fields on every run listRuns()
// already returns (internal/types.AgentRun, projected server-side from
// RecordingStore.StatAndTail), so the library is built from ONE call and a
// cast is fetched (api.getRecording) only once a viewer presses play.
const listRunsMock = vi.fn();
const getRecordingMock = vi.fn();
const healthMock = vi.fn();
vi.mock("../../lib/api/recordings", () => ({
  recordings: {
    getRecording: (...a: unknown[]) => getRecordingMock(...a),
  },
}));
vi.mock("../../lib/api/runs", () => ({
  runs: {
    listRuns: () => listRunsMock(),
  },
}));
// components.recording is read once (health()) to tell "this
// deployment never records" apart from "no run has one yet". Default to an
// older/unconfigured daemon's shape ({}) so every other test below is
// unaffected.
vi.mock("../../lib/api/health", () => ({
  health: { health: (...a: unknown[]) => healthMock(...a) },
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

// A run whose server-side projection found a recording — has_recording=true
// is the ONLY signal the screen filters on; recording_duration_sec: 0 would
// still be a real, header-only cast, never "no recording" (see recording.tsx's
// top-of-file comment).
function recorded(id: string, overrides: Partial<AgentRun> = {}): AgentRun {
  return run(id, { has_recording: true, recording_bytes: 4096, recording_duration_sec: 12, ...overrides });
}

function recording(runId: string): Recording {
  return { run_id: runId, header: { version: 2, width: 80, height: 24 }, events: [], cast: "x" };
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
    healthMock.mockReset().mockResolvedValue({});
  });

  it("renders a distinct, retryable error when listRuns() fails, and never fetches a cast", async () => {
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

  // The core F077 pin: rendering the library must issue ZERO cast fetches —
  // has_recording alone decides which runs get a card, straight off the one
  // listRuns() response.
  it("builds the library from listRuns()'s fields alone — rendering it fetches no cast", async () => {
    listRunsMock.mockResolvedValue([
      recorded("run_1", { task: "fix the leak" }),
      run("run_2", { task: "add retries", has_recording: false }),
      run("run_3", { task: "bump deps" }), // has_recording absent entirely
    ]);
    renderScreen();

    await screen.findByText("fix the leak");
    expect(screen.queryByText("add retries")).not.toBeInTheDocument();
    expect(screen.queryByText("bump deps")).not.toBeInTheDocument();
    expect(listRunsMock).toHaveBeenCalledTimes(1);
    expect(getRecordingMock).not.toHaveBeenCalled();
  });

  it("shows the 'none recorded' empty state when every run's has_recording is false", async () => {
    listRunsMock.mockResolvedValue([run("run_1"), run("run_2")]);
    renderScreen();

    await screen.findByText(/none of your runs have a recording yet/i);
    expect(screen.getByRole("link", { name: /go to runs/i })).toBeInTheDocument();
    expect(getRecordingMock).not.toHaveBeenCalled();
  });

  // Regression: a stock Helm install (persistence off) never
  // constructs a recording store, so /healthz's components.recording reports
  // "none". Both empty states must say so honestly instead of implying more
  // runs would eventually produce one.
  it("both empty states name the disabled deployment, not 'not yet', when components.recording is 'none'", async () => {
    healthMock.mockResolvedValue({ components: { recording: { selected: "none", source: "disabled" } } });
    listRunsMock.mockResolvedValue([]);
    renderScreen();

    await screen.findByText(/session recording is disabled on this deployment/i);
    expect(screen.queryByText(/recordings appear once/i)).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: /go to runs/i })).not.toBeInTheDocument();
  });

  it("the 'none recorded' state also names the disabled deployment when components.recording is 'none'", async () => {
    healthMock.mockResolvedValue({ components: { recording: { selected: "none", source: "disabled" } } });
    listRunsMock.mockResolvedValue([run("run_1"), run("run_2")]);
    renderScreen();

    await screen.findByText(/session recording is disabled on this deployment/i);
    expect(screen.queryByText(/none of your runs have a recording yet/i)).not.toBeInTheDocument();
  });

  it("filters down to a 'no recordings match' empty state, and Clear filters restores the library", async () => {
    const runs = Array.from({ length: 5 }, (_, i) => recorded(`run_${i}`, { task: `task number ${i}` }));
    listRunsMock.mockResolvedValue(runs);
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
    listRunsMock.mockResolvedValue([recorded("run_1", { task: "ship the fix" })]);
    getRecordingMock.mockResolvedValue(recording("run_1"));
    renderScreen();

    fireEvent.click(await screen.findByText("ship the fix"));

    await waitFor(() => expect(screen.getByTestId("player")).toHaveAttribute("data-run", "run_1"));
  });

  // The other half of the F077 pin: a cast is fetched exactly once, exactly
  // when a card is played — never for a card that stays unclicked.
  it("fetches a cast only when its card is played, and only that one run's", async () => {
    listRunsMock.mockResolvedValue([
      recorded("run_1", { task: "ship the fix" }),
      recorded("run_2", { task: "second one" }),
    ]);
    getRecordingMock.mockImplementation((id: string) => Promise.resolve(recording(id)));
    renderScreen();

    await screen.findByText("ship the fix");
    await screen.findByText("second one");
    expect(getRecordingMock).not.toHaveBeenCalled();

    fireEvent.click(screen.getByText("ship the fix"));
    await waitFor(() => expect(screen.getByTestId("player")).toHaveAttribute("data-run", "run_1"));
    expect(getRecordingMock).toHaveBeenCalledTimes(1);
    expect(getRecordingMock).toHaveBeenCalledWith("run_1");
  });

  it("shows a retryable error inside the dialog when the cast fetch fails", async () => {
    listRunsMock.mockResolvedValue([recorded("run_1", { task: "ship the fix" })]);
    getRecordingMock.mockRejectedValue(new Error("HTTP 500"));
    renderScreen();

    fireEvent.click(await screen.findByText("ship the fix"));

    await screen.findByText(/recording could not be loaded/i);
    expect(screen.queryByTestId("player")).not.toBeInTheDocument();
  });

  it("gives a KILLED run the same enforcement badge (RunStateBadge) runs.tsx/run-detail.tsx use", async () => {
    listRunsMock.mockResolvedValue([recorded("run_1", { task: "escape attempt", state: "KILLED" })]);
    renderScreen();

    await screen.findByText("escape attempt");
    // RunStateBadge reserves solid bg-danger EXCLUSIVELY for Killed; this card
    // used to render Killed as an ordinary bg-danger-subtle dot chip (no
    // bg-danger token at all).
    expect(screen.getByText("Killed")).toHaveClass("bg-danger");
  });

  // A11y: the card is a plain <div onClick>, invisible to keyboard/screen-reader
  // users without role="button" + tabIndex + a key handler. getByRole("button")
  // only resolves the card at all once role="button" is present.
  it("is keyboard-reachable: getByRole('button') resolves the card, and Enter fires onPlay", async () => {
    listRunsMock.mockResolvedValue([recorded("run_1", { task: "ship the fix" })]);
    getRecordingMock.mockResolvedValue(recording("run_1"));
    renderScreen();

    await screen.findByText("ship the fix");
    // The header's "Refresh" button is also role="button" — name the card by
    // its task text to resolve it specifically.
    const card = screen.getByRole("button", { name: /ship the fix/i });
    fireEvent.keyDown(card, { key: "Enter" });

    await waitFor(() => expect(screen.getByTestId("player")).toHaveAttribute("data-run", "run_1"));
  });

  it("is keyboard-reachable: Space also fires onPlay", async () => {
    listRunsMock.mockResolvedValue([recorded("run_1", { task: "ship the fix" })]);
    getRecordingMock.mockResolvedValue(recording("run_1"));
    renderScreen();

    await screen.findByText("ship the fix");
    const card = screen.getByRole("button", { name: /ship the fix/i });
    fireEvent.keyDown(card, { key: " " });

    await waitFor(() => expect(screen.getByTestId("player")).toHaveAttribute("data-run", "run_1"));
  });

  // "Open run →" and the card do DIFFERENT things — the link goes to /runs/{id},
  // the card replays. The card's onKeyDown preventDefaults Enter, which cancels
  // the anchor's own activation, so without a keydown guard on the link the
  // keyboard path silently yields the wrong screen (replay dialog, not the run).
  it("Enter on 'Open run →' is left to the link — it does not open the replay dialog", async () => {
    listRunsMock.mockResolvedValue([recorded("run_1", { task: "ship the fix" })]);
    renderScreen();

    await screen.findByText("ship the fix");
    const link = screen.getByRole("link", { name: /open run/i });
    expect(link).toHaveAttribute("href", "/runs/run_1");
    // fireEvent returns false when a handler preventDefaulted the event; the
    // browser performs an anchor's Enter-activation only on an unprevented one.
    expect(fireEvent.keyDown(link, { key: "Enter" })).toBe(true);
    expect(screen.queryByTestId("player")).not.toBeInTheDocument();
  });

  // ui-auditRec-4: the card used to be a single role="button" div with the
  // "Open run" <Link> nested INSIDE it (an ARIA nested-interactive
  // anti-pattern — a real, separately-focusable anchor inside another
  // focusable/clickable element). "Open run" must now be its own row,
  // outside the button's subtree, not merely reachable despite the nesting.
  it("'Open run' is NOT nested inside the role=button card (no nested-interactive anti-pattern)", async () => {
    listRunsMock.mockResolvedValue([recorded("run_1", { task: "ship the fix" })]);
    renderScreen();

    await screen.findByText("ship the fix");
    const card = screen.getByRole("button", { name: /ship the fix/i });
    const link = screen.getByRole("link", { name: /open run/i });

    expect(card.contains(link)).toBe(false);
    // A single focusable target per subtree: exactly one <a> in the whole
    // card's OUTER container (the button div's parent), and it isn't inside
    // the button div itself.
    expect(card.querySelector("a")).toBeNull();
  });
});
