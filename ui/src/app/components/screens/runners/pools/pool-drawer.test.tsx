/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The pool details sheet (mock M-POOLS v6 A5, A8, B2). The strings are written out, not read from the copy
// module, so a drift in copy/runner-pools.ts fails this suite.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { RunnerPool } from "../../../../lib/types/runner-pools";

const usePolicyMock = vi.fn();
const setUsePolicyMock = vi.fn();
const clearUsePolicyMock = vi.fn();
const updateMock = vi.fn();
const removeMock = vi.fn();
const removeMineMock = vi.fn();
vi.mock("../../../../lib/api/runner-pools", () => ({
  runnerPools: {
    usePolicy: (...a: unknown[]) => usePolicyMock(...a),
    setUsePolicy: (...a: unknown[]) => setUsePolicyMock(...a),
    clearUsePolicy: (...a: unknown[]) => clearUsePolicyMock(...a),
    update: (...a: unknown[]) => updateMock(...a),
    remove: (...a: unknown[]) => removeMock(...a),
    removeMine: (...a: unknown[]) => removeMineMock(...a),
  },
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

import { HttpError, PendingChangeError } from "../../../../lib/api/core";
import { OperatorProvider } from "../../../wardyn/operator-context";
import { setField } from "../../../../../test/set-field";
import { PoolDrawer } from "./pool-drawer";
import { member, pool, remoteRunner, selfRunner } from "./pool-test-fixtures";

const EU = remoteRunner();
const OFFLINE = selfRunner({ id: "r-desk", name: "desk-2", online: false, usage: { ...selfRunner().usage, online: false, available: { runs: 0, cpu_millis: 0, memory_mib: 0, disk_mib: 0 } } });

type Props = Partial<React.ComponentProps<typeof PoolDrawer>>;
function renderDrawer(p: RunnerPool, props: Props = {}, operator = true) {
  const handlers = { onClose: vi.fn(), onChanged: vi.fn(), onEdit: vi.fn(), onSetDefault: vi.fn().mockResolvedValue(undefined), onOpenRunner: vi.fn() };
  const view = render(
    <OperatorProvider operator={operator} securityOperator operatorResolved principal="dana@example.com">
      <PoolDrawer pool={p} members={[member(EU)]} isDefault={false} mine={false} {...handlers} {...props} />
    </OperatorProvider>,
  );
  return { ...view, ...handlers };
}

beforeEach(() => {
  for (const m of [usePolicyMock, setUsePolicyMock, clearUsePolicyMock, updateMock, removeMock, removeMineMock]) m.mockReset();
  usePolicyMock.mockResolvedValue({ pool_id: "p-ci", subjects: [], revision: 1, updated_at: "" });
});
afterEach(() => cleanup());

describe("organization default pool", () => {
  it("shows its chips, what it allows, its resources and the default with Clear default", async () => {
    const p = pool({ id: "p-gen", name: "General", limits: { barriers: ["CC1", "CC2", "CC3"] } });
    const { onSetDefault } = renderDrawer(p, { isDefault: true, waiting: 2 });
    const sheet = await screen.findByRole("dialog");
    expect(within(sheet).getByRole("heading", { name: "General" })).toBeInTheDocument();
    for (const chip of ["Organization pool", "Active", "✓ Default"]) expect(within(sheet).getByText(chip)).toBeInTheDocument();
    expect(within(sheet).getByText("Interactive and background")).toBeInTheDocument();
    expect(within(sheet).getByText("Fence, Wall, Vault")).toBeInTheDocument();
    expect(within(sheet).getAllByText("Biggest run it can take").length).toBeGreaterThan(0);
    expect(within(sheet).getByText("This pool adds no limits of its own: it follows its runners' limits.")).toBeInTheDocument();
    expect(within(sheet).getByText("This is the organization default.")).toBeInTheDocument();
    expect(within(sheet).getByText("Machine: not applicable (remote-hosted)")).toBeInTheDocument();
    expect(within(sheet).getByText("Runs at once: 4 · Running now: 1 of 4")).toBeInTheDocument();
    expect(within(sheet).getByText("Revision 9 ·", { exact: false })).toBeInTheDocument();
    await userEvent.click(within(sheet).getByRole("button", { name: "Clear default" }));
    expect(onSetDefault).toHaveBeenCalledWith(null);
  });

  it("reads a cap no tighter than its runners as what its runners allow, and a narrowing one as default and max", async () => {
    // The pool caps memory at 4 GB (its runner takes 8) and CPU at 16 (its runner takes 4): only memory is narrowed.
    const p = pool({ limits: { background: { memory_mib: { default: 2048, cap: 4096 }, cpu_millis: { default: 1000, cap: 16000 } }, barriers: ["CC2"], max_concurrent_runs: 40 } });
    renderDrawer(p);
    const sheet = await screen.findByRole("dialog");
    const row = within(sheet).getByText("Background tasks", { selector: "strong" }).closest("tr") as HTMLElement;
    expect(within(row).getByText("default 2 GB (max 4 GB)")).toBeInTheDocument();
    expect(within(row).getByText("Up to 4 CPUs, from its runners")).toBeInTheDocument();
    expect(within(row).getByText("Up to 50 GB, from its runners")).toBeInTheDocument();
    expect(within(sheet).getByText("Pool limits: memory per run narrowed by this pool; 40 runs maximum in this pool. Otherwise from its runners.")).toBeInTheDocument();
    expect(within(sheet).getByText("A run gets the strictest of your organization's policy, this pool's limits and its runner's per-run limits.")).toBeInTheDocument();
  });

  it("with no runners says nothing can run there", async () => {
    renderDrawer(pool(), { members: [] });
    expect(await screen.findByText("No runners yet, so nothing can run here until one is added.")).toBeInTheDocument();
    expect(screen.getByText("None yet. Only its owner chooses runners.")).toBeInTheDocument();
  });
});

describe("user pool", () => {
  const user = pool({ id: "p-lap", name: "Laptop", scope: "user", owner: "dana@example.com", limits: { barriers: ["CC1", "CC2"] } });

  it("is read-only for an administrator and says who can use it", async () => {
    const { onOpenRunner } = renderDrawer(user, { members: [member(selfRunner()), member(EU)] });
    const sheet = await screen.findByRole("dialog");
    expect(within(sheet).getByText("User pool")).toBeInTheDocument();
    expect(within(sheet).getByText("Only dana@example.com can use it. Only they can change it or choose its runners.")).toBeInTheDocument();
    expect(within(sheet).getByText("Read-only. Only dana@example.com can edit or delete this pool.")).toBeInTheDocument();
    for (const b of ["Edit", "Switch off", "Delete"]) expect(within(sheet).queryByRole("button", { name: b })).toBeNull();
    expect(within(sheet).queryByText("Organization default")).toBeNull();
    expect(usePolicyMock).not.toHaveBeenCalled();
    expect(within(sheet).getByText("dana-mbp-14.corp.example.com")).toBeInTheDocument();
    await userEvent.click(within(sheet).getByRole("button", { name: "laptop-7" }));
    expect(onOpenRunner).toHaveBeenCalledWith("self_hosted", "r-laptop");
  });

  it("says a pool with a use policy can't be deleted before the server is asked", async () => {
    usePolicyMock.mockResolvedValue({ pool_id: "p-ci", subjects: [{ subject_type: "group", subject: "platform-eng" }], revision: 9, updated_at: "2026-10-09T10:00:00Z" });
    renderDrawer(pool({ id: "p-ci", name: "CI jobs" }), { waiting: 3 });
    const sheet = await screen.findByRole("dialog");
    await within(sheet).findByText("platform-eng");
    await userEvent.click(within(sheet).getByRole("button", { name: "Delete" }));
    const dlg = await screen.findByRole("alertdialog");
    expect(within(dlg).getByText(/3 runs are waiting\./)).toBeInTheDocument();
    expect(within(dlg).getByRole("alert")).toHaveTextContent("clear the use policy first: deleting a pool would clear it without a second human");
    expect(within(dlg).getByRole("button", { name: "Delete pool" })).toBeDisabled();
    expect(removeMock).not.toHaveBeenCalled();
  });

  it("offers Edit and Delete to its owner, and Delete goes through their own route", async () => {
    const { onEdit, onChanged, onClose } = renderDrawer(user, { mine: true, members: [member(selfRunner())] });
    const sheet = await screen.findByRole("dialog");
    expect(within(sheet).getByText("The runners dana@example.com chose for this pool. A run goes to one of dana@example.com's own runners with room first, then to a remote-hosted runner.")).toBeInTheDocument();
    await userEvent.click(within(sheet).getByRole("button", { name: "Edit" }));
    expect(onEdit).toHaveBeenCalledWith(user);
    removeMineMock.mockResolvedValue(undefined);
    await userEvent.click(within(sheet).getByRole("button", { name: "Delete" }));
    const dlg = await screen.findByRole("alertdialog");
    expect(within(dlg).getByText("Delete Laptop?")).toBeInTheDocument();
    expect(within(dlg).getByText(/Your self-hosted runners run nothing until you put them in another pool/)).toBeInTheDocument();
    expect(within(dlg).queryByText(/People who saved it as their default/)).toBeNull();
    await userEvent.click(within(dlg).getByRole("button", { name: "Delete pool" }));
    await waitFor(() => expect(removeMineMock).toHaveBeenCalledWith("p-lap"));
    expect(removeMock).not.toHaveBeenCalled();
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(onChanged).toHaveBeenCalled();
  });
});

describe("switched-off pool", () => {
  const off = pool({ state: "disabled" });

  it("says its capacity isn't counted and that it can't be the default, and offers Switch on", async () => {
    renderDrawer(off, { members: [member(EU), member(OFFLINE)] });
    const sheet = await screen.findByRole("dialog");
    expect(within(sheet).getByText("Switched off")).toBeInTheDocument();
    expect(within(sheet).getByText("Switched off: nothing can start here, so its capacity isn't counted as available.")).toBeInTheDocument();
    expect(within(sheet).getByText("A switched-off pool can't be the default.")).toBeInTheDocument();
    expect(within(sheet).getAllByText("Not counted")).toHaveLength(4);
    expect(within(sheet).getByText("1 of its 2 runners is offline. Its capacity isn't counted as available.")).toBeInTheDocument();
    expect(within(sheet).getByText("(offline: not counted)", { exact: false })).toBeInTheDocument();
    expect(within(sheet).getByRole("button", { name: "Switch on" })).toBeInTheDocument();
    expect(within(sheet).queryByRole("button", { name: "Make default" })).toBeNull();
  });

  it("switches on in one step", async () => {
    updateMock.mockResolvedValue({});
    const { onChanged } = renderDrawer(off);
    await userEvent.click(await screen.findByRole("button", { name: "Switch on" }));
    await waitFor(() => expect(updateMock).toHaveBeenCalledWith("p-ci", { revision: 9, state: "active" }));
    expect(onChanged).toHaveBeenCalled();
  });

  it("switches an active pool off after a confirmation", async () => {
    updateMock.mockResolvedValue({});
    renderDrawer(pool());
    await userEvent.click(await screen.findByRole("button", { name: "Switch off" }));
    const dlg = await screen.findByRole("alertdialog");
    expect(within(dlg).getByText("Switch off CI jobs?")).toBeInTheDocument();
    await userEvent.click(within(dlg).getByRole("button", { name: "Switch off" }));
    await waitFor(() => expect(updateMock).toHaveBeenCalledWith("p-ci", { revision: 9, state: "disabled" }));
  });

  it("tells a security admin only a super admin edits, switches off or deletes", async () => {
    renderDrawer(pool(), {}, false);
    expect(await screen.findByText("Only a super admin can edit, switch off or delete a pool.")).toBeInTheDocument();
    expect(screen.getByText("Super admin only")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();
    expect(screen.getByRole("button", { name: "Edit who may use it" })).toBeInTheDocument();
  });
});

describe("who may use it", () => {
  const POLICY = { pool_id: "p-ci", subjects: [{ subject_type: "group", subject: "platform-eng" }], revision: 3, updated_at: "2026-10-10T10:00:00Z" };

  it("says everyone may use a pool with no policy, and nobody when its people have left", async () => {
    const { unmount } = renderDrawer(pool());
    expect(await screen.findByText("Everyone who may launch runs on organization pools can use this pool.")).toBeInTheDocument();
    unmount();
    usePolicyMock.mockResolvedValue({ ...POLICY, subjects: [] });
    renderDrawer(pool());
    expect(await screen.findByText("Nobody can use this pool.")).toBeInTheDocument();
    expect(screen.getByText("Everyone it listed has left. Add people, groups or roles, or clear the policy.")).toBeInTheDocument();
  });

  it("is held, not saved, when a second person must approve the change", async () => {
    usePolicyMock.mockResolvedValue(POLICY);
    setUsePolicyMock.mockRejectedValue(new PendingChangeError({} as never));
    renderDrawer(pool());
    await userEvent.click(await screen.findByRole("button", { name: "Edit who may use it" }));
    setField(screen.getByLabelText("Name"), "lee@example.com");
    await userEvent.click(screen.getByRole("button", { name: "Add to list" }));
    expect(screen.getByText("lee@example.com")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Save who may use it" }));
    await waitFor(() => expect(setUsePolicyMock).toHaveBeenCalledWith("p-ci", 3, [POLICY.subjects[0], { subject_type: "user", subject: "lee@example.com" }]));
    expect(await screen.findByText("Held for a second person.")).toBeInTheDocument();
    expect(screen.getByText("Nothing changes until they approve it in Governance.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Edit who may use it" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Save who may use it" })).toBeNull();
  });

  it("shows the server's sentence under the editor when a save is refused, and refuses an empty or repeated name", async () => {
    usePolicyMock.mockResolvedValue(POLICY);
    setUsePolicyMock.mockRejectedValue(new HttpError(422, "a use policy lists 1 to 64 people, groups or roles"));
    renderDrawer(pool());
    await userEvent.click(await screen.findByRole("button", { name: "Edit who may use it" }));
    await userEvent.click(screen.getByRole("button", { name: "Add to list" }));
    expect(screen.getByRole("alert")).toHaveTextContent("Name a person, group or role to add.");
    await userEvent.selectOptions(screen.getByLabelText("Add"), "group");
    setField(screen.getByLabelText("Name"), "platform-eng");
    await userEvent.click(screen.getByRole("button", { name: "Add to list" }));
    expect(screen.getByRole("alert")).toHaveTextContent('group "platform-eng" is listed twice');
    await userEvent.click(screen.getByRole("button", { name: "Remove platform-eng" }));
    await userEvent.click(screen.getByRole("button", { name: "Save who may use it" }));
    expect(await screen.findByText("a use policy lists 1 to 64 people, groups or roles")).toBeInTheDocument();
  });

  it("clears the policy after a confirmation", async () => {
    usePolicyMock.mockResolvedValue(POLICY);
    clearUsePolicyMock.mockResolvedValue(undefined);
    renderDrawer(pool());
    await userEvent.click(await screen.findByRole("button", { name: "Edit who may use it" }));
    await userEvent.click(screen.getByRole("button", { name: "Clear the policy" }));
    const dlg = await screen.findByRole("alertdialog");
    expect(within(dlg).getByText("Let everyone use CI jobs?")).toBeInTheDocument();
    await userEvent.click(within(dlg).getByRole("button", { name: "Clear the policy" }));
    await waitFor(() => expect(clearUsePolicyMock).toHaveBeenCalledWith("p-ci"));
  });
});
