/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const createWorkspaceMock = vi.fn();
const scanWorkspaceMock = vi.fn();
const getWorkspaceMock = vi.fn();
const setRequirementsMock = vi.fn();
vi.mock("../../../lib/api/workspaces", () => ({
  workspaces: {
    createWorkspace: (...a: unknown[]) => createWorkspaceMock(...a),
    scanWorkspace: (...a: unknown[]) => scanWorkspaceMock(...a),
    getWorkspace: (...a: unknown[]) => getWorkspaceMock(...a),
    setRequirements: (...a: unknown[]) => setRequirementsMock(...a),
    updateWorkspace: vi.fn(),
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
import { C } from "../../../lib/workspace-copy";

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

beforeEach(() => {
  createWorkspaceMock.mockReset();
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
  it("creates the workspace, scans it, seeds requirements from the profile, and lands on a usable Done screen", async () => {
    const ws = baseWorkspace();
    createWorkspaceMock.mockResolvedValue(ws);
    scanWorkspaceMock.mockResolvedValue({ async: false });
    getWorkspaceMock.mockResolvedValue(ws);
    setRequirementsMock.mockResolvedValue({
      ...ws,
      requirements: { "secret:DATABASE_URL": { level: "required", provenance: "operator_set" } },
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
    await screen.findByText(C.S3_BLURB);
    // The Requirements step opens on its Record tab (step-requirements.tsx) —
    // DATABASE_URL lives under Secrets. Radix Tabs activates on mousedown, so
    // this needs real userEvent rather than fireEvent.click.
    await userEvent.setup().click(screen.getByRole("tab", { name: "Secrets" }));
    // Seeded from the profile — DATABASE_URL defaults to required.
    expect(screen.getByText("DATABASE_URL")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /accept & finish/i }));
    await waitFor(() => expect(setRequirementsMock).toHaveBeenCalledTimes(1));
    expect(setRequirementsMock.mock.calls[0][1]).toMatchObject({
      "secret:DATABASE_URL": { level: "required" },
      "egress:registry.npmjs.org": { level: "required" },
    });
    await screen.findByText("payments is usable.");
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
    await screen.findByText(C.S3_BLURB);
    fireEvent.click(screen.getByRole("button", { name: /accept & finish/i }));
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
