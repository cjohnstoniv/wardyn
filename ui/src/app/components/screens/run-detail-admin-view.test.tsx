/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// M-7's Check — "the monitor has no relaunch or door; the member owner door" —
// pinned where the screen wires it: RunDetailScreen keys the relaunch, the
// failure block and the reauth row on the ROUTE it is mounted under, so each
// case mounts the same run at /admin/runs/:id and at /runs/:id. Its own file:
// run-detail.test.tsx is at the size gate.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import type { ApprovalRequest, RunEnding } from "../../lib/types";

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
const listApprovalsMock = vi.fn();
vi.mock("../../lib/api/approvals", () => ({
  approvals: { listApprovals: (...a: unknown[]) => listApprovalsMock(...a), approve: vi.fn(), deny: vi.fn() },
}));
const ending = vi.hoisted(() => ({ value: undefined as RunEnding | undefined }));
vi.mock("../../lib/api/audit", async (importOriginal) => ({
  audit: { listAudit: vi.fn().mockResolvedValue([]) },
  egressFromAudit: () => [],
  exitCodeFromAudit: (await importOriginal<typeof import("../../lib/api/audit")>()).exitCodeFromAudit,
  createRequestFromAudit: () => ({}),
  runEndingFromAudit: () => ending.value,
}));
vi.mock("../../lib/api/recordings", () => ({ recordings: { getRecording: vi.fn().mockResolvedValue(null) } }));
vi.mock("../../lib/api/health", () => ({ health: { health: vi.fn().mockResolvedValue({}) } }));
vi.mock("sonner", () => ({ toast: { warning: vi.fn(), error: vi.fn(), success: vi.fn() } }));

import { RunDetailScreen } from "./run-detail";
import { OperatorProvider } from "../wardyn/operator-context";
import { ModelAccessProvider } from "../wardyn/model-access-context";
import { RUN } from "../wardyn/copy";
import { MODEL_ACCESS_RUN_DOOR, REAUTH_ROW } from "../wardyn/model-access-copy";
import { OPEN_IN_USER_VIEW } from "../wardyn/copy/console-view";
import { baseStatus } from "../../lib/test-fixtures";

const ME = "admin@corp";
const RUN_ROW = {
  id: "run-1",
  created_at: new Date().toISOString(),
  updated_at: new Date().toISOString(),
  created_by: ME,
  agent: "claude-code",
  repo: "acme/widgets",
  task: "audit the egress proxy",
  confinement_class: "CC2",
  state: "RUNNING",
  spiffe_id: "spiffe://wardyn.local/agent-run/run-1",
  runner_target: "docker",
  interactive: false,
};

// The admin's own sign-in is dead on a per-user Bedrock lane — the state in
// which the User view offers the owner the door.
const STATUS = baseStatus({
  model_access: { state: "expired_signin", action: "Sign in to AWS" },
  harnesses: [
    {
      id: "claude-code",
      display: "claude-code",
      has_gateway: false,
      has_login: true,
      enabled: true,
      mechanism: "bedrock_sso",
      credential_source: "per_user",
    },
  ],
});

function mount(path: string, run: Record<string, unknown>) {
  getRunMock.mockResolvedValue(run);
  return render(
    <MemoryRouter initialEntries={[path]}>
      <OperatorProvider operator principal={ME}>
        <ModelAccessProvider status={STATUS} onRefresh={() => {}}>
          <Routes>
            <Route path="/runs/:id" element={<RunDetailScreen />} />
            <Route path="/admin/runs/:id" element={<RunDetailScreen />} />
          </Routes>
        </ModelAccessProvider>
      </OperatorProvider>
    </MemoryRouter>,
  );
}

const switchLink = () => screen.queryByRole("button", { name: OPEN_IN_USER_VIEW });

beforeEach(() => {
  vi.clearAllMocks();
  ending.value = undefined;
  listApprovalsMock.mockResolvedValue([]);
});

describe("RunDetailScreen at /admin/runs/:id — the monitor (M-7)", () => {
  it("a finished run of the admin's own has no relaunch", async () => {
    mount("/admin/runs/run-1", { ...RUN_ROW, state: "COMPLETED" });
    await screen.findByRole("heading", { name: RUN_ROW.task, level: 1 });
    expect(screen.queryByRole("button", { name: RUN.CLONE_CTA })).not.toBeInTheDocument();
  });

  it("negative control: the same run at /runs/:id keeps it", async () => {
    mount("/runs/run-1", { ...RUN_ROW, state: "COMPLETED" });
    expect(await screen.findByRole("button", { name: RUN.CLONE_CTA })).toBeInTheDocument();
  });

  it("a held per-user sign-in on the admin's own run: no door, the switch link instead", async () => {
    listApprovalsMock.mockResolvedValue([heldReauth()]);
    mount("/admin/runs/run-1", RUN_ROW);
    const panel = await screen.findByTestId("live-approvals");
    expect(await within(panel).findByText(REAUTH_ROW.notYoursHint(ME))).toBeInTheDocument();
    expect(within(panel).queryByRole("button", { name: REAUTH_ROW.ariaLabel })).not.toBeInTheDocument();
    expect(within(panel).getByRole("button", { name: OPEN_IN_USER_VIEW })).toBeInTheDocument();
  });

  it("the owner door: the same held run at /runs/:id offers the owner the sign-in", async () => {
    listApprovalsMock.mockResolvedValue([heldReauth()]);
    mount("/runs/run-1", RUN_ROW);
    const panel = await screen.findByTestId("live-approvals");
    expect(await within(panel).findByRole("button", { name: REAUTH_ROW.ariaLabel })).toBeInTheDocument();
    expect(switchLink()).not.toBeInTheDocument();
  });

  it("a credential ending on the admin's own run: no door, the switch link instead", async () => {
    ending.value = { kind: "credential", action: "run.create", outcome: "failure", mechanism: "bedrock_sso" };
    mount("/admin/runs/run-1", { ...RUN_ROW, state: "FAILED" });
    const block = await screen.findByTestId("run-failure-block");
    expect(within(block).queryByRole("button", { name: MODEL_ACCESS_RUN_DOOR.SIGN_IN_ARIA })).not.toBeInTheDocument();
    expect(within(block).getByRole("button", { name: OPEN_IN_USER_VIEW })).toBeInTheDocument();
  });

  it("the owner door: the same failed run at /runs/:id offers the sign-in", async () => {
    ending.value = { kind: "credential", action: "run.create", outcome: "failure", mechanism: "bedrock_sso" };
    mount("/runs/run-1", { ...RUN_ROW, state: "FAILED" });
    const block = await screen.findByTestId("run-failure-block");
    expect(within(block).getByRole("button", { name: MODEL_ACCESS_RUN_DOOR.SIGN_IN_ARIA })).toBeInTheDocument();
    expect(switchLink()).not.toBeInTheDocument();
  });
});

function heldReauth(): ApprovalRequest {
  return {
    id: "reauth-1",
    run_id: "run-1",
    kind: "credential_reauth",
    requested_scope: { mechanism: "bedrock_sso", credential_source: "per_user", owner: ME },
    state: "PENDING",
    requested_at: new Date().toISOString(),
  } as ApprovalRequest;
}
