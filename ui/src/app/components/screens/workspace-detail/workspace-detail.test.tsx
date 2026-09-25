/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes, useLocation, useNavigate } from "react-router-dom";
import type { RecordResult, SetupStatus, Workspace } from "../../../lib/types";
import { OperatorProvider } from "../../wardyn/operator-context";
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

import { toast } from "sonner";
import { WorkspaceDetailScreen } from "./workspace-detail";

function ws(over: Partial<Workspace> = {}): Workspace {
  return {
    id: "ws-1",
    name: "payments",
    kind: "repo",
    source: "acme/payments",
    ref: "main",
    status: "scanned",
    created_at: "",
    updated_at: "",
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

function renderDetail(id = "ws-1", operator = true, securityOperator = operator) {
  return render(
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
    </MemoryRouter>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  getSetupStatusMock.mockResolvedValue(setupStatus());
});

describe("WorkspaceDetailScreen — not found", () => {
  it("shows a not-found state and navigates back to the list", async () => {
    getWorkspaceMock.mockResolvedValue(undefined);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderDetail();
    expect(await screen.findByText(/workspace not found/i)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /back to workspaces/i }));
    expect(await screen.findByText("back on the list")).toBeInTheDocument();
  });
});

describe("WorkspaceDetailScreen — header: name, source line, and Start a run", () => {
  it("shows the muted mono kind · source · ref line and offers Start a run unconditionally — no scan/status gating", async () => {
    getWorkspaceMock.mockResolvedValue(ws());
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderDetail();
    expect(await screen.findByRole("heading", { name: "payments" })).toBeInTheDocument();
    expect(screen.getByText("repo · acme/payments · main")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /^start a run$/i }));
    expect(await screen.findByText("runs screen (openNewRun)")).toBeInTheDocument();
  });
});

describe("WorkspaceDetailScreen — image row: three honest variants, never a /build fetch to find out", () => {
  it("standard sandbox image: no base_image, nothing detected — no Rebuild action", async () => {
    getWorkspaceMock.mockResolvedValue(ws());
    renderDetail();
    expect(await screen.findByText("standard sandbox image")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Rebuild" })).not.toBeInTheDocument();
  });

  it("devcontainer.json: the scan detected one — Rebuild calls buildWorkspace", async () => {
    getWorkspaceMock.mockResolvedValue(ws({ profile: { has_devcontainer: true } as unknown as Record<string, unknown> }));
    buildWorkspaceMock.mockResolvedValue({ state: "building" });
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderDetail();
    expect(await screen.findByText(".devcontainer/devcontainer.json (this repo, @main)")).toBeInTheDocument();
    expect(screen.getByText("Built as written — we don't modify it.")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Rebuild" }));
    await waitFor(() => expect(buildWorkspaceMock).toHaveBeenCalledWith("ws-1"));
  });

  it("pinned ref: an explicit base_image — no Rebuild action, the ref renders mono", async () => {
    getWorkspaceMock.mockResolvedValue(ws({ base_image: { kind: "byo", image: "ghcr.io/acme/dev@sha256:ab12" } }));
    renderDetail();
    expect(await screen.findByText("ghcr.io/acme/dev@sha256:ab12")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Rebuild" })).not.toBeInTheDocument();
  });
});

describe("WorkspaceDetailScreen — Delete workspace", () => {
  it("deletes and navigates back to the list", async () => {
    getWorkspaceMock.mockResolvedValue(ws());
    deleteWorkspaceMock.mockResolvedValue(undefined);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderDetail();
    await user.click(await screen.findByRole("button", { name: /delete this workspace/i }));
    await user.click(await screen.findByRole("button", { name: /^delete workspace$/i }));
    await waitFor(() => expect(deleteWorkspaceMock).toHaveBeenCalledWith("ws-1"));
    expect(await screen.findByText("back on the list")).toBeInTheDocument();
  });

  it("a viewer sees the trigger disabled", async () => {
    getWorkspaceMock.mockResolvedValue(ws());
    renderDetail("ws-1", false);
    expect(await screen.findByRole("button", { name: /delete this workspace/i })).toBeDisabled();
  });
});

describe("WorkspaceDetailScreen — three cards only", () => {
  it("renders 'Recorded sessions' (renamed from 'Sessions'), 'Allowed hosts', and 'Denied hosts', never the retired Requirements/Detected/Env-as-code cards", async () => {
    getWorkspaceMock.mockResolvedValue(ws());
    renderDetail();
    expect(await screen.findByText("Recorded sessions")).toBeInTheDocument();
    expect(screen.getByText(/Run a task once with everything open/)).toBeInTheDocument();
    expect(screen.getByText("Allowed hosts · 1")).toBeInTheDocument();
    expect(screen.getByText("Denied hosts · 0")).toBeInTheDocument();
    expect(screen.queryByText("Requirements")).not.toBeInTheDocument();
    expect(screen.queryByText("Detected, not required")).not.toBeInTheDocument();
    expect(screen.queryByText("Env as code")).not.toBeInTheDocument();
  });

  // ui-wsDetail-4: the header's source-path CopyButton must say what it
  // copies — a bare "Copy" accessible name doesn't tell a screen-reader user
  // what's on their clipboard.
  it("the source-path CopyButton's accessible name says what it copies", async () => {
    getWorkspaceMock.mockResolvedValue(ws());
    renderDetail();
    await screen.findByText("Recorded sessions");
    expect(screen.getByRole("button", { name: "Copy source path" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^copy$/i })).not.toBeInTheDocument();
  });
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
    await waitFor(() => expect(setApprovedEgressMock).toHaveBeenCalledWith("ws-1", []));
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
    await waitFor(() => expect(setDeniedEgressMock).toHaveBeenCalledWith("ws-1", []));
  });
});

// Ported from the retired import-panel.test.tsx's "egress approvals confirm
// before applying" coverage — a session's one-click "Approve N observed
// hosts" is untrusted-content-derived (a run's own observed traffic, not
// something the operator typed), so it must gate behind the same confirm the
// rest of the app uses before actually promoting it.
describe("WorkspaceDetailScreen — a session's egress promotion confirms before applying", () => {
  it("does not call promoteRecordEgress until the confirm dialog is accepted", async () => {
    const rr: RecordResult = {
      run_id: "r1",
      label: "build & test",
      mode: "interactive",
      status: "recorded",
      observations: {
        domains: [{ host: "evil.example.com", allow_count: 1, deny_count: 0, pending_count: 0 }],
        minted_grant_ids: [],
      } as unknown as RecordResult["observations"],
    };
    // A host approved EARLIER is in the workspace's allowlist but was not
    // observed by this recording. The retired 404-fallback computed
    // approved ∪ observed and PUT that whole union; the third argument is now
    // the promote SUBSET the confirm approved, so this host must not appear.
    getWorkspaceMock.mockResolvedValue(
      ws({ record_results: { "build-test": rr }, approved_egress: ["already.example.com"] }),
    );
    promoteRecordEgressMock.mockResolvedValue(ws({}));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderDetail();

    await user.click(await screen.findByRole("button", { name: /approve 1 observed host/i }));
    expect(await screen.findByText(/approve egress to evil\.example\.com/i)).toBeInTheDocument();
    expect(promoteRecordEgressMock).not.toHaveBeenCalled();

    await user.click(screen.getByRole("button", { name: /approve host/i }));
    await waitFor(() =>
      expect(promoteRecordEgressMock).toHaveBeenCalledWith("ws-1", "build-test", ["evil.example.com"]),
    );
  });

  // requestPromoteEgress must subtract both ws.approved_egress and
  // profile.egress_domains, matching session-helpers.ts's newEgressHosts
  // (which drives the button's own count) — otherwise the confirm could list
  // an auto-allowed host the button never asked about. The bulk list is not
  // read-only either: it checkboxes, and only the checked subset is posted
  // (the server's own optional {"hosts":[…]} field).
  it("the confirm lists exactly what the button counted, and promotes only the hosts left checked", async () => {
    const rr: RecordResult = {
      run_id: "r1",
      label: "build & test",
      mode: "interactive",
      status: "recorded",
      observations: {
        domains: [
          { host: "proxy.golang.org", allow_count: 1, deny_count: 0, pending_count: 0 },
          { host: "nexus.corp.internal", allow_count: 1, deny_count: 0, pending_count: 0 },
          { host: "files.pythonhosted.org", allow_count: 1, deny_count: 0, pending_count: 0 },
        ],
        minted_grant_ids: [],
      } as unknown as RecordResult["observations"],
    };
    getWorkspaceMock.mockResolvedValue(
      ws({
        record_results: { "build-test": rr },
        profile: { egress_domains: ["proxy.golang.org"] } as unknown as Record<string, unknown>,
      }),
    );
    promoteRecordEgressMock.mockResolvedValue(ws({}));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderDetail();

    // The button already excludes the auto-allowed host from its own count.
    await user.click(await screen.findByRole("button", { name: /approve 2 observed hosts/i }));
    const dialog = within(await screen.findByRole("alertdialog"));
    expect(dialog.getByText(/nexus\.corp\.internal/)).toBeInTheDocument();
    expect(dialog.queryByText(/proxy\.golang\.org/)).not.toBeInTheDocument();

    // Everything starts checked; unchecking one must drop it from the POST.
    await user.click(dialog.getByRole("checkbox", { name: /approve files\.pythonhosted\.org/i }));
    await user.click(dialog.getByRole("button", { name: /approve 1 host$/i }));
    await waitFor(() =>
      expect(promoteRecordEgressMock).toHaveBeenCalledWith("ws-1", "build-test", ["nexus.corp.internal"]),
    );
  });
});

// This screen never read useOperator at all — a viewer saw enabled
// Delete/lane toggles/record controls that all 403 server-side.
describe("WorkspaceDetailScreen — a viewer's write controls are disabled", () => {
  it("disables the Sessions card's session controls, the Allowed hosts remove control, and the Denied hosts remove control", async () => {
    getWorkspaceMock.mockResolvedValue(
      ws({
        approved_egress: ["registry.npmjs.org"],
        denied_egress: ["evil.example.com"],
        record_results: {
          "build-test": { run_id: "r1", label: "build & test", mode: "interactive", status: "recorded" },
        },
      }),
    );
    renderDetail("ws-1", false);

    await screen.findByText("Recorded sessions");
    // Unconfounded by any OTHER disabled condition (busyTask, empty name):
    // proves the operator gate itself, not something else that happens to
    // already disable the control.
    expect(screen.getByLabelText(/session name/i)).toBeDisabled();
    expect(screen.getByRole("button", { name: /re-record/i })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Remove registry.npmjs.org" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Remove evil.example.com" })).toBeDisabled();
  });

  // 0.7 §B: PUT .../approved-egress and .../denied-egress register on
  // securityOps (routes.go:363,368) — a security admin decides which hosts a
  // workspace's runs may reach. The workspace Delete button beside them stays
  // super-only (ownsWorkspaceOrAdmin is still isOperator), so this asserts BOTH
  // halves of the split on one render.
  it("leaves the host-list removes live for a security admin, but not the workspace Delete", async () => {
    getWorkspaceMock.mockResolvedValue(
      ws({ approved_egress: ["registry.npmjs.org"], denied_egress: ["evil.example.com"] }),
    );
    render(
      <MemoryRouter initialEntries={["/workspaces/ws-1"]}>
        <OperatorProvider operator={false} securityOperator={true}>
          <Routes>
            <Route path="/workspaces/:id" element={<WorkspaceDetailScreen />} />
          </Routes>
        </OperatorProvider>
      </MemoryRouter>,
    );

    expect(await screen.findByRole("button", { name: "Remove registry.npmjs.org" })).not.toBeDisabled();
    expect(screen.getByRole("button", { name: "Remove evil.example.com" })).not.toBeDisabled();
    expect(screen.getByRole("button", { name: /delete this workspace/i })).toBeDisabled();
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
    expect(blocked).toHaveAttribute("title", OPERATOR_ONLY_REASON);
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
  });
});

// X3-F2 / F5-F1 — the detail page's Delete and Rebuild are the same classOwner
// route the list's kebab is, so they follow the row's OWNER, not the caller's
// tier. Both legs fail closed: an operator-owned row (owned_by absent) and an
// unresolved /me leave a member with nothing live.
describe("WorkspaceDetailScreen — Delete and Rebuild follow the row's owner", () => {
  function renderOwned(principal: string, over: Partial<Workspace> = {}, operatorResolved = true) {
    getWorkspaceMock.mockResolvedValue(
      ws({ profile: { has_devcontainer: true } as unknown as Record<string, unknown>, ...over }),
    );
    return render(
      <MemoryRouter initialEntries={["/workspaces/ws-1"]}>
        <OperatorProvider
          operator={false}
          securityOperator={false}
          principal={principal}
          operatorResolved={operatorResolved}
        >
          <Routes>
            <Route path="/workspaces/:id" element={<WorkspaceDetailScreen />} />
          </Routes>
        </OperatorProvider>
      </MemoryRouter>,
    );
  }

  it("a member's OWN row: Delete and Rebuild are live", async () => {
    renderOwned("dana@corp.example", { owned_by: "dana@corp.example" });
    expect(await screen.findByRole("button", { name: /delete this workspace/i })).not.toBeDisabled();
    expect(screen.getByRole("button", { name: "Rebuild" })).not.toBeDisabled();
  });

  it("someone else's row, and an unresolved /me, leave both parked", async () => {
    renderOwned("dana@corp.example", { owned_by: "" });
    expect(await screen.findByRole("button", { name: /delete this workspace/i })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Rebuild" })).toBeDisabled();

    cleanup();
    renderOwned("dana@corp.example", { owned_by: "dana@corp.example" }, false);
    expect(await screen.findByRole("button", { name: /delete this workspace/i })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Rebuild" })).toBeDisabled();
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
    expect(parked).toHaveAttribute("title", EGRESS.SCAN_SEEDED_REASON);
    expect(screen.getByRole("button", { name: "Remove pypi.org" })).not.toBeDisabled();
  });

  it("negative control: the approved host still PUTs the narrowed allowlist", async () => {
    getWorkspaceMock.mockResolvedValue(ws({ approved_egress: ["pypi.org", "registry.npmjs.org"] }));
    setApprovedEgressMock.mockResolvedValue(ws({ approved_egress: ["registry.npmjs.org"] }));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderDetail();

    await user.click(await screen.findByRole("button", { name: "Remove pypi.org" }));
    await waitFor(() =>
      expect(setApprovedEgressMock).toHaveBeenCalledWith("ws-1", ["registry.npmjs.org"]),
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
    expect(parked).toHaveAttribute("title", EGRESS.INHERITED_REASON);
    expect(parked).not.toHaveAttribute("title", EGRESS.SCAN_SEEDED_REASON);
    // The row's own sentence and its parked reason agree.
    expect(screen.getByText("required by this workspace")).toBeInTheDocument();
  });

  it("a member (whose egress: keys the server drops) is told the tier, not handed a dead X", async () => {
    getWorkspaceMock.mockResolvedValue(ws({ approved_egress: ["pypi.org"] }));
    renderDetail("ws-1", false, false);
    const parked = await screen.findByRole("button", { name: "Remove pypi.org" });
    expect(parked).toBeDisabled();
    expect(parked).toHaveAttribute("title", SECURITY_ONLY_REASON);
  });

  // The Denied twin renders the same HostList behind the same securityOperator
  // gate — a member must not get a bare disabled X there while the Allowed
  // card beside it says why. Same reason, same tier.
  it("the Denied hosts twin says the same thing to the same caller", async () => {
    getWorkspaceMock.mockResolvedValue(ws({ denied_egress: ["evil.example.com"] }));
    renderDetail("ws-1", false, false);
    const parked = await screen.findByRole("button", { name: "Remove evil.example.com" });
    expect(parked).toBeDisabled();
    expect(parked).toHaveAttribute("title", SECURITY_ONLY_REASON);
  });
});

// F5-F10: the subtitle must not claim the record loop "writes the
// least-privilege policy" — it writes `egress:` requirement rows; the policy
// hand-off is the separate optional "Save session profile" action. The
// retired sentence must appear nowhere.
describe("WorkspaceDetailScreen — the Recorded-sessions subtitle stops overclaiming", () => {
  // ticket: F5-F10
  it("never says the loop writes the least-privilege policy", async () => {
    getWorkspaceMock.mockResolvedValue(ws());
    renderDetail();
    await screen.findByText("Recorded sessions");
    expect(screen.queryByText(/writes the least-privilege policy/i)).not.toBeInTheDocument();
  });
});

// F6-F3 (site 2): getSetupStatus() degrades to the synthetic READY_FALLBACK
// (unreachable:true) on any non-401 failure — hasLlmPath(READY_FALLBACK) is
// always false, so `.then((s) => setLlmReady(hasLlmPath(s)))` alone would
// tell an operator "no model provider configured" for a daemon that simply
// never answered. `unreachable` must read as unknown, not "no".
describe("WorkspaceDetailScreen — an unreachable setup status never claims no model provider", () => {
  // ticket: F6-F3 (site 2)
  it("shows no model-provider warning when the setup status is the synthetic unreachable fallback", async () => {
    getSetupStatusMock.mockResolvedValue(setupStatus({ unreachable: true }));
    getWorkspaceMock.mockResolvedValue(ws());
    renderDetail();
    await screen.findByText("Recorded sessions");
    expect(screen.queryByText(/no model provider is configured/i)).not.toBeInTheDocument();
  });

  it("neg: a genuinely reachable, provider-less status still warns", async () => {
    getSetupStatusMock.mockResolvedValue(setupStatus());
    getWorkspaceMock.mockResolvedValue(ws());
    renderDetail();
    await screen.findByText("Recorded sessions");
    expect(await screen.findByText(/no model provider is configured/i)).toBeInTheDocument();
  });
});

// F5-F7: `load` needs a request token across an `:id` change — the component
// is REUSED across a workspace-to-workspace navigation (react-router keeps
// the same element mounted, only the param changes), so a slow response for
// the OLD id that resolves after the NEW id's own load could paint stale data
// under the new URL. Copies audit.tsx's drillRequestId idiom.
function NavButton({ to }: { to: string }) {
  const navigate = useNavigate();
  return (
    <button type="button" onClick={() => navigate(to)}>
      go to {to}
    </button>
  );
}

describe("WorkspaceDetailScreen — a stale load can't clobber a newer one", () => {
  // ticket: F5-F7
  it("renders workspace B even when A's load resolves after B's", async () => {
    let resolveA: (w: ReturnType<typeof ws>) => void = () => {};
    const aPromise = new Promise<ReturnType<typeof ws>>((res) => {
      resolveA = res;
    });
    getWorkspaceMock.mockImplementation((id: string) => {
      if (id === "ws-a") return aPromise;
      if (id === "ws-b") return Promise.resolve(ws({ id: "ws-b", name: "beta" }));
      return Promise.resolve(undefined);
    });
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(
      <MemoryRouter initialEntries={["/workspaces/ws-a"]}>
        <OperatorProvider operator securityOperator>
          <NavButton to="/workspaces/ws-b" />
          <Routes>
            <Route path="/workspaces/:id" element={<WorkspaceDetailScreen />} />
          </Routes>
        </OperatorProvider>
      </MemoryRouter>,
    );
    // Navigate to B (same mounted component, new :id param) before A's fetch
    // ever resolves.
    await user.click(screen.getByRole("button", { name: /go to \/workspaces\/ws-b/i }));
    expect(await screen.findByRole("heading", { name: "beta" })).toBeInTheDocument();
    // A's stale response lands last — it must not clobber B's render.
    resolveA(ws({ id: "ws-a", name: "alpha" }));
    await new Promise((r) => setTimeout(r, 0));
    expect(screen.getByRole("heading", { name: "beta" })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "alpha" })).not.toBeInTheDocument();
  });
});
