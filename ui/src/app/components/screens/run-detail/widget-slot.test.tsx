/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// R4-F021: the per-widget ErrorBoundary landed on the CANVAS only (6118c5c3),
// while focus mode's dock renders the same RUN_WIDGETS entries. A widget that
// threw in the dock therefore escaped to app-shell's boundary and replaced the
// whole main region — session included — on the one screen where the session IS
// the page. Both renderers now mount widgets through WidgetSlot, so this suite
// asserts the SAME contract twice: one throwing widget, the session survives.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

const getLayout = vi.fn();
const putLayout = vi.fn();
vi.mock("../../../lib/api/run-layout", () => ({
  runLayout: {
    getLayout: (...a: unknown[]) => getLayout(...a),
    putLayout: (...a: unknown[]) => putLayout(...a),
  },
}));
vi.mock("../../../lib/api/runs", () => ({
  runs: {
    getFiles: vi.fn().mockRejectedValue(new Error("no runner in this test")),
    getResources: vi.fn().mockRejectedValue(new Error("no runner in this test")),
  },
}));
// The unmapped-wire-value case the boundary exists for, on the widget both
// renderers show first.
vi.mock("./widgets", async () => {
  const real = await vi.importActual<typeof import("./widgets")>("./widgets");
  return {
    ...real,
    EgressWidget: () => {
      throw new Error("unmapped wire value");
    },
  };
});

import { AppShell } from "../app-shell";
import { ThemeProvider } from "../../wardyn/theme-provider";
import { RunCanvas } from "./canvas";
import { FocusMode } from "./focus-mode";
import type { WidgetContext } from "./widget-registry";

const RUN = {
  id: "run-1a2b3c4d5e",
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

function ctx(): WidgetContext {
  return {
    run: RUN as unknown as WidgetContext["run"],
    finished: false,
    principal: null,
    operator: false,
    grants: [],
    egress: [],
    audit: [],
    onGoAudit: () => {},
    terminalPane: <div data-testid="hero">the session</div>,
  };
}

beforeEach(() => {
  getLayout.mockReset().mockResolvedValue({ preset: "live", layout: [] });
  putLayout.mockReset().mockResolvedValue({ preset: "live", layout: [] });
  // The boundary logs the caught error on purpose (error-boundary.tsx's
  // componentDidCatch); silence it so a deliberate throw is not read as noise.
  vi.spyOn(console, "error").mockImplementation(() => {});
});
afterEach(() => vi.restoreAllMocks());

describe("one throwing widget is contained — in BOTH renderers of RUN_WIDGETS", () => {
  it("the canvas keeps the session and shows the widget's own error card", async () => {
    render(
      <MemoryRouter initialEntries={["/runs/run-1"]}>
        <ThemeProvider>
          <Routes>
            <Route element={<AppShell pendingApprovals={0} attentionCount={0} onSignOut={() => {}} />}>
              <Route path="/runs/:id" element={<RunCanvas ctx={ctx()} />} />
            </Route>
          </Routes>
        </ThemeProvider>
      </MemoryRouter>,
    );
    expect(await screen.findByText("unmapped wire value")).toBeInTheDocument();
    expect(screen.getByTestId("hero")).toBeInTheDocument();
    expect(screen.queryByText(/Something went wrong rendering Runs/)).toBeNull();
  });

  it("focus mode does too — the dock degrades, the full-screen session stays up", () => {
    render(<FocusMode ctx={ctx()} onExit={() => {}} />);
    expect(screen.getByText("unmapped wire value")).toBeInTheDocument();
    expect(screen.getByTestId("hero")).toBeInTheDocument();
    // The dock's boundary names the WIDGET, so the operator is told which card
    // failed rather than being handed a blank overlay.
    expect(screen.getByText(/Something went wrong rendering Egress/)).toBeInTheDocument();
  });
});
