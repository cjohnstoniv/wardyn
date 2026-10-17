/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Identity card says when, and why, a run is not recorded (Go: AgentRun.NotRecorded), and says
// nothing for a recorded run.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import type { AgentRun } from "../../../../lib/types";

const healthMock = vi.fn();
vi.mock("../../../../lib/api/health", () => ({ health: { health: (...a: unknown[]) => healthMock(...a) } }));
const listAuditMock = vi.fn();
vi.mock("../../../../lib/api/audit", async (importOriginal) => {
  const m = await importOriginal<Record<string, unknown>>();
  return { ...m, audit: { ...(m.audit as object), listAudit: (...a: unknown[]) => listAuditMock(...a) } };
});

import { IdentityWidget } from "./identity";
import { RUN_RECORDING } from "../../../wardyn/copy/runners";

beforeEach(() => {
  healthMock.mockReset().mockResolvedValue({ status: "ok" });
  listAuditMock.mockReset().mockResolvedValue([]);
});

const run: AgentRun = {
  id: "run-1",
  created_at: new Date().toISOString(),
  updated_at: new Date().toISOString(),
  created_by: "me",
  agent: "claude-code",
  repo: "acme/widgets",
  task: "Fix the flaky auth tests",
  confinement_class: "CC2",
  state: "RUNNING",
  spiffe_id: "spiffe://x",
  runner_target: "docker",
  placement: "local",
};

describe("IdentityWidget recording row", () => {
  it.each([
    ["org_off", RUN_RECORDING.org_off],
    ["runner_declined", RUN_RECORDING.runner_declined],
  ] as const)("says why a %s run is not recorded", (reason, text) => {
    render(<IdentityWidget run={{ ...run, not_recorded: reason }} onGoPolicy={() => {}} />);
    expect(screen.getByText(RUN_RECORDING.LABEL)).toBeInTheDocument();
    expect(screen.getByText(text)).toBeInTheDocument();
  });

  it("shows no recording row for a recorded run", () => {
    render(<IdentityWidget run={run} onGoPolicy={() => {}} />);
    expect(screen.queryByText(RUN_RECORDING.LABEL)).not.toBeInTheDocument();
  });
});
