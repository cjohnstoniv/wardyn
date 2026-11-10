/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import type { RecordResult, SetupModelProvider, SetupStatus, Workspace } from "../../../lib/types";
import { OperatorProvider } from "../../wardyn/operator-context";
import { ModelAccessProvider } from "../../wardyn/model-access-context";
import { EGRESS, OPERATOR_ONLY_REASON, SECURITY_ONLY_REASON } from "../../wardyn/copy";

const getWorkspaceMock = vi.fn();
const deleteWorkspaceMock = vi.fn();
const setRequirementsMock = vi.fn();
const setWorkspaceLLMCredMock = vi.fn();
const recordTaskMock = vi.fn();
const promoteRecordEgressMock = vi.fn();
const setApprovedEgressMock = vi.fn();
const setDeniedEgressMock = vi.fn();
const buildWorkspaceMock = vi.fn();
vi.mock("../../../lib/api/workspaces", () => ({
  workspaces: {
    getWorkspace: (...a: unknown[]) => getWorkspaceMock(...a),
    deleteWorkspace: (...a: unknown[]) => deleteWorkspaceMock(...a),
    setRequirements: (...a: unknown[]) => setRequirementsMock(...a),
    setWorkspaceLLMCred: (...a: unknown[]) => setWorkspaceLLMCredMock(...a),
    recordTask: (...a: unknown[]) => recordTaskMock(...a),
    promoteRecordEgress: (...a: unknown[]) => promoteRecordEgressMock(...a),
    setApprovedEgress: (...a: unknown[]) => setApprovedEgressMock(...a),
    setDeniedEgress: (...a: unknown[]) => setDeniedEgressMock(...a),
    buildWorkspace: (...a: unknown[]) => buildWorkspaceMock(...a),
    createWorkspace: vi.fn(),
  },
}));
const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) },
}));
const killRunMock = vi.fn();
vi.mock("../../../lib/api/runs", () => ({ runs: { killRun: (...a: unknown[]) => killRunMock(...a) } }));
vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn(), info: vi.fn() } }));
const getAvailabilityMock = vi.fn();
const getMyCapabilitiesMock = vi.fn();
vi.mock("../../../lib/api/permissions", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/permissions")>("../../../lib/api/permissions");
  return {
    ...actual,
    permissions: {
      ...actual.permissions,
      getAvailability: (...a: unknown[]) => getAvailabilityMock(...a),
      getMyCapabilities: (...a: unknown[]) => getMyCapabilitiesMock(...a),
    },
  };
});

import { toast } from "sonner";
import { WorkspaceDetailScreen } from "./workspace-detail";
import { WORKSPACE_HOSTS } from "../../wardyn/copy/workspace-hosts";
import { HttpError } from "../../../lib/api/core";

function ws(over: Partial<Workspace> = {}): Workspace {
  return {
    id: "ws-1",
    name: "payments",
    kind: "repo",
    source: "acme/payments",
    ref: "main",
    status: "scanned",
    created_at: "",
    updated_at: "2026-01-02T03:04:05.123456Z",
    ...over,
  };
}

function setupStatus(overrides: Partial<SetupStatus> = {}): SetupStatus {
  return {
    ready: false,
    checks: [],
    auth: { mode: "local", local_loopback: true },
    runner: { driver: "docker", confinement_classes: ["CC1"] },
    providers: [],
    secrets: { present: [], github_app: false },
    age_key: { durable: false },
    has_runs: false,
    platform: { os: "linux", wsl: false, kvm: true },
    ...overrides,
  };
}

// Reads location.state so the CTA's contract with /runs (#10/D14: route
// state opens the New-Run dialog on arrival) is provable from THIS side
// without depending on the real RunsScreen or its own API surface.
function RunsRouteProbe() {
  const location = useLocation();
  const state = location.state as { openNewRun?: boolean } | null;
  return <div>runs screen{state?.openNewRun ? " (openNewRun)" : ""}</div>;
}

// modelProviders is undefined by default — no <ModelAccessProvider> mounted
// at all, the exact shape every case but #922's model-provider-arm ones
// renders (and behaviorally identical to a mounted Provider whose own status
// carries no model_providers key: both mean "no answer", resolvedModelProviders's
// own fail-open). Pass an array (including `[]`, a REAL loaded answer) to
// mount one, matching workspaces.test.tsx's own idiom.
function renderDetail(
  id = "ws-1",
  operator = true,
  securityOperator = operator,
  modelProviders?: SetupModelProvider[],
) {
  const screen = (
    <MemoryRouter initialEntries={[`/workspaces/${id}`]}>
      {/* 0.7 §B: the Sessions pane and both host cards moved to
          useSecurityOperator, so this fixture's viewer must be a MEMBER on
          both predicates; the workspace Delete/Rebuild controls it also
          asserts stay on useOperator. `securityOperator` defaults to
          `operator` so every existing caller is unchanged, and splits for the
          SECURITY-ADMIN persona (operator=false, securityOperator=true) — the
          caller F030 is about, which this fixture could not express. */}
      <OperatorProvider operator={operator} securityOperator={securityOperator}>
        <Routes>
          <Route path="/workspaces/:id" element={<WorkspaceDetailScreen />} />
          <Route path="/workspaces" element={<div>back on the list</div>} />
          <Route path="/runs" element={<RunsRouteProbe />} />
          <Route path="/runs/:id" element={<div>run detail screen</div>} />
        </Routes>
      </OperatorProvider>
    </MemoryRouter>
  );
  if (modelProviders === undefined) return render(screen);
  return render(
    <ModelAccessProvider status={setupStatus({ model_providers: modelProviders })} onRefresh={() => {}}>
      {screen}
    </ModelAccessProvider>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  getSetupStatusMock.mockResolvedValue(setupStatus());
  // Never settles by default, so the Availability card stays empty in every
  // test that isn't about it.
  getAvailabilityMock.mockReturnValue(new Promise(() => {}));
  // Same idiom, same reason (#922): a test that isn't about "Start a run"'s
  // own availability must render byte-identical to before this hook existed —
  // caps stays null forever, and capabilityAllowed's own fail-open default
  // (null caps => allowed) is what that renders as.
  getMyCapabilitiesMock.mockReturnValue(new Promise(() => {}));
});

// The F031 case below queues two mockResolvedValueOnce answers on
// getWorkspaceMock. vi.clearAllMocks() above clears calls/results, not a
// queued ONCE implementation, so a failure before both are consumed (e.g. the
// first findByTestId times out) would otherwise leave the second queued and
// leak it into the very next test's first getWorkspace() call, failing it for
// an unrelated reason. Reset it every time, pass or fail.
afterEach(() => {
  getWorkspaceMock.mockReset();
});

describe("WorkspaceDetailScreen — Allowed hosts card", () => {
  it("a repo's clone host is structural: shown, no remove control", async () => {
    getWorkspaceMock.mockResolvedValue(ws());
    renderDetail();
    expect(await screen.findByText("Allowed hosts · 1")).toBeInTheDocument();
    expect(screen.getByText("github.com")).toBeInTheDocument();
    expect(screen.getByText("clone host for this workspace")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /remove github.com/i })).not.toBeInTheDocument();
  });

  it("an approved host renders a remove control that PUTs the narrowed allowlist", async () => {
    getWorkspaceMock.mockResolvedValue(ws({ approved_egress: ["registry.npmjs.org"] }));
    setApprovedEgressMock.mockResolvedValue(ws({ approved_egress: [] }));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderDetail();
    expect(await screen.findByText("Allowed hosts · 2")).toBeInTheDocument();
    expect(screen.getByText("approved for this workspace")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Remove registry.npmjs.org" }));
    // Asks first, naming the host and the workspace; nothing is written until the confirm.
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText(WORKSPACE_HOSTS.ALLOWED_TITLE("registry.npmjs.org", "payments"))).toBeInTheDocument();
    // The write reaches new runs only; a run already started keeps its frozen rules, and the confirm says so.
    expect(within(dialog).getByText(/^New runs on this workspace can no longer reach this host.*Runs already started keep their current rules\.$/)).toBeInTheDocument();
    expect(setApprovedEgressMock).not.toHaveBeenCalled();
    await user.click(within(dialog).getByRole("button", { name: WORKSPACE_HOSTS.CONFIRM }));
    await waitFor(() => expect(setApprovedEgressMock).toHaveBeenCalledWith("ws-1", [], "2026-01-02T03:04:05.123456Z"));
    await waitFor(() => expect(toast.success).toHaveBeenCalledWith(WORKSPACE_HOSTS.REMOVED_ALLOWED("registry.npmjs.org")));
  });

  it("Cancel in the remove confirm writes nothing", async () => {
    getWorkspaceMock.mockResolvedValue(ws({ approved_egress: ["registry.npmjs.org"] }));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderDetail();
    await user.click(await screen.findByRole("button", { name: "Remove registry.npmjs.org" }));
    const dialog = await screen.findByRole("alertdialog");
    await user.click(within(dialog).getByRole("button", { name: WORKSPACE_HOSTS.CANCEL }));
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(setApprovedEgressMock).not.toHaveBeenCalled();
  });

  it("a host promoted from a recorded session is attributed to it by name", async () => {
    const rr: RecordResult = {
      run_id: "r1",
      label: "build-and-test",
      mode: "interactive",
      status: "recorded",
      egress_promoted: true,
      observations: {
        domains: [{ host: "pypi.org", allow_count: 1, deny_count: 0, pending_count: 0 }],
        minted_grant_ids: [],
      } as unknown as RecordResult["observations"],
    };
    getWorkspaceMock.mockResolvedValue(ws({ approved_egress: ["pypi.org"], record_results: { "build-test": rr } }));
    renderDetail();
    expect(await screen.findByText('promoted from session "build-and-test"')).toBeInTheDocument();
  });
});

describe("WorkspaceDetailScreen — Denied hosts card", () => {
  it("shows nothing denied by default", async () => {
    getWorkspaceMock.mockResolvedValue(ws());
    renderDetail();
    expect(await screen.findByText("Denied hosts · 0")).toBeInTheDocument();
    expect(screen.getByText("Nothing denied.")).toBeInTheDocument();
  });

  it("a denied host renders a remove control that PUTs the narrowed denylist", async () => {
    getWorkspaceMock.mockResolvedValue(ws({ denied_egress: ["evil.example.com"] }));
    setDeniedEgressMock.mockResolvedValue(ws({ denied_egress: [] }));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderDetail();
    expect(await screen.findByText("Denied hosts · 1")).toBeInTheDocument();
    expect(screen.getByText("denied for this workspace")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Remove evil.example.com" }));
    // Lifting a permanent block asks first, and says it is a block being lifted and not an approval.
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText(WORKSPACE_HOSTS.DENIED_TITLE("evil.example.com", "payments"))).toBeInTheDocument();
    expect(within(dialog).getByText(WORKSPACE_HOSTS.DENIED_BODY)).toBeInTheDocument();
    expect(within(dialog).getByText(/^New runs on this workspace stop being blocked.*Runs already started keep their current rules\.$/)).toBeInTheDocument();
    expect(setDeniedEgressMock).not.toHaveBeenCalled();
    await user.click(within(dialog).getByRole("button", { name: WORKSPACE_HOSTS.CONFIRM }));
    await waitFor(() => expect(setDeniedEgressMock).toHaveBeenCalledWith("ws-1", [], "2026-01-02T03:04:05.123456Z"));
    await waitFor(() => expect(toast.success).toHaveBeenCalledWith(WORKSPACE_HOSTS.REMOVED_DENIED("evil.example.com")));
  });
});

// F030 — removing an allowed host is up to TWO writes on TWO tiers: PUT
// .../approved-egress (securityOps) and, when an operator-authored
// requirements row backs the host, PUT .../requirements (operatorOnly). The
// card was gated on the security tier alone and treated the pair as atomic.
describe("WorkspaceDetailScreen — Allowed hosts, the two-tier remove", () => {
  const operatorSet = { level: "required" as const, provenance: "operator_set" as const };

  it("a security admin may not remove a host whose requirements row is operator_set, but may remove a plain approved one", async () => {
    getWorkspaceMock.mockResolvedValue(
      ws({
        approved_egress: ["registry.npmjs.org", "pypi.org"],
        requirements: { "egress:registry.npmjs.org": operatorSet },
      }),
    );
    renderDetail("ws-1", false, true);

    const blocked = await screen.findByRole("button", { name: "Remove registry.npmjs.org" });
    // Disabled because the SECOND write is operatorOnly — not because the card
    // is closed to this caller: the sibling row proves the card is live.
    expect(blocked).toBeDisabled();
    expect(blocked).toHaveAccessibleDescription(OPERATOR_ONLY_REASON);
    expect(screen.getByRole("button", { name: "Remove pypi.org" })).not.toBeDisabled();
  });

  it("publishes the landed allowlist write and reports a failed requirements clear as PARTIAL, not as a total failure", async () => {
    getWorkspaceMock.mockResolvedValue(
      ws({
        approved_egress: ["registry.npmjs.org"],
        requirements: { "egress:registry.npmjs.org": operatorSet },
      }),
    );
    // The first PUT LANDS: the server has already dropped the host from the
    // allowlist, and its requirements row is all that still holds it.
    setApprovedEgressMock.mockResolvedValue(
      ws({ approved_egress: [], requirements: { "egress:registry.npmjs.org": operatorSet } }),
    );
    setRequirementsMock.mockRejectedValue(new Error("operator role required"));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderDetail();

    expect(await screen.findByText("Allowed hosts · 2")).toBeInTheDocument();
    expect(screen.getByText("approved for this workspace")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Remove registry.npmjs.org" }));
    await user.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: WORKSPACE_HOSTS.CONFIRM }));

    await waitFor(() => expect(setRequirementsMock).toHaveBeenCalled());
    // The landed half was published: the row re-renders from the FIRST write's
    // workspace, so its provenance is now the requirements row, not the
    // allowlist the server has already dropped it from.
    expect(await screen.findByText("required by this workspace")).toBeInTheDocument();
    expect(screen.queryByText("approved for this workspace")).toBeNull();
    // ...and the operator is told which half failed.
    expect(toast.error).toHaveBeenCalledWith(
      "Removed registry.npmjs.org from the allowlist, but its required-host row could not be cleared",
      { description: "operator role required" },
    );
    expect(toast.error).not.toHaveBeenCalledWith("Failed to remove registry.npmjs.org", expect.anything());
    expect(toast.success).not.toHaveBeenCalledWith(WORKSPACE_HOSTS.REMOVED_ALLOWED("registry.npmjs.org"));
  });
});

// A save the server refuses as stale (412) changed nothing: the list is reloaded and shows what the
// server holds, here the host an "always" decision added since this page read the workspace.
describe("a stale host-list save", () => {
  it.each([
    ["approved", "approved_egress", "Allowed hosts", setApprovedEgressMock, "registry.npmjs.org"],
    ["denied", "denied_egress", "Denied hosts", setDeniedEgressMock, "evil.example.com"],
  ] as const)("%s: answers 412, applies nothing, and reloads the workspace", async (_kind, field, title, mock, host) => {
    const decided = "decided.example.com";
    getWorkspaceMock.mockResolvedValueOnce(ws({ [field]: [host] }));
    mock.mockRejectedValue(new HttpError(412, "The workspace changed while this edit was being saved", "workspace_update_conflict"));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderDetail();
    await user.click(await screen.findByRole("button", { name: `Remove ${host}` }));
    // The reload returns the list as the server holds it now.
    getWorkspaceMock.mockResolvedValue(ws({ [field]: [host, decided], updated_at: "2026-01-02T03:04:06.000000Z" }));
    await user.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: WORKSPACE_HOSTS.CONFIRM }));
    await waitFor(() => expect(mock).toHaveBeenCalledWith("ws-1", [], "2026-01-02T03:04:05.123456Z"));
    expect(toast.error).toHaveBeenCalledWith(`Failed to remove ${host}`, { description: "The workspace changed while this edit was being saved" });
    expect(toast.success).not.toHaveBeenCalled();
    expect(await screen.findByText(decided)).toBeInTheDocument();
    expect(screen.getByText(host)).toBeInTheDocument();
    expect(screen.getByText(new RegExp(`^${title} · `))).toBeInTheDocument();
  });
});

// F5-F3 — Remove must be disabled for a row the remove path cannot reach.
// Removing a host is PUT .../approved-egress plus, for an operator-authored
// requirements row, PUT .../requirements — and that second write reads the
// workspace's OWN overlay, while the row's provenance is read off the
// EFFECTIVE fold. A scan_seeded row is in neither: removing it would fire two
// no-op writes and the row would come straight back.
describe("WorkspaceDetailScreen — Allowed hosts, removable means the remove path can reach it", () => {
  it("parks a scan_seeded-only row with a reason, and leaves an approved host live", async () => {
    getWorkspaceMock.mockResolvedValue(
      ws({
        approved_egress: ["pypi.org"],
        requirements: { "egress:proxy.golang.org": { level: "required", provenance: "scan_seeded" } },
      }),
    );
    renderDetail();

    const parked = await screen.findByRole("button", { name: "Remove proxy.golang.org" });
    expect(parked).toBeDisabled();
    expect(parked).toHaveAccessibleDescription(EGRESS.SCAN_SEEDED_REASON);
    expect(screen.getByRole("button", { name: "Remove pypi.org" })).not.toBeDisabled();
  });

  it("negative control: the approved host still PUTs the narrowed allowlist", async () => {
    getWorkspaceMock.mockResolvedValue(ws({ approved_egress: ["pypi.org", "registry.npmjs.org"] }));
    setApprovedEgressMock.mockResolvedValue(ws({ approved_egress: ["registry.npmjs.org"] }));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderDetail();

    await user.click(await screen.findByRole("button", { name: "Remove pypi.org" }));
    await user.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: WORKSPACE_HOSTS.CONFIRM }));
    await waitFor(() =>
      expect(setApprovedEgressMock).toHaveBeenCalledWith("ws-1", ["registry.npmjs.org"], "2026-01-02T03:04:05.123456Z"),
    );
  });

  // The other half of the overlay/fold mismatch: a row whose operator_set
  // requirement is INHERITED (in the effective fold, absent from the
  // workspace's own overlay — a source contract this workspace composes) must
  // be parked for the right reason, but it is not a scan row — saying so would
  // put two contradictory provenances on one row ("required by this workspace"
  // beside "detected by this workspace's scan").
  it("an INHERITED operator_set row is parked as inherited, not as a scan row", async () => {
    getWorkspaceMock.mockResolvedValue(
      ws({
        approved_egress: [],
        requirements: {},
        effective_requirements: { "egress:corp.internal": { level: "required", provenance: "operator_set" } },
      }),
    );
    renderDetail();

    const parked = await screen.findByRole("button", { name: "Remove corp.internal" });
    expect(parked).toBeDisabled();
    expect(parked).toHaveAccessibleDescription(EGRESS.INHERITED_REASON);
    expect(parked).not.toHaveAccessibleDescription(EGRESS.SCAN_SEEDED_REASON);
    // The row's own sentence and its parked reason agree.
    expect(screen.getByText("required by this workspace")).toBeInTheDocument();
  });

  it("a member (whose egress: keys the server drops) is told the tier, not handed a dead X", async () => {
    getWorkspaceMock.mockResolvedValue(ws({ approved_egress: ["pypi.org"] }));
    renderDetail("ws-1", false, false);
    const parked = await screen.findByRole("button", { name: "Remove pypi.org" });
    expect(parked).toBeDisabled();
    expect(parked).toHaveAccessibleDescription(SECURITY_ONLY_REASON);
  });

  // The Denied twin renders the same HostList behind the same securityOperator
  // gate — a member must not get a bare disabled X there while the Allowed
  // card beside it says why. Same reason, same tier.
  it("the Denied hosts twin says the same thing to the same caller", async () => {
    getWorkspaceMock.mockResolvedValue(ws({ denied_egress: ["evil.example.com"] }));
    renderDetail("ws-1", false, false);
    const parked = await screen.findByRole("button", { name: "Remove evil.example.com" });
    expect(parked).toBeDisabled();
    expect(parked).toHaveAccessibleDescription(SECURITY_ONLY_REASON);
  });
});
