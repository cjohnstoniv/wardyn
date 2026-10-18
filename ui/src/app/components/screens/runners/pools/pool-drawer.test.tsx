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
import { aheadByHours } from "../../../../lib/test-clock";
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
  // What the server answers for a pool with no policy: no subjects, and the zero time.
  usePolicyMock.mockResolvedValue({ pool_id: "p-ci", subjects: [], revision: 1, updated_at: "0001-01-01T00:00:00Z" });
});
afterEach(() => cleanup());

describe("organization pool members", () => {
  it("shows an organization runner as an organization runner, beside a remote-hosted one", () => {
    const cluster = selfRunner({ id: "r-cluster", name: "cluster-a", owner: "", owner_kind: "org", trust_class: "org_cluster", machine_name: "node-1.corp.example.com" });
    renderDrawer(pool(), { members: [member(EU), member(cluster)] });
    const cards = document.querySelectorAll("[data-member]");
    expect(cards).toHaveLength(2);
    const org = within(cards[1] as HTMLElement);
    expect(org.getByText("cluster-a")).toBeInTheDocument();
    expect(org.getByText("Organization")).toBeInTheDocument();
    expect(org.queryByText("Self-hosted")).not.toBeInTheDocument();
    expect(org.getByText("node-1.corp.example.com")).toBeInTheDocument();
    expect(within(cards[0] as HTMLElement).getByText("Remote-hosted")).toBeInTheDocument();
    expect(screen.getByText("Remote-hosted and organization runners")).toBeInTheDocument();
  });
});

describe("organization pool members read by a pool member", () => {
  it("shows an organization runner the server sent without the people behind it, and prints no undefined", () => {
    const sent = selfRunner({ id: "r-cluster", name: "cluster-a", owner_kind: "org", trust_class: "org_cluster", machine_name: "node-1.corp.example.com" });
    // What a pool member reads: the owner, minter, trust writer and revoker keys are not sent at all.
    for (const k of ["owner", "minted_by", "minted_by_email", "trust_set_by", "trust_set_at", "revoked_by", "revoked_by_email", "revoke_reason"] as const) delete sent[k];
    renderDrawer(pool(), { members: [member(sent)] });
    const card = document.querySelector("[data-member]") as HTMLElement;
    expect(within(card).getByText("cluster-a")).toBeInTheDocument();
    expect(within(card).getByText("Organization")).toBeInTheDocument();
    expect(card.textContent).not.toMatch(/undefined|null/);
  });
});

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

  // Its runner takes 4 CPUs, 8 GB memory and 50 GB disk per run. The pool sets memory below that, disk equal
  // to it and CPU wider than it; a run that asks for nothing gets the pool's default in all three.
  const OWN = { background: { memory_mib: { default: 2048, cap: 4096 }, disk_mib: { default: 20480, cap: 51200 }, cpu_millis: { default: 1000, cap: 16000 } }, barriers: ["CC2" as const], max_concurrent_runs: 40 };

  it("shows a default and maximum the pool sets whether it is below, equal to or wider than its runners", async () => {
    renderDrawer(pool({ limits: OWN }));
    const sheet = await screen.findByRole("dialog");
    const row = within(sheet).getByText("Background tasks", { selector: "strong" }).closest("tr") as HTMLElement;
    const cell = (label: string) => row.querySelector(`[data-l="${label}: "]`) as HTMLElement;
    // Lower: the pool bounds the run.
    expect(cell("Memory")).toHaveTextContent(/^default 2 GB \(max 4 GB\)$/);
    // Equal: still the pool's own, and its default still applies.
    expect(cell("Disk")).toHaveTextContent(/^default 20 GB \(max 50 GB\)$/);
    // Wider: shown as set, with what its runners allow, which is what bounds the run.
    expect(within(cell("CPU")).getByText("default 1 CPU (max 16 CPUs)")).toBeInTheDocument();
    expect(within(cell("CPU")).getByText("Up to 4 CPUs, from its runners")).toBeInTheDocument();
    expect(within(sheet).getByText("Pool limits: memory per run narrowed by this pool; cpu, disk per run set by this pool; 40 runs maximum in this pool. Otherwise from its runners.")).toBeInTheDocument();
    expect(within(sheet).queryByText("This pool adds no limits of its own: it follows its runners' limits.")).toBeNull();
    expect(within(sheet).getByText("A run gets the strictest of your organization's policy, this pool's limits and its runner's per-run limits.")).toBeInTheDocument();
  });

  it("with no runners still shows what the pool sets, and a dash for what it leaves to them", async () => {
    renderDrawer(pool({ limits: { background: { memory_mib: { default: 2048, cap: 4096 } }, barriers: ["CC2"] } }), { members: [] });
    const sheet = await screen.findByRole("dialog");
    const row = within(sheet).getByText("Background tasks", { selector: "strong" }).closest("tr") as HTMLElement;
    expect(row.querySelector('[data-l="Memory: "]')).toHaveTextContent(/^default 2 GB \(max 4 GB\)$/);
    expect(row.querySelector('[data-l="CPU: "]')).toHaveTextContent(/^—$/);
  });

  it("says it adds no limits of its own only when it sets none", async () => {
    renderDrawer(pool({ limits: { background: {}, barriers: ["CC2"] } }));
    const sheet = await screen.findByRole("dialog");
    expect(within(sheet).getByText("This pool adds no limits of its own: it follows its runners' limits.")).toBeInTheDocument();
    expect(within(sheet).getByText("Up to 8 GB, from its runners")).toBeInTheDocument();
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
    usePolicyMock.mockResolvedValue({ pool_id: "p-ci", subjects: [{ subject_type: "group", subject: "platform-eng" }], revision: 9, updated_at: aheadByHours(-72) });
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

  it("says so in the sheet when a switch lost to someone else's change, and reads the pool again", async () => {
    const sentence = "Staging changed while this run was being set up. Review it and try again.";
    updateMock.mockRejectedValue(new HttpError(409, sentence, "runner_pool_stale"));
    const { onChanged } = renderDrawer(pool({ id: "p-stg", name: "Staging", revision: 2 }));
    const sheet = await screen.findByRole("dialog");
    await userEvent.click(within(sheet).getByRole("button", { name: "Switch off" }));
    await userEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Switch off" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(within(sheet).getByRole("alert")).toHaveTextContent(sentence);
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
  const POLICY = { pool_id: "p-ci", subjects: [{ subject_type: "group", subject: "platform-eng" }], revision: 3, updated_at: aheadByHours(-48) };

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
