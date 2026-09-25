/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// F1-F9: RunContextRow kept a LOCAL third copy of the headline fallback chain
// ((run.title ?? "").trim() || run.task || "Interactive session") that ignored
// run.interactive — a nameless NON-interactive run was called "Interactive
// session" on the approval card. The canonical helper (board-groups.ts's
// rowHeadline) already gets this right; this pins that RunContextRow now
// defers to it.
import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { AgentRun } from "../../lib/types";

const getRunMock = vi.fn();
vi.mock("../../lib/api/runs", () => ({
  runs: { getRun: (...a: unknown[]) => getRunMock(...a) },
}));

import { RunContextRow } from "./run-context-row";

const RUN: AgentRun = {
  id: "run-1",
  created_at: new Date().toISOString(),
  updated_at: new Date().toISOString(),
  created_by: "me",
  agent: "claude-code",
  repo: "acme/widgets",
  task: "",
  confinement_class: "CC2",
  state: "RUNNING",
  spiffe_id: "spiffe://x",
  runner_target: "docker",
  interactive: false,
};

function renderRow(run: Partial<AgentRun>) {
  getRunMock.mockResolvedValue({ ...RUN, ...run });
  return render(
    <MemoryRouter>
      <RunContextRow runId="run-1" />
    </MemoryRouter>,
  );
}

describe("RunContextRow — headline defers to the canonical rowHeadline chain", () => {
  // ticket: F1-F9
  it("a nameless NON-interactive run reads as the fallback dash, never 'Interactive session'", async () => {
    renderRow({ title: undefined, task: "", workspace_path: undefined, interactive: false });
    expect(await screen.findByText("—")).toBeInTheDocument();
    expect(screen.queryByText("Interactive session")).not.toBeInTheDocument();
  });

  // Neg: a genuinely interactive run with nothing else to say still gets the
  // "Interactive session" label — the fix must not remove that fallback for
  // the run it actually describes.
  it("neg: a nameless INTERACTIVE run still reads as 'Interactive session'", async () => {
    renderRow({ title: undefined, task: "", workspace_path: undefined, interactive: true });
    expect(await screen.findByText("Interactive session")).toBeInTheDocument();
  });
});
