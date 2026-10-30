/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { AddWorkspaceDialog } from "./add-workspace-dialog";
import { OperatorProvider } from "../wardyn/operator-context";
import type { RunnerView } from "../../lib/types/runners";

const createWorkspaceMock = vi.fn();
const listMineMock = vi.fn();
const getMyCapabilitiesMock = vi.fn();
vi.mock("../../lib/api/setup", () => ({ setup: { getSetupStatus: vi.fn().mockResolvedValue({ runner: { driver: "docker" } }) } }));
vi.mock("../../lib/api/workspaces", () => ({ workspaces: { createWorkspace: (...a: unknown[]) => createWorkspaceMock(...a) } }));
vi.mock("../../lib/api/runners", () => ({ runners: { listMine: (...a: unknown[]) => listMineMock(...a) } }));
vi.mock("../../lib/api/permissions", async () => {
  const actual = await vi.importActual<typeof import("../../lib/api/permissions")>("../../lib/api/permissions");
  return { ...actual, permissions: { ...actual.permissions, getMyCapabilities: (...a: unknown[]) => getMyCapabilitiesMock(...a) } };
});
vi.mock("sonner", () => ({ toast: { warning: vi.fn(), error: vi.fn() } }));

const runner = (id: string, name: string, over: Partial<RunnerView> = {}): RunnerView =>
  ({ id, name, state: "claimed", owner_kind: "person", kind: "self_hosted", ...over }) as RunnerView;

const MAC = runner("11111111-aaaa", "ec2-mac-rc9");
const LAPTOP = runner("22222222-bbbb", "dev-laptop");

// A member, clamped to a root, unless a test says otherwise: only a member binds a folder to their own runner.
function renderDialog({ root = "/Users/dana/work", operator = false }: { root?: string | null; operator?: boolean } = {}) {
  return render(
    <OperatorProvider operator={operator} memberLocalDirRoot={root}>
      <AddWorkspaceDialog existingNames={[]} onClose={() => {}} onCreated={() => {}} />
    </OperatorProvider>,
  );
}

async function pickLocalDir() {
  await userEvent.click(screen.getByRole("button", { name: /^local directory/i }));
}

beforeEach(() => {
  vi.clearAllMocks();
  listMineMock.mockResolvedValue([]);
  getMyCapabilitiesMock.mockResolvedValue({ may_self_host_runner: true });
  createWorkspaceMock.mockResolvedValue({ id: "ws-1", name: "payments" });
});

describe("Add workspace: where is this folder", () => {
  it("is hidden for a repository and for an empty workspace, shown for a local directory", async () => {
    listMineMock.mockResolvedValue([MAC]);
    renderDialog();
    expect(screen.queryByText("Where is this folder?")).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: /^empty/i }));
    expect(screen.queryByText("Where is this folder?")).not.toBeInTheDocument();
    await pickLocalDir();
    expect(screen.getByText("Where is this folder?")).toBeInTheDocument();
  });

  it("lists the server first, then one option per claimed, unrevoked, person-owned runner", async () => {
    listMineMock.mockResolvedValue([
      MAC,
      LAPTOP,
      runner("33333333-cccc", "old-box", { state: "revoked", revoked_at: "2026-10-29T00:00:00Z" }),
      runner("44444444-dddd", "not-claimed-yet", { state: "unclaimed" }),
      runner("55555555-eeee", "org-pool", { owner_kind: "org" }),
    ]);
    renderDialog();
    await pickLocalDir();
    expect(await screen.findByRole("radio", { name: "On my runner: ec2-mac-rc9" })).toBeInTheDocument();
    const radios = screen.getAllByRole("radio").map((r) => r.parentElement?.textContent);
    expect(radios).toEqual(["On the Wardyn server (this host)", "On my runner: ec2-mac-rc9", "On my runner: dev-laptop"]);
    expect(screen.getByRole("radio", { name: "On the Wardyn server (this host)" })).toBeChecked();
  });

  it("the server option keeps today's label and clamped root hint, with no helper text", async () => {
    listMineMock.mockResolvedValue([MAC]);
    renderDialog();
    await pickLocalDir();
    await screen.findByRole("radio", { name: "On my runner: ec2-mac-rc9" });
    expect(screen.getByLabelText("Path on this host")).toBeInTheDocument();
    expect(screen.getByText("Mounted from this machine into the sandbox. Must be under /Users/dana/work — your admin set this boundary.")).toBeInTheDocument();
    expect(screen.queryByText(/The runner checks the folder when the run starts/)).not.toBeInTheDocument();
  });

  it("an administrator is not asked where the folder is, reads no runners, and sends no runner_id", async () => {
    listMineMock.mockResolvedValue([MAC]);
    renderDialog({ root: null, operator: true });
    await pickLocalDir();
    expect(screen.queryByText("Where is this folder?")).not.toBeInTheDocument();
    expect(screen.queryByRole("radio")).not.toBeInTheDocument();
    expect(screen.getByLabelText("Path on this host")).toBeInTheDocument();
    await userEvent.type(screen.getByLabelText("Path on this host"), "/srv/payments");
    await userEvent.click(screen.getByRole("button", { name: /^add workspace/i }));
    expect(listMineMock).not.toHaveBeenCalled();
    expect(createWorkspaceMock.mock.calls[0][0].sources[0].runner_id).toBeUndefined();
  });

  it("picking a runner names it in the path label and hint, and shows the helper", async () => {
    listMineMock.mockResolvedValue([MAC, LAPTOP]);
    renderDialog();
    await pickLocalDir();
    await userEvent.click(await screen.findByRole("radio", { name: "On my runner: ec2-mac-rc9" }));
    expect(screen.getByLabelText("Path on ec2-mac-rc9")).toBeInTheDocument();
    expect(screen.queryByLabelText("Path on this host")).not.toBeInTheDocument();
    expect(screen.getByText(/^Mounted from ec2-mac-rc9 into the sandbox\./)).toBeInTheDocument();
    expect(screen.getByText("The runner checks the folder when the run starts. The run then runs only on that runner.")).toBeInTheDocument();
  });

  it("picking a runner under a clamped root names both in the hint", async () => {
    listMineMock.mockResolvedValue([MAC]);
    renderDialog();
    await pickLocalDir();
    await userEvent.click(await screen.findByRole("radio", { name: "On my runner: ec2-mac-rc9" }));
    expect(screen.getByText("Mounted from ec2-mac-rc9 into the sandbox. Must be under /Users/dana/work — your admin set this boundary.")).toBeInTheDocument();
  });

  it("going back to the server restores the host label and drops the helper", async () => {
    listMineMock.mockResolvedValue([MAC]);
    renderDialog();
    await pickLocalDir();
    await userEvent.click(await screen.findByRole("radio", { name: "On my runner: ec2-mac-rc9" }));
    await userEvent.click(screen.getByRole("radio", { name: "On the Wardyn server (this host)" }));
    expect(screen.getByLabelText("Path on this host")).toBeInTheDocument();
    expect(screen.queryByText(/The runner checks the folder/)).not.toBeInTheDocument();
  });

  it("sends the picked runner's id with the source, and none for the server", async () => {
    listMineMock.mockResolvedValue([MAC]);
    renderDialog();
    await pickLocalDir();
    await userEvent.click(await screen.findByRole("radio", { name: "On my runner: ec2-mac-rc9" }));
    await userEvent.type(screen.getByLabelText("Path on ec2-mac-rc9"), "/Users/dana/work/payments");
    await userEvent.click(screen.getByRole("button", { name: /^add workspace/i }));
    expect(createWorkspaceMock.mock.calls[0][0].sources[0]).toMatchObject({ type: "local_dir", path: "/Users/dana/work/payments", runner_id: MAC.id });
  });

  it("a server folder carries no runner_id", async () => {
    listMineMock.mockResolvedValue([MAC]);
    renderDialog();
    await pickLocalDir();
    await screen.findByRole("radio", { name: "On my runner: ec2-mac-rc9" });
    await userEvent.type(screen.getByLabelText("Path on this host"), "/srv/payments");
    await userEvent.click(screen.getByRole("button", { name: /^add workspace/i }));
    expect(createWorkspaceMock.mock.calls[0][0].sources[0].runner_id).toBeUndefined();
  });

  it("with no runner of their own, shows the server option and the add-a-runner line", async () => {
    renderDialog();
    await pickLocalDir();
    expect(await screen.findByText("You have no runner of your own. Add one in Settings → My runner with Add this computer.")).toBeInTheDocument();
    expect(screen.getAllByRole("radio")).toHaveLength(1);
    expect(screen.queryByText(/The runner checks the folder/)).not.toBeInTheDocument();
    expect(screen.getByLabelText("Path on this host")).toBeInTheDocument();
  });

  it("with personal runners off, offers no runner even when the list holds some", async () => {
    getMyCapabilitiesMock.mockResolvedValue({ may_self_host_runner: false });
    listMineMock.mockResolvedValue([MAC]);
    renderDialog();
    await pickLocalDir();
    expect(await screen.findByText("Personal runners aren't turned on for you. Ask your admin.")).toBeInTheDocument();
    expect(screen.queryByRole("radio", { name: /On my runner/ })).not.toBeInTheDocument();
    expect(screen.getAllByRole("radio")).toHaveLength(1);
    expect(screen.queryByText(/You have no runner of your own/)).not.toBeInTheDocument();
  });

  it("with personal runners off, shows the existing sentence instead", async () => {
    getMyCapabilitiesMock.mockResolvedValue({ may_self_host_runner: false });
    listMineMock.mockRejectedValue(new Error("runners_disabled"));
    renderDialog();
    await pickLocalDir();
    expect(await screen.findByText("Personal runners aren't turned on for you. Ask your admin.")).toBeInTheDocument();
    expect(screen.queryByText(/You have no runner of your own/)).not.toBeInTheDocument();
    expect(screen.getAllByRole("radio")).toHaveLength(1);
  });

  it("a failed read of the runners shows the server alone, and neither line", async () => {
    listMineMock.mockRejectedValue(new Error("boom"));
    renderDialog();
    await pickLocalDir();
    await waitFor(() => expect(listMineMock).toHaveBeenCalled());
    await screen.findByLabelText("Path on this host");
    expect(screen.getAllByRole("radio")).toHaveLength(1);
    expect(screen.queryByText(/You have no runner of your own/)).not.toBeInTheDocument();
    expect(screen.queryByText(/Personal runners aren't turned on/)).not.toBeInTheDocument();
  });

  it("shows neither line until /me/capabilities has answered, then the one that applies", async () => {
    let answer: (c: { may_self_host_runner: boolean }) => void = () => {};
    getMyCapabilitiesMock.mockReturnValue(new Promise((res) => (answer = res)));
    renderDialog();
    await pickLocalDir();
    await waitFor(() => expect(listMineMock).toHaveBeenCalled());
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.queryByText(/You have no runner of your own/)).not.toBeInTheDocument();
    expect(screen.queryByText(/Personal runners aren't turned on/)).not.toBeInTheDocument();
    answer({ may_self_host_runner: false });
    expect(await screen.findByText("Personal runners aren't turned on for you. Ask your admin.")).toBeInTheDocument();
    expect(screen.queryByText(/You have no runner of your own/)).not.toBeInTheDocument();
  });

  it("a failed refresh keeps the picked runner and still sends its id", async () => {
    listMineMock.mockResolvedValueOnce([MAC]).mockRejectedValue(new Error("boom"));
    renderDialog();
    await pickLocalDir();
    await userEvent.click(await screen.findByRole("radio", { name: "On my runner: ec2-mac-rc9" }));
    await userEvent.type(screen.getByLabelText("Path on ec2-mac-rc9"), "/Users/dana/work/payments");
    await userEvent.click(screen.getByRole("button", { name: /^repository/i }));
    await userEvent.click(screen.getByRole("button", { name: /^local directory/i }));
    await waitFor(() => expect(listMineMock).toHaveBeenCalledTimes(2));
    expect(screen.getByRole("radio", { name: "On my runner: ec2-mac-rc9" })).toBeChecked();
    expect(screen.getByLabelText("Path on ec2-mac-rc9")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: /^add workspace/i }));
    expect(createWorkspaceMock.mock.calls[0][0].sources[0]).toMatchObject({ path: "/Users/dana/work/payments", runner_id: MAC.id });
  });

  it("a refresh that no longer lists the picked runner clears it, visibly back on the server", async () => {
    listMineMock.mockResolvedValueOnce([MAC]).mockResolvedValue([]);
    renderDialog();
    await pickLocalDir();
    await userEvent.click(await screen.findByRole("radio", { name: "On my runner: ec2-mac-rc9" }));
    await userEvent.click(screen.getByRole("button", { name: /^repository/i }));
    await userEvent.click(screen.getByRole("button", { name: /^local directory/i }));
    expect(await screen.findByText(/You have no runner of your own/)).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: "On the Wardyn server (this host)" })).toBeChecked();
    expect(screen.getByLabelText("Path on this host")).toBeInTheDocument();
  });

  it("ties the visible label to the radio group, and the helper and the notice to it as its description", async () => {
    listMineMock.mockResolvedValue([MAC]);
    renderDialog();
    await pickLocalDir();
    const group = await screen.findByRole("radiogroup", { name: "Where is this folder?" });
    expect(group).toHaveAttribute("aria-labelledby", screen.getByText("Where is this folder?").id);
    expect(group).not.toHaveAccessibleDescription();
    await userEvent.click(screen.getByRole("radio", { name: "On my runner: ec2-mac-rc9" }));
    expect(group).toHaveAccessibleDescription("The runner checks the folder when the run starts. The run then runs only on that runner.");
  });

  it("describes the group by the no-runner line", async () => {
    renderDialog();
    await pickLocalDir();
    await screen.findByText(/You have no runner of your own/);
    expect(screen.getByRole("radiogroup", { name: "Where is this folder?" })).toHaveAccessibleDescription(
      "You have no runner of your own. Add one in Settings → My runner with Add this computer.",
    );
  });
});
