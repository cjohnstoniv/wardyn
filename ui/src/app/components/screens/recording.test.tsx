/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, render, screen, waitFor, fireEvent } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { AgentRun, Recording } from "../../lib/types";
import type { RecordingProbe } from "../../lib/api/recordings";

// The screen has no "list all recordings" endpoint to call — it lists every
// run, then probes api.probeRecording(run.id) per run and keeps only the ones
// that resolve with a cast. MEDIUM behaviors pinned here (re-expressed
// against that client-synthesized library, which replaced the old
// single-run picker):
//  - a listRuns() rejection renders its own distinct, retryable error and
//    never fires any per-run checks.
//  - a per-run probeRecording() 404 (resolves undefined) is silently "no
//    recording" — no card, no failure count.
//  - a per-run probeRecording() REJECTION is a real check failure: it must
//    surface a retryable "N runs couldn't be checked" banner, not vanish.
//  - "no runs at all", "every run checked but none recorded", and "filters
//    hid everything" are three distinct empty states with their own CTAs.
//  - the probes go out at most PROBE_CONCURRENCY at a time, and a card's cast
//    is parsed only when it is played (F077: the bare forEach put every run's
//    request in flight at once and held a parsed event stream for each).

const listRunsMock = vi.fn();
const probeRecordingMock = vi.fn();
// parseCast stands in for the real parser (an AsciicastEvent per output frame).
// It is a spy, not a stub, because WHEN it runs is the point: once, for the one
// cast a viewer pressed play on.
const parseCastMock = vi.fn((runId: string, text: string): Recording => ({
  run_id: runId,
  header: { version: 2, width: 80, height: 24 },
  events: [],
  cast: text,
}));
const healthMock = vi.fn();
vi.mock("../../lib/api/recordings", () => ({
  recordings: {
    probeRecording: (...a: unknown[]) => probeRecordingMock(...a),
    parseCast: (runId: string, text: string) => parseCastMock(runId, text),
  },
}));
vi.mock("../../lib/api/runs", () => ({
  runs: {
    listRuns: () => listRunsMock(),
  },
}));
// W21-S1-7: components.recording is read once (health()) to tell "this
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

import { PROBE_CONCURRENCY, RecordingScreen } from "./recording";

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

function probe(id: string): RecordingProbe {
  return { run_id: id, durationSec: 12, bytes: 1, cast: "x" };
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
    probeRecordingMock.mockReset();
    parseCastMock.mockClear();
    healthMock.mockReset().mockResolvedValue({});
  });

  it("renders a distinct, retryable error when listRuns() fails, and never fires per-run checks", async () => {
    listRunsMock.mockRejectedValue(new Error("boom"));
    renderScreen();

    await waitFor(() =>
      expect(screen.getByText(/couldn't load the list of runs/i)).toBeInTheDocument(),
    );
    expect(screen.getByRole("button", { name: /retry/i })).toBeInTheDocument();
    expect(screen.queryByText(/none of your runs have a recording yet/i)).not.toBeInTheDocument();
    expect(probeRecordingMock).not.toHaveBeenCalled();
  });

  it("shows the true-empty state (no runs at all) with a 'Go to Runs' CTA", async () => {
    listRunsMock.mockResolvedValue([]);
    renderScreen();

    await screen.findByText(/recordings appear once a run's terminal session is captured/i);
    expect(screen.getByRole("link", { name: /go to runs/i })).toBeInTheDocument();
    expect(probeRecordingMock).not.toHaveBeenCalled();
  });

  it("builds a card only for runs whose recording resolves — a 404 is silently 'no recording', a rejection is a real check failure", async () => {
    listRunsMock.mockResolvedValue([
      run("run_1", { task: "fix the leak" }),
      run("run_2", { task: "add retries" }),
      run("run_3", { task: "bump deps" }),
    ]);
    probeRecordingMock.mockImplementation((id: string) => {
      if (id === "run_1") return Promise.resolve(probe("run_1"));
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
    probeRecordingMock.mockResolvedValue(undefined);
    renderScreen();

    await screen.findByText(/none of your runs have a recording yet/i);
    expect(screen.getByRole("link", { name: /go to runs/i })).toBeInTheDocument();
  });

  // W21-S1-7 regression: a stock Helm install (persistence off) never
  // constructs a recording store, so /healthz's components.recording reports
  // "none". Both empty states must say so honestly instead of implying more
  // runs would eventually produce one — on base 763beb5 componentsInfo didn't
  // carry the constructed store at all (it echoed the *flag*, "fs", even when
  // the fs backend's empty Dir left it disabled), so this failed there too.
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
    probeRecordingMock.mockResolvedValue(undefined);
    renderScreen();

    await screen.findByText(/session recording is disabled on this deployment/i);
    expect(screen.queryByText(/none of your runs have a recording yet/i)).not.toBeInTheDocument();
  });

  it("filters down to a 'no recordings match' empty state, and Clear filters restores the library", async () => {
    const runs = Array.from({ length: 5 }, (_, i) => run(`run_${i}`, { task: `task number ${i}` }));
    listRunsMock.mockResolvedValue(runs);
    probeRecordingMock.mockImplementation((id: string) => Promise.resolve(probe(id)));
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
    probeRecordingMock.mockResolvedValue(probe("run_1"));
    renderScreen();

    fireEvent.click(await screen.findByText("ship the fix"));

    await waitFor(() => expect(screen.getByTestId("player")).toHaveAttribute("data-run", "run_1"));
  });

  it("gives a KILLED run the same enforcement badge (RunStateBadge) runs.tsx/run-detail.tsx use", async () => {
    listRunsMock.mockResolvedValue([run("run_1", { task: "escape attempt", state: "KILLED" })]);
    probeRecordingMock.mockResolvedValue(probe("run_1"));
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
    listRunsMock.mockResolvedValue([run("run_1", { task: "ship the fix" })]);
    probeRecordingMock.mockResolvedValue(probe("run_1"));
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
    probeRecordingMock.mockResolvedValue(probe("run_1"));
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
    listRunsMock.mockResolvedValue([run("run_1", { task: "ship the fix" })]);
    probeRecordingMock.mockResolvedValue(probe("run_1"));
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
    listRunsMock.mockResolvedValue([run("run_1", { task: "ship the fix" })]);
    probeRecordingMock.mockResolvedValue(probe("run_1"));
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
  // F077: the library used to fire one request per run in a bare forEach —
  // 200 runs measured 200 requests in flight at once. The window is the fix;
  // the total work and the order are unchanged.
  it(`probes at most PROBE_CONCURRENCY (${PROBE_CONCURRENCY}) runs at a time, and still checks every run`, async () => {
    const N = 30;
    listRunsMock.mockResolvedValue(Array.from({ length: N }, (_, i) => run(`run_${i}`)));
    let inFlight = 0;
    let peak = 0;
    const pending: Array<() => void> = [];
    probeRecordingMock.mockImplementation(
      () =>
        new Promise<undefined>((resolve) => {
          inFlight++;
          peak = Math.max(peak, inFlight);
          pending.push(() => {
            inFlight--;
            resolve(undefined);
          });
        }),
    );
    renderScreen();

    await waitFor(() => expect(probeRecordingMock).toHaveBeenCalledTimes(PROBE_CONCURRENCY));
    // Nothing else goes out until a lane frees up — drain one at a time.
    for (let i = 0; i < N; i++) {
      await waitFor(() => expect(pending.length).toBeGreaterThan(0));
      const release = pending.shift() as () => void;
      await act(async () => {
        release();
      });
    }
    await waitFor(() => expect(probeRecordingMock).toHaveBeenCalledTimes(N));
    expect(peak).toBeLessThanOrEqual(PROBE_CONCURRENCY);
    expect(peak).toBeGreaterThan(1); // …and it is a WINDOW, not a serial queue
    expect(new Set(probeRecordingMock.mock.calls.map((c) => c[0])).size).toBe(N);
  });

  // F077: a card renders a duration and a size. Parsing every probed cast into
  // an event stream on load was work nobody asked for, held live in state for
  // every run at once.
  it("parses a cast only when its card is played — never at probe time", async () => {
    listRunsMock.mockResolvedValue([
      run("run_1", { task: "ship the fix" }),
      run("run_2", { task: "second one" }),
    ]);
    probeRecordingMock.mockImplementation((id: string) => Promise.resolve(probe(id)));
    renderScreen();

    await screen.findByText("ship the fix");
    await screen.findByText("second one");
    expect(parseCastMock).not.toHaveBeenCalled();

    fireEvent.click(screen.getByText("ship the fix"));
    await waitFor(() => expect(screen.getByTestId("player")).toHaveAttribute("data-run", "run_1"));
    expect(parseCastMock).toHaveBeenCalledTimes(1);
    expect(parseCastMock).toHaveBeenCalledWith("run_1", "x");
  });
});
