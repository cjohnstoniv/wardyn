/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// fix: "Run not found" only ever listed archived / deleted / stale-link as
// possible reasons — never "you don't have access" — even though the Copy
// link feature two lines above (copyLink) routinely produces exactly that
// case: a member pastes a coworker's copied run link and hits this state
// because they lack access, not because the run is gone. This pins that the
// description now names that reason too (without disambiguating which case
// applies — preserves the anti-enumeration property).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

const getRunMock = vi.fn();
vi.mock("../../lib/api/runs", () => ({
  runs: {
    getRun: (...a: unknown[]) => getRunMock(...a),
    getGrants: vi.fn().mockResolvedValue([]),
    killRun: vi.fn(),
  },
}));
vi.mock("../../lib/api/approvals", () => ({
  approvals: {
    listApprovals: vi.fn().mockResolvedValue([]),
    approve: vi.fn(),
    deny: vi.fn(),
  },
}));
vi.mock("../../lib/api/audit", () => ({
  audit: { listAudit: vi.fn().mockResolvedValue([]) },
  egressFromAudit: () => [],
  exitCodeFromAudit: () => undefined,
}));
vi.mock("../../lib/api/recordings", () => ({
  recordings: { getRecording: vi.fn() },
}));
vi.mock("../../lib/api/health", () => ({
  health: { health: vi.fn().mockResolvedValue({}) },
}));

import { RunDetailScreen } from "./run-detail";

beforeEach(() => {
  getRunMock.mockReset();
});

describe("RunDetailScreen — 'Run not found' names lack-of-access as a real reason", () => {
  it("mentions not having access alongside archived/deleted/stale-link", async () => {
    getRunMock.mockResolvedValue(undefined);
    render(
      <MemoryRouter initialEntries={["/runs/run-1"]}>
        <Routes>
          <Route path="/runs/:id" element={<RunDetailScreen />} />
        </Routes>
      </MemoryRouter>,
    );

    expect(await screen.findByText("Run not found")).toBeInTheDocument();
    expect(screen.getByText(/don't have access/i)).toBeInTheDocument();
  });
});
