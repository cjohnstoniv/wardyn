/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #125 — a launch that answers 2xx navigates here in the same tick, carrying
// any advisory `warnings[]` as router state (use-launch.ts). This is where
// they land: the rail's own advisory-block shape, dismissible, gone on
// reload. Its own file rather than more cases in run-detail.test.tsx (938
// lines, one shared api fake every other describe there depends on) — same
// reason run-detail-ado.test.tsx gives for keeping ITS own describe
// self-contained, and this suite needed a full extra describe of its own
// once the review's real-BrowserRouter reload proof (defect 1) pushed the
// shared file over the 1000-line gate.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { BrowserRouter, MemoryRouter, Route, Routes, useNavigate } from "react-router-dom";
import * as React from "react";

const getRunMock = vi.fn();
vi.mock("../../lib/api/runs", () => ({
  runs: {
    getRun: (...a: unknown[]) => getRunMock(...a),
    getGrants: vi.fn().mockResolvedValue([]),
    killRun: vi.fn(),
    getFiles: vi.fn().mockRejectedValue(new Error("no runner in this test")),
    getResources: vi.fn().mockRejectedValue(new Error("no runner in this test")),
    getAttachHolder: vi.fn().mockResolvedValue({ held: false }),
    takeoverAttach: vi.fn(),
  },
}));
vi.mock("../../lib/api/approvals", () => ({
  approvals: { listApprovals: vi.fn().mockResolvedValue([]), approve: vi.fn(), deny: vi.fn() },
}));
vi.mock("../../lib/api/audit", () => ({
  audit: { listAudit: vi.fn().mockResolvedValue([]) },
  egressFromAudit: () => [],
  exitCodeFromAudit: () => undefined,
  createRequestFromAudit: () => ({}),
  runEndingFromAudit: () => undefined,
}));
vi.mock("../../lib/api/recordings", () => ({
  recordings: { getRecording: vi.fn().mockResolvedValue(null) },
}));
vi.mock("../../lib/api/health", () => ({
  health: { health: vi.fn().mockResolvedValue({}) },
}));
vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn(), warning: vi.fn() } }));

import { RunDetailScreen } from "./run-detail";
import { RUN_DETAIL } from "../wardyn/copy/run-cockpit";
import { AGENTS } from "../../lib/workspace-providers-copy";

beforeEach(() => {
  getRunMock.mockReset();
});

const RUN = {
  id: "run-1",
  created_at: new Date().toISOString(),
  updated_at: new Date().toISOString(),
  created_by: "me",
  agent: "claude-code",
  repo: "acme/widgets",
  task: "audit the egress proxy",
  confinement_class: "CC2",
  state: "RUNNING",
  spiffe_id: "spiffe://wardyn.local/agent-run/run-1",
  runner_target: "docker",
  interactive: false,
};

describe("RunDetailScreen — the launch advisory from router state (#125)", () => {
  function renderWithLaunchState(run: Record<string, unknown>, warnings: string[]) {
    getRunMock.mockResolvedValue(run);
    return render(
      <MemoryRouter initialEntries={[{ pathname: "/runs/run-1", state: { launchWarnings: warnings } }]}>
        <Routes>
          <Route path="/runs/:id" element={<RunDetailScreen />} />
        </Routes>
      </MemoryRouter>,
    );
  }

  it("renders the warnings and the ephemeral note, and Dismiss removes them", async () => {
    renderWithLaunchState({ ...RUN, state: "RUNNING" }, [
      "Egress narrowed to api.anthropic.com by member policy.",
    ]);

    expect(await screen.findByText(AGENTS.LAUNCH_WARNING_TITLE)).toBeInTheDocument();
    expect(screen.getByText("Egress narrowed to api.anthropic.com by member policy.")).toBeInTheDocument();
    expect(screen.getByText(RUN_DETAIL.LAUNCH_WARNING_EPHEMERAL)).toBeInTheDocument();

    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(screen.getByRole("button", { name: RUN_DETAIL.LAUNCH_WARNING_DISMISS }));
    expect(screen.queryByText(AGENTS.LAUNCH_WARNING_TITLE)).not.toBeInTheDocument();
  });

  // review defect 1: MemoryRouter never touches window.history at all, so a
  // "no launch state in the initial entry" case (the test this replaces)
  // would pass identically whether or not the fix actually worked — it
  // never exercised the real bug. BrowserRouter restores `location.state`
  // from `window.history.state.usr` on EVERY mount over the same entry
  // (react-router's own createBrowserHistory), which is exactly what a real
  // reload is: a fresh mount, same entry. This proves the fix by doing that
  // for real — navigate through a REAL BrowserRouter (the same mechanism
  // use-launch.ts's own navigate() uses, so it populates history.state.usr
  // exactly as a real launch would), then unmount and mount a SECOND,
  // independent BrowserRouter over the same URL and assert nothing shows.
  describe("reload — a real BrowserRouter, not MemoryRouter (review defect 1)", () => {
    afterEach(() => {
      // Leaves no history entry behind for any other test/file sharing this
      // jsdom window (App.test.tsx's own convention for the same reason).
      window.history.pushState({}, "", "/");
    });

    function Launcher({ warnings }: { warnings: string[] }) {
      const navigate = useNavigate();
      React.useEffect(() => {
        navigate("/runs/run-1", { replace: true, state: { launchWarnings: warnings } });
        // eslint-disable-next-line react-hooks/exhaustive-deps -- fire once, on mount
      }, []);
      return null;
    }

    it("really goes on reload: a fresh mount over the same history entry shows nothing", async () => {
      getRunMock.mockResolvedValue({ ...RUN, state: "RUNNING" });

      const { unmount } = render(
        <BrowserRouter>
          <Routes>
            <Route path="/" element={<Launcher warnings={["Egress narrowed to api.anthropic.com by member policy."]} />} />
            <Route path="/runs/:id" element={<RunDetailScreen />} />
          </Routes>
        </BrowserRouter>,
      );
      expect(await screen.findByText(AGENTS.LAUNCH_WARNING_TITLE)).toBeInTheDocument();
      unmount();

      // The "reload": a brand-new BrowserRouter instance, mounted fresh over
      // the SAME history entry the first one just navigated to. Without the
      // fix's `navigate(..., { replace: true, state: null })`, history.state
      // still carries `usr: { launchWarnings: [...] }` here and the note
      // would render again — which is exactly what the reviewer's probe saw.
      render(
        <BrowserRouter>
          <Routes>
            <Route path="/runs/:id" element={<RunDetailScreen />} />
          </Routes>
        </BrowserRouter>,
      );
      await screen.findAllByText(RUN.task);
      expect(screen.queryByText(AGENTS.LAUNCH_WARNING_TITLE)).not.toBeInTheDocument();
    });
  });
});
