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
vi.mock("../../../lib/api/integrations", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/integrations")>(
    "../../../lib/api/integrations",
  );
  return { ...actual, integrationsApi: { list: () => listIntegrationsMock() } };
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
    // Two calls now: leaving step ③ Integrations persists whatever was picked
    // there (nothing, in this walk), and step ⑤'s "Save & continue" persists
    // the full seeded contract — assert the LAST one, the final saved state.
    await waitFor(() => expect(setRequirementsMock).toHaveBeenCalledTimes(2));
    expect(setRequirementsMock.mock.calls.at(-1)?.[1]).toMatchObject({
      // Seeded under the STORABLE name — the PUT must pass the server's
      // secret-name grammar (the live 400 this pins).
      "secret:database-url": { level: "required" },
      "egress:registry.npmjs.org": { level: "required" },
    });
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
