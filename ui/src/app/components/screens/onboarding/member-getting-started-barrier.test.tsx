/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The barrier chip's governance filtering — split out of
// member-getting-started.test.tsx, which was already at the
// check-file-size.sh ceiling. Its own copy of the page's mock harness, same
// shape as new-run-screen-barrier.test.tsx's.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import type { SetupStatus } from "../../../lib/types";
import { WithDoor } from "../../../../test/door-harness";
import { baseMe, baseStatus } from "../../../lib/test-fixtures";

const startSignInMock = vi.fn();
vi.mock("../../../lib/api/model-provider-signin", () => ({
  modelProviderSignIn: {
    startSignIn: (id: string) => {
      startSignInMock(id);
      return new Promise(() => {});
    },
    captureSignIn: vi.fn(),
  },
}));

const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) },
}));

const adoConnectMock = vi.fn();
vi.mock("../../../lib/hooks/use-ado-connect", () => ({
  useAdoConnect: () => ({ connecting: false, connect: adoConnectMock, connectFallback: adoConnectMock, blockedUrl: null }),
}));

const listSecretsMineMock = vi.fn();
vi.mock("../../../lib/api/secrets", () => ({
  secrets: {
    listSecretsMine: (...a: unknown[]) => listSecretsMineMock(...a),
    setSecret: vi.fn(),
    deleteSecret: vi.fn(),
  },
}));

const listRunsMock = vi.fn();
vi.mock("../../../lib/api/runs", () => ({
  runs: {
    listRuns: (...a: unknown[]) => listRunsMock(...a),
    createRun: vi.fn(),
    getRun: vi.fn(),
    killRun: vi.fn(),
    getGrants: vi.fn(),
  },
}));

vi.mock("../../attach-terminal", () => ({
  AttachTerminal: ({ runId }: { runId: string }) => <div data-testid="attach-terminal">{runId}</div>,
}));
vi.mock("../../wardyn/live-approvals", () => ({
  LiveApprovals: ({ runId }: { runId: string }) => <div data-testid="live-approvals">{runId}</div>,
}));
const listAuditMock = vi.fn();
vi.mock("../../../lib/api/audit", () => ({
  audit: { listAudit: (...a: unknown[]) => listAuditMock(...a) },
  egressFromAudit: () => [],
  demoAuditRows: () => [],
}));
vi.mock("../profile-review", () => ({
  ProfileReview: ({ runId }: { runId: string | null }) =>
    runId ? <div data-testid="profile-review">{runId}</div> : null,
}));
vi.mock("sonner", () => ({ toast: { error: vi.fn() } }));

const listKeysMock = vi.fn();
vi.mock("../../../lib/api/ssh-keys", () => ({
  sshKeys: { listKeys: (...a: unknown[]) => listKeysMock(...a) },
}));

const listWorkspacesMock = vi.fn();
vi.mock("../../../lib/api/workspaces", () => ({
  workspaces: { listWorkspaces: (...a: unknown[]) => listWorkspacesMock(...a), scanWorkspace: vi.fn() },
}));

const getDefaultPolicyMock = vi.fn();
vi.mock("../../../lib/api/policies", () => ({
  policies: { getDefaultPolicy: (...a: unknown[]) => getDefaultPolicyMock(...a) },
}));

import { MemberGettingStarted } from "./member-getting-started";
import { OperatorProvider } from "../../wardyn/operator-context";
import { ViewAccessProvider } from "../../wardyn/console-view";

function status(overrides: Partial<SetupStatus> = {}): SetupStatus {
  return baseStatus({
    ready: true,
    llm_ready: false,
    runner: { driver: "docker", confinement_classes: ["CC1"] },
    auth: { mode: "local", local_loopback: true },
    ...overrides,
  });
}

function renderPage() {
  const me = baseMe();
  return render(
    <WithDoor status={null} path="/setup" operator={false}>
      <ViewAccessProvider value="url">
        <OperatorProvider
          operator={false}
          securityOperator={false}
          userDrive={me.user_drive}
          userDriveDeniedByProfile={me.user_drive_denied_by_profile}
          userType={me.user_type ?? null}
        >
          <MemberGettingStarted />
        </OperatorProvider>
      </ViewAccessProvider>
    </WithDoor>,
  );
}

beforeEach(() => {
  localStorage.clear();
  getSetupStatusMock.mockReset().mockResolvedValue(status());
  listSecretsMineMock.mockReset().mockResolvedValue({ names: [], mine: [] });
  listRunsMock.mockReset().mockResolvedValue([]);
  listKeysMock.mockReset().mockResolvedValue([]);
  listWorkspacesMock.mockReset().mockResolvedValue([]);
  getDefaultPolicyMock.mockReset().mockResolvedValue({ min_confinement_class: "CC1" });
});

// #1200 — the barrier chip is installed ∧ allowed (visibleTiers), never the
// strongest INSTALLED tier alone: a floor this member's admin set narrows
// what the chip may claim, the same rule every other picker follows.
describe("MemberGettingStarted — the barrier chip respects the governance floor (T-9)", () => {
  it("a Wall-only floor drops Fence from the chip's own candidates, showing the strongest ALLOWED tier", async () => {
    getSetupStatusMock.mockResolvedValue(
      status({ runner: { driver: "docker", confinement_classes: ["CC1", "CC2"] } }),
    );
    getDefaultPolicyMock.mockResolvedValue({ min_confinement_class: "CC2", governance_profile_name: "walled" });
    renderPage();
    expect(await screen.findByText("Barrier · Wall")).toBeInTheDocument();
    expect(screen.queryByText("Barrier · Fence")).not.toBeInTheDocument();
  });

  // Review P2-4: the blocked case renders the SAME shared requirement card
  // every other TierPicker consumer shows for T-9, with a VISIBLE line —
  // never a hover-only tooltip (invisible on touch and to most AT) behind a
  // non-canon "blocked" chip.
  it("a floor this host can't build at all shows the requirement card with a visible line, never a tooltip-only chip", async () => {
    getSetupStatusMock.mockResolvedValue(
      status({ runner: { driver: "docker", confinement_classes: ["CC1", "CC2"] } }),
    );
    getDefaultPolicyMock.mockResolvedValue({ min_confinement_class: "CC3", governance_profile_name: "vault-required" });
    renderPage();
    expect(
      await screen.findByText(/Your admin requires Vault, and this host can't run it/),
    ).toBeInTheDocument();
    expect(screen.queryByText("Barrier · blocked")).not.toBeInTheDocument();
    // Never the false claim "Wall" — the strongest tier the HOST has, but
    // not one this member's ceiling allows.
    expect(screen.queryByText("Barrier · Wall")).not.toBeInTheDocument();
  });

  it("no floor at all shows the strongest installed tier, unrestricted", async () => {
    getSetupStatusMock.mockResolvedValue(
      status({ runner: { driver: "docker", confinement_classes: ["CC1", "CC2"] } }),
    );
    getDefaultPolicyMock.mockResolvedValue({});
    renderPage();
    expect(await screen.findByText("Barrier · Wall")).toBeInTheDocument();
  });
});
