/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor, act } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const createWorkspaceMock = vi.fn();
const updateWorkspaceMock = vi.fn();
const buildWorkspaceMock = vi.fn();
const scanWorkspaceMock = vi.fn();
const getWorkspaceMock = vi.fn();
const setRequirementsMock = vi.fn();
vi.mock("../../../lib/api/workspaces", () => ({
  workspaces: {
    createWorkspace: (...a: unknown[]) => createWorkspaceMock(...a),
    scanWorkspace: (...a: unknown[]) => scanWorkspaceMock(...a),
    getWorkspace: (...a: unknown[]) => getWorkspaceMock(...a),
    setRequirements: (...a: unknown[]) => setRequirementsMock(...a),
    updateWorkspace: (...a: unknown[]) => updateWorkspaceMock(...a),
    buildWorkspace: (...a: unknown[]) => buildWorkspaceMock(...a),
    getWorkspaceBuild: (...a: unknown[]) => buildWorkspaceMock(...a),
  },
}));

const listSecretsMock = vi.fn();
vi.mock("../../../lib/api/secrets", () => ({
  secrets: { listSecrets: () => listSecretsMock(), setSecret: vi.fn() },
}));

const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({
  setup: { getSetupStatus: () => getSetupStatusMock() },
}));

const listIntegrationsMock = vi.fn();
const adoptIntegrationMock = vi.fn();
vi.mock("../../../lib/api/integrations", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/integrations")>(
    "../../../lib/api/integrations",
  );
  return {
    ...actual,
    integrationsApi: { list: () => listIntegrationsMock(), adoptIntegration: (...a: unknown[]) => adoptIntegrationMock(...a) },
  };
});

import { WorkspaceWizard } from "./wizard";
import { C, RD2 } from "../../../lib/workspace-copy";
import { INTEGRATIONS_BLURB } from "./step-integrations";
import type { Workspace } from "../../../lib/types";

function baseWorkspace(overrides: Record<string, unknown> = {}) {
  return {
    id: "ws-1",
    name: "payments",
    kind: "local_dir",
    source: "/home/me/payments",
    status: "ready",
    created_at: "now",
    updated_at: "now",
    profile: {
      languages: ["Go"],
      package_managers: ["pnpm"],
      required_secrets: [{ name: "DATABASE_URL" }],
      egress_domains: ["registry.npmjs.org"],
    },
    ...overrides,
  };
}

// A strictly `Workspace`-typed fixture for the `initial` (edit-hydration)
// prop below — baseWorkspace() above is deliberately loose (status: "ready"
// isn't a real WorkspaceStatus) and was never checked against the real type
// until now.
function editableWorkspace(over: Partial<Workspace> = {}): Workspace {
  return {
    id: "ws-1",
    name: "payments",
    kind: "repo",
    source: "acme/payments",
    ref: "main",
    status: "scanned",
    created_at: "now",
    updated_at: "now",
    sources: [{ type: "repo", source: "acme/payments", ref: "main", target: "/home/agent/work" }],
    profile: {
      languages: ["Go"],
      package_managers: ["pnpm"],
      required_secrets: [{ name: "DATABASE_URL" }],
      egress_domains: ["registry.npmjs.org"],
    },
    ...over,
  };
}

beforeEach(() => {
  createWorkspaceMock.mockReset();
  buildWorkspaceMock.mockReset().mockResolvedValue({ state: "done", image: "wardyn-workspace/test:abc" });
  // Step ② persists the base-image pick via updateWorkspace; echo back the
  // workspace the test created (profile intact) so the walk keeps its state.
  updateWorkspaceMock.mockReset().mockImplementation(async () => {
    const last = createWorkspaceMock.mock.results.at(-1);
    return last ? await last.value : baseWorkspace();
  });
  scanWorkspaceMock.mockReset();
  getWorkspaceMock.mockReset();
  setRequirementsMock.mockReset();
  listSecretsMock.mockReset().mockResolvedValue([]);
  getSetupStatusMock.mockReset().mockResolvedValue({ secrets: { present: [], github_app: false } });
  listIntegrationsMock.mockReset().mockResolvedValue({ ai: [], scm: [], mirror: [], proxy: [] });
  adoptIntegrationMock.mockReset().mockResolvedValue(undefined);
});

// Drives the wizard from a blank Sources step through a resolved (synchronous,
// local_dir-only) scan and lands it on Phase B of the Base image step.
async function driveToBaseImage() {
  fireEvent.change(screen.getByLabelText("Name"), { target: { value: "payments" } });
  fireEvent.click(screen.getByRole("button", { name: "Add Local directory" }));
  fireEvent.change(screen.getByPlaceholderText("/home/me/projects/payments"), {
    target: { value: "/home/me/payments" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Continue →" }));
  await screen.findByText("Recommended — built for this workspace");
}

describe("WorkspaceWizard — the happy path end to end", () => {
  // The suite's longest walk (all 7 steps, real userEvent on Radix tabs) —
  // ~3s under coverage+load, which grazes vitest's 5s default and flakes the
  // full `make ci` run while passing isolated. Room, not speed, is the fix.
  it("creates the workspace, scans it, seeds requirements from the profile, and lands on a usable Done screen", { timeout: 15000 }, async () => {
    const ws = baseWorkspace();
    createWorkspaceMock.mockResolvedValue(ws);
    scanWorkspaceMock.mockResolvedValue({ async: false });
    getWorkspaceMock.mockResolvedValue(ws);
    setRequirementsMock.mockResolvedValue({
      ...ws,
      requirements: { "secret:database-url": { level: "required", provenance: "operator_set" } },
    });

    render(<WorkspaceWizard onClose={vi.fn()} />);
    await driveToBaseImage();

    expect(createWorkspaceMock).toHaveBeenCalledTimes(1);
    expect(createWorkspaceMock.mock.calls[0][0]).toMatchObject({ name: "payments" });
    // The detected chips render in the needs card AND as each card's Carries
    // inventory now (the tools-not-AI round) — presence, not uniqueness.
    expect(screen.getAllByText("Go").length).toBeGreaterThan(0);
    expect(screen.getAllByText("pnpm").length).toBeGreaterThan(0);

    fireEvent.click(screen.getByRole("button", { name: "Continue →" }));
    // Step ③ Integrations — what the workspace connects through joins
    // sources+image in shaping Requirements.
    await screen.findByText(INTEGRATIONS_BLURB);
    fireEvent.click(screen.getByRole("button", { name: "Continue →" }));
    // Step ④ Build — the image build is its own followable step now.
    expect(await screen.findByText("Image ready")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Continue →" }));
    await screen.findByText(C.S3_BLURB);
    // The Requirements step opens on its Record tab (step-requirements.tsx) —
    // DATABASE_URL lives under Secrets. Radix Tabs activates on mousedown, so
    // this needs real userEvent rather than fireEvent.click.
    await userEvent.setup().click(screen.getByRole("tab", { name: "Secrets" }));
    // Seeded from the profile — DATABASE_URL defaults to required.
    expect(screen.getByText("DATABASE_URL")).toBeInTheDocument();

    // Step ⑤ saves the contract (the verify session reads the STORED rows),
    // then step ⑥ Verify carries the session affordance; Finish lands on Done.
    fireEvent.click(screen.getByRole("button", { name: /save & continue/i }));
    await screen.findByText(RD2.CARRY);
    fireEvent.click(screen.getByRole("button", { name: "Finish" }));
    // Two calls now: leaving step ③ Integrations and step ⑤'s "Save &
    // continue". UI-WS-7: PUT /requirements replaces the workspace OVERLAY,
    // which carries ONLY operator-authored rows — this walk edits nothing, so
    // both PUTs send {}. The scan_seeded DATABASE_URL / registry.npmjs.org rows
    // (seeded here under the STORABLE name for DISPLAY) live on the source
    // contract and fold into effective_requirements server-side; persisting
    // them into the overlay would freeze them past a rescan.
    await waitFor(() => expect(setRequirementsMock).toHaveBeenCalledTimes(2));
    expect(setRequirementsMock.mock.calls.at(-1)?.[1]).toEqual({});
    await screen.findByText("payments is usable.");
  });
});

// design §3 / HANDOFF-2026-08-06.md §4 moving part 3: the Build step (right
// after Integrations) can only see a named integration if leaving Integrations
// actually persisted it first — previously the Continue there was a bare
// client-side step patch.
describe("WorkspaceWizard — leaving Integrations persists the pick before Build runs", () => {
  it("issues setRequirements, and it resolves before Build's own kick fires", async () => {
    const ws = baseWorkspace();
    createWorkspaceMock.mockResolvedValue(ws);
    scanWorkspaceMock.mockResolvedValue({ async: false });
    getWorkspaceMock.mockResolvedValue(ws);
    setRequirementsMock.mockResolvedValue({
      ...ws,
      requirements: { "integration:anthropic_api_key": { level: "required", provenance: "operator_set" } },
    });

    render(<WorkspaceWizard onClose={vi.fn()} />);
    await driveToBaseImage();
    fireEvent.click(screen.getByRole("button", { name: "Continue →" }));
    await screen.findByText(INTEGRATIONS_BLURB);

    fireEvent.click(screen.getByRole("button", { name: "Continue →" }));

    await waitFor(() => expect(setRequirementsMock).toHaveBeenCalledTimes(1));
    expect(setRequirementsMock.mock.calls[0][0]).toBe("ws-1");
    await waitFor(() => expect(buildWorkspaceMock).toHaveBeenCalled());
    expect(setRequirementsMock.mock.invocationCallOrder[0]).toBeLessThan(
      buildWorkspaceMock.mock.invocationCallOrder[0],
    );
  });

  it("a failed save keeps the operator on Integrations and never kicks a Build", async () => {
    const ws = baseWorkspace();
    createWorkspaceMock.mockResolvedValue(ws);
    scanWorkspaceMock.mockResolvedValue({ async: false });
    getWorkspaceMock.mockResolvedValue(ws);
    setRequirementsMock.mockRejectedValue(new Error("network down"));

    render(<WorkspaceWizard onClose={vi.fn()} />);
    await driveToBaseImage();
    fireEvent.click(screen.getByRole("button", { name: "Continue →" }));
    await screen.findByText(INTEGRATIONS_BLURB);

    fireEvent.click(screen.getByRole("button", { name: "Continue →" }));

    await waitFor(() => expect(setRequirementsMock).toHaveBeenCalledTimes(1));
    expect(screen.getByText(INTEGRATIONS_BLURB)).toBeInTheDocument();
    expect(buildWorkspaceMock).not.toHaveBeenCalled();
  });

  // Item 2 (reconcile-wave4.md): the rail's Build circle used to bypass
  // continueFromIntegrations with a bare `patch({step:"build"})`, reaching
  // Build without the just-toggled pick ever reaching the server. Two lines
  // of defense now close it: StepIndicator's own clickable gate (i <=
  // currentIdx) already refuses a FORWARD jump — Build sits one step ahead
  // of Integrations, so its circle is disabled while viewing Integrations,
  // full stop — and onJump's own routing (this describe block's real fix)
  // is the second layer for whichever caller/future change ever makes it
  // reachable some other way. `disabled` genuinely blocks the click here
  // (verified: fireEvent.click on a disabled button never invokes onClick).
  it("the rail's Build circle is disabled while viewing Integrations — the sibling bypass has no click to take", async () => {
    const ws = baseWorkspace();
    createWorkspaceMock.mockResolvedValue(ws);
    scanWorkspaceMock.mockResolvedValue({ async: false });
    getWorkspaceMock.mockResolvedValue(ws);

    render(<WorkspaceWizard onClose={vi.fn()} />);
    await driveToBaseImage();
    fireEvent.click(screen.getByRole("button", { name: "Continue →" }));
    await screen.findByText(INTEGRATIONS_BLURB);

    expect(screen.getByRole("button", { name: /build/i })).toBeDisabled();
    // Confirms it stays inert even if something WAS clicked: no persistence,
    // no build kick.
    expect(setRequirementsMock).not.toHaveBeenCalled();
    expect(buildWorkspaceMock).not.toHaveBeenCalled();
  });

  it("jumping to Build via the rail from a LATER step (genuinely reachable — behind current) doesn't needlessly re-persist", async () => {
    const ws = baseWorkspace();
    createWorkspaceMock.mockResolvedValue(ws);
    scanWorkspaceMock.mockResolvedValue({ async: false });
    getWorkspaceMock.mockResolvedValue(ws);
    setRequirementsMock.mockResolvedValue(ws);

    render(<WorkspaceWizard onClose={vi.fn()} />);
    await driveToBaseImage();
    fireEvent.click(screen.getByRole("button", { name: "Continue →" })); // -> integrations
    await screen.findByText(INTEGRATIONS_BLURB);
    fireEvent.click(screen.getByRole("button", { name: "Continue →" })); // -> build (persists once)
    await screen.findByText("Image ready");
    fireEvent.click(screen.getByRole("button", { name: "Continue →" })); // -> reqs
    await screen.findByText(C.S3_BLURB);
    await waitFor(() => expect(setRequirementsMock).toHaveBeenCalledTimes(1));

    // Requirements (idx4) sits AHEAD of Build (idx3), so the rail's Build
    // circle is genuinely clickable from here — this is the `else` branch
    // of the fix (s.step !== "integrations"), which must NOT call
    // continueFromIntegrations a second time for no reason.
    await userEvent.setup().click(screen.getByRole("button", { name: /build/i }));
    await screen.findByText("Image ready");
    expect(setRequirementsMock).toHaveBeenCalledTimes(1);
  });
});

describe("WorkspaceWizard — close semantics", () => {
  it("says Cancel before the workspace exists, and calls onClose directly", () => {
    const onClose = vi.fn();
    render(<WorkspaceWizard onClose={onClose} />);
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("switches to Close + shows C.CLOSE_KEEPS once the workspace has been created", async () => {
    const ws = baseWorkspace();
    createWorkspaceMock.mockResolvedValue(ws);
    scanWorkspaceMock.mockResolvedValue({ async: false });
    getWorkspaceMock.mockResolvedValue(ws);

    render(<WorkspaceWizard onClose={vi.fn()} />);
    await driveToBaseImage();

    expect(screen.getByText(C.CLOSE_KEEPS)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Close" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Cancel" })).not.toBeInTheDocument();
  });
});

describe("WorkspaceWizard — accidental dismissal is blocked once there's something to strand (Fix C)", () => {
  it("Escape closes normally before any workspace exists (nothing to strand)", () => {
    const onClose = vi.fn();
    render(<WorkspaceWizard onClose={onClose} />);
    fireEvent.keyDown(document, { key: "Escape" });
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("Escape is blocked once the workspace exists and the step isn't Done", async () => {
    const ws = editableWorkspace({ status: "scanned", image_ref: "" });
    const onClose = vi.fn();
    render(<WorkspaceWizard initial={ws} onClose={onClose} />);
    await screen.findByText("Recommended — built for this workspace");

    fireEvent.keyDown(document, { key: "Escape" });
    expect(onClose).not.toHaveBeenCalled();
    // Still open — the step content is still there, not stranded mid-close.
    expect(screen.getByText("Recommended — built for this workspace")).toBeInTheDocument();
  });

  it("the X button still closes deliberately even while Escape/outside-click are blocked", async () => {
    const ws = editableWorkspace({ status: "scanned", image_ref: "" });
    const onClose = vi.fn();
    render(<WorkspaceWizard initial={ws} onClose={onClose} />);
    await screen.findByText("Recommended — built for this workspace");

    fireEvent.click(screen.getByRole("button", { name: /close/i }));
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});

describe("WorkspaceWizard — edit hydration via the `initial` prop (Fix B)", () => {
  it("hydrates onto an existing workspace: no create call, lands on the heuristic's step, titled Edit workspace", async () => {
    const ws = editableWorkspace({ status: "scanned", image_ref: "" });
    render(<WorkspaceWizard initial={ws} onClose={vi.fn()} />);

    expect(await screen.findByRole("heading", { name: "Edit workspace" })).toBeInTheDocument();
    // scanned + no image_ref yet -> lands on Base image, not Sources.
    expect(await screen.findByText("Recommended — built for this workspace")).toBeInTheDocument();
    expect(createWorkspaceMock).not.toHaveBeenCalled();
  });

  it("a not-yet-scanned workspace lands on Sources instead", async () => {
    const ws = editableWorkspace({ status: "pending_scan" });
    render(<WorkspaceWizard initial={ws} onClose={vi.fn()} />);
    expect(await screen.findByLabelText("Name")).toHaveValue("payments");
  });

  it("a scanned workspace with an image already built lands on Requirements", async () => {
    const ws = editableWorkspace({ status: "scanned", image_ref: "wardyn-workspace/ws-1:abc123" });
    render(<WorkspaceWizard initial={ws} onClose={vi.fn()} />);
    expect(await screen.findByText(C.S3_BLURB)).toBeInTheDocument();
  });

  it("continuing from the hydrated Base image step PUTs via updateWorkspace, never creates", async () => {
    const ws = editableWorkspace({ status: "scanned", image_ref: "" });
    render(<WorkspaceWizard initial={ws} onClose={vi.fn()} />);
    await screen.findByText("Recommended — built for this workspace");

    fireEvent.click(screen.getByRole("button", { name: "Continue →" }));

    await waitFor(() => expect(updateWorkspaceMock).toHaveBeenCalledTimes(1));
    expect(updateWorkspaceMock.mock.calls[0][0]).toBe("ws-1");
    expect(updateWorkspaceMock.mock.calls[0][1]).toMatchObject({
      sources: [{ type: "repo", source: "acme/payments", ref: "main", target: "/home/agent/work" }],
      base_image: { kind: "recommended" },
    });
    expect(createWorkspaceMock).not.toHaveBeenCalled();
  });
});

// The rail's Requirements/Verify circles used to render enabled but silently
// no-op whenever the workspace had no profile — the footer's own Back button
// navigates to those same steps with no such guard.
describe("WorkspaceWizard — rail and footer agree on reachability without a profile", () => {
  it("the Requirements rail circle jumps back even with no profile, matching the footer's unguarded Back", async () => {
    const noProfileWs = editableWorkspace({
      status: "scanned",
      image_ref: "wardyn-workspace/ws-1:abc123",
      profile: undefined,
    });
    setRequirementsMock.mockResolvedValue({ ...noProfileWs, requirements: {} });
    render(<WorkspaceWizard initial={noProfileWs} onClose={vi.fn()} />);

    // Lands directly on Requirements (initialStepFor: scanned + image_ref set).
    await screen.findByText(C.S3_BLURB);

    fireEvent.click(screen.getByRole("button", { name: /save & continue/i }));
    await screen.findByText(RD2.CARRY); // now on Verify

    // The Requirements circle sits at-or-before the current (Verify) index,
    // so it's clickable — and must actually jump, exactly like the footer's
    // own unguarded Back button does from this same step.
    await userEvent.setup().click(screen.getByRole("button", { name: /requirements/i }));
    await screen.findByText(C.S3_BLURB);
  });
});

// `partial` (the "Continue anyway"/"Continue without waiting" acknowledgment)
// used to be set once and never cleared, so the warning chip outlived the
// scan it described.
describe("WorkspaceWizard — a partial-scan acknowledgment clears once the scan actually finishes", () => {
  it("'based on a partial scan' disappears once every source lands, without a rescan", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const created = baseWorkspace({ status: "pending_scan" });
      createWorkspaceMock.mockResolvedValue(created);
      scanWorkspaceMock.mockResolvedValue({ async: true });
      getWorkspaceMock
        .mockResolvedValueOnce({ ...created, status: "scanning" })
        .mockResolvedValueOnce(baseWorkspace({ status: "scanned" }));

      render(<WorkspaceWizard onClose={vi.fn()} />);
      fireEvent.change(screen.getByLabelText("Name"), { target: { value: "payments" } });
      fireEvent.click(screen.getByRole("button", { name: "Add Local directory" }));
      fireEvent.change(screen.getByPlaceholderText("/home/me/projects/payments"), {
        target: { value: "/home/me/payments" },
      });
      fireEvent.click(screen.getByRole("button", { name: "Continue →" }));

      // Still scanning — "Continue without waiting" sets the partial ack.
      // Base image renders the chip in more than one place (recommended card,
      // registry card, needs header) — assert at least one, not exactly one.
      const continueBtn = await screen.findByRole("button", { name: /continue without waiting/i });
      fireEvent.click(continueBtn);
      expect(screen.getAllByText("based on a partial scan").length).toBeGreaterThan(0);

      // Let the poll loop run its course to the settled response.
      await act(() => vi.advanceTimersByTimeAsync(3500));
      await waitFor(() => expect(screen.queryAllByText("based on a partial scan")).toHaveLength(0));
    } finally {
      vi.useRealTimers();
    }
  });

  // Item 3 (reconcile-wave3.md): the timeout path used to clear `partial`
  // right alongside the settled path — so a scan that was STILL running
  // after the full 40x1.5s poll silently lost the "based on a partial scan"
  // chip in exactly the case (a slow scan, acknowledged) where it mattered
  // most.
  it(
    "stays 'based on a partial scan' when the poll exhausts without settling — the scan genuinely didn't finish",
    { timeout: 20000 },
    async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true });
      try {
        const created = baseWorkspace({ status: "pending_scan" });
        createWorkspaceMock.mockResolvedValue(created);
        scanWorkspaceMock.mockResolvedValue({ async: true });
        // Every poll comes back still "scanning" — the loop exhausts all 40
        // iterations without ever finding a terminal status.
        getWorkspaceMock.mockResolvedValue({ ...created, status: "scanning" });

        render(<WorkspaceWizard onClose={vi.fn()} />);
        fireEvent.change(screen.getByLabelText("Name"), { target: { value: "payments" } });
        fireEvent.click(screen.getByRole("button", { name: "Add Local directory" }));
        fireEvent.change(screen.getByPlaceholderText("/home/me/projects/payments"), {
          target: { value: "/home/me/payments" },
        });
        fireEvent.click(screen.getByRole("button", { name: "Continue →" }));

        const continueBtn = await screen.findByRole("button", { name: /continue without waiting/i });
        fireEvent.click(continueBtn);
        expect(screen.getAllByText("based on a partial scan").length).toBeGreaterThan(0);

        // Exhaust the whole 40x1.5s poll (60s) — it never settles.
        await act(() => vi.advanceTimersByTimeAsync(61000));
        // The scan never finished — the honesty chip must survive the
        // timeout, not be silently erased by it.
        expect(screen.getAllByText("based on a partial scan").length).toBeGreaterThan(0);
      } finally {
        vi.useRealTimers();
      }
    },
  );
});

describe("WorkspaceWizard — going back to Sources after a scan warns first", () => {
  it("shows the RESCAN_DESTROYS-style confirm on Back, and only navigates back on confirmation", async () => {
    const ws = baseWorkspace();
    createWorkspaceMock.mockResolvedValue(ws);
    scanWorkspaceMock.mockResolvedValue({ async: false });
    getWorkspaceMock.mockResolvedValue(ws);

    render(<WorkspaceWizard onClose={vi.fn()} />);
    await driveToBaseImage();

    fireEvent.click(screen.getByRole("button", { name: "Back" }));
    const dialog = screen.getByRole("alertdialog");
    expect(dialog).toHaveTextContent(C.RESCAN_DESTROYS);

    // Cancel keeps us on the Base image step.
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.getByText("Recommended — built for this workspace")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Back" }));
    fireEvent.click(screen.getByRole("button", { name: "Go back" }));
    expect(await screen.findByLabelText("Name")).toBeInTheDocument();
  });
});

describe("WorkspaceWizard — SSH gate at scan time", () => {
  it("fails an ungated SSH source immediately without waiting on the network, alongside a local dir that still scans for real", async () => {
    const ws = baseWorkspace({ id: "ws-2" });
    createWorkspaceMock.mockResolvedValue(ws);
    scanWorkspaceMock.mockResolvedValue({ async: false });
    getWorkspaceMock.mockResolvedValue(ws);

    render(<WorkspaceWizard onClose={vi.fn()} />);
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "payments" } });
    fireEvent.click(screen.getByRole("button", { name: "Add Local directory" }));
    fireEvent.change(screen.getByPlaceholderText("/home/me/projects/payments"), {
      target: { value: "/home/me/payments" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Add Repository" }));
    fireEvent.change(screen.getByPlaceholderText("acme/payments-service"), {
      target: { value: "git@ghes.corp.internal:acme/payments.git" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Continue →" }));

    // The gated row fails immediately (no key stored); the real scan still
    // runs (for the local dir) rather than being blocked by its sibling.
    await screen.findByText("SSH key needed first");
    await waitFor(() => expect(scanWorkspaceMock).toHaveBeenCalledTimes(1));
    await screen.findByText(/scanned/);
  });

  it("skips the network scan entirely when every non-ephemeral source is gated", async () => {
    const ws = baseWorkspace({ id: "ws-3" });
    createWorkspaceMock.mockResolvedValue(ws);

    render(<WorkspaceWizard onClose={vi.fn()} />);
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "payments" } });
    fireEvent.click(screen.getByRole("button", { name: "Add Repository" }));
    fireEvent.change(screen.getByPlaceholderText("acme/payments-service"), {
      target: { value: "git@ghes.corp.internal:acme/payments.git" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Continue →" }));

    await screen.findByText("SSH key needed first");
    expect(scanWorkspaceMock).not.toHaveBeenCalled();
  });
});

describe("WorkspaceWizard — Done's primary action follows `origin`", () => {
  async function driveToDone(origin?: "library" | "setup" | "run") {
    const ws = baseWorkspace();
    createWorkspaceMock.mockResolvedValue(ws);
    scanWorkspaceMock.mockResolvedValue({ async: false });
    getWorkspaceMock.mockResolvedValue(ws);
    setRequirementsMock.mockResolvedValue(ws);

    const onClose = vi.fn();
    const onAttach = vi.fn();
    const onOpenWorkspace = vi.fn();
    render(<WorkspaceWizard origin={origin} onClose={onClose} onAttach={onAttach} onOpenWorkspace={onOpenWorkspace} />);
    await driveToBaseImage();
    fireEvent.click(screen.getByRole("button", { name: "Continue →" }));
    await screen.findByText(INTEGRATIONS_BLURB);
    fireEvent.click(screen.getByRole("button", { name: "Continue →" }));
    await screen.findByText("Image ready");
    fireEvent.click(screen.getByRole("button", { name: "Continue →" }));
    await screen.findByText(C.S3_BLURB);
    fireEvent.click(screen.getByRole("button", { name: /save & continue/i }));
    await screen.findByText(RD2.CARRY);
    fireEvent.click(screen.getByRole("button", { name: "Finish" }));
    await screen.findByText("payments is usable.");
    return { onClose, onAttach, onOpenWorkspace };
  }

  it('origin="run": primary action is "Attach to this run"', async () => {
    const { onAttach } = await driveToDone("run");
    fireEvent.click(screen.getByRole("button", { name: "Attach to this run" }));
    expect(onAttach).toHaveBeenCalledWith("ws-1");
  });

  it('origin="setup": primary action is "Back to setup"', async () => {
    const { onClose } = await driveToDone("setup");
    fireEvent.click(screen.getByRole("button", { name: "Back to setup" }));
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('origin="library" (default): primary action opens the workspace', async () => {
    const { onOpenWorkspace } = await driveToDone();
    fireEvent.click(screen.getByRole("button", { name: "Open payments →" }));
    expect(onOpenWorkspace).toHaveBeenCalledWith("ws-1");
  });
});

describe("WorkspaceWizard — no-edit edit-mode saves send the verbatim baseline, not an invented value (H2)", () => {
  it("Continue on Base image with zero source edits sends initial.sources byte-for-byte (target '' stays '')", async () => {
    const ws = editableWorkspace({
      status: "scanned",
      image_ref: "",
      sources: [{ type: "local_dir", path: "/srv/payments", target: "" }],
    });
    render(<WorkspaceWizard initial={ws} onClose={vi.fn()} />);
    await screen.findByText("Recommended — built for this workspace");

    fireEvent.click(screen.getByRole("button", { name: "Continue →" }));

    await waitFor(() => expect(updateWorkspaceMock).toHaveBeenCalledTimes(1));
    // Sent EXACTLY what was hydrated — toSourceInput would have invented
    // target: "/home/agent/work" for an empty stored target, which is
    // precisely the false "edit" this must not manufacture.
    expect(updateWorkspaceMock.mock.calls[0][1].sources).toEqual([
      { type: "local_dir", path: "/srv/payments", target: "" },
    ]);
    expect(createWorkspaceMock).not.toHaveBeenCalled();
  });
});

describe("WorkspaceWizard — an edited Sources save PUTs first, then scans, and the profile survives the next save (H3)", () => {
  it("sends the EDITED value (not the stale hydrated one), PUTs before scanning, and a later Base-image save doesn't re-trigger sourcesChanged", async () => {
    const ws = editableWorkspace({
      status: "pending_scan",
      image_ref: "",
      sources: [{ type: "local_dir", path: "/srv/old", target: "" }],
    });
    const scanned = { ...ws, status: "scanned" as const, profile: { languages: ["Go"] } };
    updateWorkspaceMock
      .mockReset()
      .mockResolvedValue({ ...ws, sources: [{ type: "local_dir", path: "/srv/new", target: "" }] });
    scanWorkspaceMock.mockResolvedValue({ async: false });
    getWorkspaceMock.mockResolvedValue(scanned);

    render(<WorkspaceWizard initial={ws} onClose={vi.fn()} />);
    // pending_scan -> lands on Sources, hydrated path pre-filled.
    fireEvent.change(screen.getByPlaceholderText("/home/me/projects/payments"), {
      target: { value: "/srv/new" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Continue →" }));

    await waitFor(() => expect(updateWorkspaceMock).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(scanWorkspaceMock).toHaveBeenCalledTimes(1));
    // PUT before scan — the old order scanned the server's stale composition
    // first and PUT the edit only later, from continueFromImage.
    expect(updateWorkspaceMock.mock.invocationCallOrder[0]).toBeLessThan(
      scanWorkspaceMock.mock.invocationCallOrder[0],
    );
    // The edit itself reached the server — a single local_dir source's
    // target defaults to DEFAULT_TARGET, not the stale "/srv/old".
    expect(updateWorkspaceMock.mock.calls[0][1].sources).toEqual([
      { type: "local_dir", path: "/srv/new", target: "/home/agent/work" },
    ]);

    // The scan (against the RIGHT composition) landed a profile; Base image
    // Phase B renders off it.
    await screen.findByText("Recommended — built for this workspace");
    fireEvent.click(screen.getByRole("button", { name: "Continue →" }));

    await waitFor(() => expect(updateWorkspaceMock).toHaveBeenCalledTimes(2));
    // Same sources sent again (nothing changed since) — this must NOT be a
    // sourcesChanged edge the server would wipe the just-produced profile
    // over. (initialSources stays null once touched, so this is a fresh
    // derivation both times — deliberately identical here since nothing
    // changed between the two saves.)
    expect(updateWorkspaceMock.mock.calls[1][1].sources).toEqual([
      { type: "local_dir", path: "/srv/new", target: "/home/agent/work" },
    ]);
  });
});

describe("WorkspaceWizard — Back-to-Sources preserves operator_set lanes when sources are untouched (M4)", () => {
  it("restores the hydrated requirements instead of {} once sources are confirmed unchanged", async () => {
    const ws = editableWorkspace({
      status: "scanned",
      image_ref: "",
      requirements: {
        "egress:manually-added.example.com": { level: "required", provenance: "operator_set" },
      },
    });
    scanWorkspaceMock.mockResolvedValue({ async: false });
    getWorkspaceMock.mockResolvedValue(ws);
    updateWorkspaceMock.mockReset().mockResolvedValue(ws);
    setRequirementsMock.mockResolvedValue(ws);

    render(<WorkspaceWizard initial={ws} onClose={vi.fn()} />);
    await screen.findByText("Recommended — built for this workspace"); // lands on Base image

    // Back triggers the RESCAN_DESTROYS-style confirm (hasScanned is true);
    // confirming with sources UNTOUCHED must restore requirements, not wipe
    // them — the old unconditional {} here downgraded every operator_set
    // lane back to scan defaults on the next full-replace requirements PUT.
    fireEvent.click(screen.getByRole("button", { name: "Back" }));
    fireEvent.click(screen.getByRole("button", { name: "Go back" }));
    expect(await screen.findByLabelText("Name")).toBeInTheDocument();

    // Forward again with zero edits.
    fireEvent.click(screen.getByRole("button", { name: "Continue →" }));
    await screen.findByText("Recommended — built for this workspace");
    fireEvent.click(screen.getByRole("button", { name: "Continue →" }));
    await screen.findByText(INTEGRATIONS_BLURB);
    fireEvent.click(screen.getByRole("button", { name: "Continue →" }));
    await screen.findByText("Image ready");
    fireEvent.click(screen.getByRole("button", { name: "Continue →" }));
    await screen.findByText(C.S3_BLURB);
    fireEvent.click(screen.getByRole("button", { name: /save & continue/i }));

    // Leaving step ③ Integrations now persists too, so this is the SECOND
    // call — assert the LAST one, the final saved state, same as above.
    await waitFor(() => expect(setRequirementsMock).toHaveBeenCalledTimes(2));
    expect(setRequirementsMock.mock.calls.at(-1)?.[1]).toMatchObject({
      "egress:manually-added.example.com": { level: "required", provenance: "operator_set" },
    });
  });
});

// UI-WS-3: unlike M4 above (sources confirmed UNCHANGED), an ACTUAL sources
// edit after Back must not let the pre-edit operator_set contract ride back
// onto the new composition — the server just wiped it for the same reason.
describe("WorkspaceWizard — a sources edit after Back adopts the server's wiped contract, not the stale one (UI-WS-3)", () => {
  it("does not re-apply a row reviewed against the OLD source once sources are actually touched", async () => {
    const editWs = editableWorkspace({
      status: "scanned",
      image_ref: "",
      requirements: {
        "egress:old-repo-host.example.com": { level: "required", provenance: "operator_set" },
      },
    });
    scanWorkspaceMock.mockResolvedValue({ async: false });
    getWorkspaceMock.mockResolvedValue(editWs);
    // The server wipes Requirements/Profile/ApprovedEgress once sourcesChanged
    // (internal/api/workspaces.go) — mirrored here for the sources-edit PUT.
    updateWorkspaceMock.mockReset().mockResolvedValue({ ...editWs, requirements: {} });
    setRequirementsMock.mockResolvedValue(editWs);

    render(<WorkspaceWizard initial={editWs} onClose={vi.fn()} />);
    await screen.findByText("Recommended — built for this workspace"); // lands on Base image

    // Back triggers the RESCAN_DESTROYS-style confirm (hasScanned is true).
    fireEvent.click(screen.getByRole("button", { name: "Back" }));
    fireEvent.click(screen.getByRole("button", { name: "Go back" }));
    expect(await screen.findByLabelText("Name")).toBeInTheDocument();

    // A REAL edit this time (unlike M4) — repoint the source at a different repo.
    fireEvent.change(screen.getByLabelText("Source"), { target: { value: "acme/new-repo" } });
    fireEvent.click(screen.getByRole("button", { name: "Continue →" }));

    await screen.findByText("Recommended — built for this workspace");
    fireEvent.click(screen.getByRole("button", { name: "Continue →" }));
    await screen.findByText(INTEGRATIONS_BLURB);
    fireEvent.click(screen.getByRole("button", { name: "Continue →" }));
    await screen.findByText("Image ready");
    fireEvent.click(screen.getByRole("button", { name: "Continue →" }));
    await screen.findByText(C.S3_BLURB);
    fireEvent.click(screen.getByRole("button", { name: /save & continue/i }));

    await waitFor(() => expect(setRequirementsMock).toHaveBeenCalledTimes(2));
    // The row reviewed against repo A must not silently ride onto repo B —
    // nothing in ANY write from here on names it.
    for (const call of setRequirementsMock.mock.calls) {
      expect(call[1]).not.toHaveProperty("egress:old-repo-host.example.com");
    }
  });
});

// UI-WS-13: adopt-then-setLane used to derive its patch from the RENDER-TIME
// `s` closure (patch()'s own eager eval), not React's latest state — a second
// row named while the first row's adopt was still in flight got silently
// dropped once the deferred call finally landed.
describe("WorkspaceWizard — Integrations' adopt-then-name doesn't drop a row named mid-flight (UI-WS-13)", () => {
  it("a row named while another row's adopt is still pending survives that adopt's resolution", async () => {
    const ws = baseWorkspace();
    createWorkspaceMock.mockResolvedValue(ws);
    scanWorkspaceMock.mockResolvedValue({ async: false });
    getWorkspaceMock.mockResolvedValue(ws);
    let resolveAdopt!: () => void;
    adoptIntegrationMock.mockReset().mockImplementation(
      () => new Promise<void>((resolve) => { resolveAdopt = resolve; }),
    );
    listIntegrationsMock.mockReset().mockResolvedValue({
      ai: [
        // Derived (needs adopt): its own onLane defers behind adoptIntegration.
        { id: "ai:gitlab", serverId: "git_host:gitlab.com", name: "GitLab", typeLabel: "gitlab.com" },
        // Already stored: its onLane calls setLane synchronously, no adopt gate.
        { id: "artifactory", serverId: "artifactory", name: "Artifactory", typeLabel: "artifactory.corp" },
      ],
      scm: [],
    });

    render(<WorkspaceWizard onClose={vi.fn()} />);
    await driveToBaseImage();
    fireEvent.click(screen.getByRole("button", { name: "Continue →" })); // -> integrations
    await screen.findByText(INTEGRATIONS_BLURB);

    const useButtons = await screen.findAllByRole("button", { name: /use in this workspace/i });
    expect(useButtons).toHaveLength(2);

    // GitLab needs adopt first — click it, its promise never resolves yet.
    fireEvent.click(useButtons[0]);
    await waitFor(() => expect(adoptIntegrationMock).toHaveBeenCalledWith("git_host:gitlab.com"));

    // Artifactory names itself synchronously WHILE GitLab's adopt is pending.
    fireEvent.click(useButtons[1]);
    await waitFor(() => expect(screen.getAllByRole("button", { name: /not used/i })).toHaveLength(1));

    // GitLab's adopt now resolves — its OWN setLane must not clobber
    // Artifactory's already-landed one.
    await act(async () => {
      resolveAdopt();
      await Promise.resolve();
    });

    await waitFor(() => expect(screen.getAllByRole("button", { name: /not used/i })).toHaveLength(2));
  });
});
