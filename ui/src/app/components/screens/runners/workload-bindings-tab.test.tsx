/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Admin -> Runners -> Workload bindings and its two dialogs (mock r16 T3-b). The strings are written out, not
// read from the copy module, so a drift in it fails this suite.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { WorkloadBindingsTab } from "./workload-bindings-tab";

const listMock = vi.fn();
const createMock = vi.fn();
const removeMock = vi.fn();
vi.mock("../../../lib/api/workloads", () => ({
  workloadBindings: {
    list: (...a: unknown[]) => listMock(...a),
    create: (...a: unknown[]) => createMock(...a),
    remove: (...a: unknown[]) => removeMock(...a),
  },
}));
const poolsMock = vi.fn();
vi.mock("../../../lib/api/runner-pools", () => ({ runnerPools: { list: (...a: unknown[]) => poolsMock(...a) } }));
const servicesMock = vi.fn();
vi.mock("../../../lib/api/service-identities", () => ({ serviceIdentities: { listAll: (...a: unknown[]) => servicesMock(...a) } }));
const toast = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn() }));
vi.mock("sonner", () => ({ toast }));

const D = 24 * 60 * 60 * 1000;
const ago = (ms: number) => new Date(Date.now() - ms).toISOString();
const PLATFORM = "11111111-1111-1111-1111-111111111111";
const BATCH = "22222222-2222-2222-2222-222222222222";

const binding = (id: string, pool_id: string, namespace: string, service: string, sponsor: string, age: number) => ({
  id, pool_id, namespace, service, sponsor, created_by: sponsor, created_at: ago(age),
});

beforeEach(() => {
  listMock.mockResolvedValue([
    binding("b1", PLATFORM, "team-payments", "billing", "ana@example.com", 12.2 * D),
    binding("b2", PLATFORM, "team-search", "search", "kofi@example.com", 5.2 * D),
    binding("b3", BATCH, "team-metrics", "metrics", "lena@example.com", 61 * D),
  ]);
  poolsMock.mockResolvedValue({
    pools: [
      { id: PLATFORM, name: "Platform services", scope: "organization" },
      { id: BATCH, name: "Batch", scope: "organization" },
      { id: "u1", name: "My pool", scope: "user" },
    ],
  });
  servicesMock.mockResolvedValue([
    { name: "billing", state: "active" },
    { name: "search", state: "sponsor_inactive" },
    { name: "old", state: "retired" },
  ]);
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("Workload bindings tab", () => {
  it("lists each binding with the mock's columns", async () => {
    render(<WorkloadBindingsTab />);
    const rows = await screen.findAllByTestId("binding-row");
    expect(within(screen.getByRole("table")).getAllByRole("columnheader").slice(0, 5).map((c) => c.textContent)).toEqual([
      "Pool", "Namespace", "Service identity", "Sponsor", "Created",
    ]);
    const cells = (i: number) => within(rows[i]).getAllByRole("cell").map((c) => c.textContent);
    expect(cells(0)).toEqual(["Platform services", "team-payments", "service:billing", "ana@example.com", "12 days ago", "Delete"]);
    expect(cells(1)).toEqual(["Platform services", "team-search", "service:search", "kofi@example.com", "5 days ago", "Delete"]);
    expect(cells(2)).toEqual(["Batch", "team-metrics", "service:metrics", "lena@example.com", "2 months ago", "Delete"]);
  });

  it("says so when nothing is bound", async () => {
    listMock.mockResolvedValue([]);
    render(<WorkloadBindingsTab />);
    expect(await screen.findByText("No namespaces are bound yet.")).toBeInTheDocument();
  });

  it("Bind namespace shows the safety sentence, offers organization pools and live identities, and binds by pool id", async () => {
    createMock.mockResolvedValue({});
    render(<WorkloadBindingsTab />);
    await screen.findAllByTestId("binding-row");
    await userEvent.click(screen.getByRole("button", { name: "Bind namespace" }));
    const dialog = await screen.findByRole("dialog", { name: "Bind namespace" });
    expect(within(dialog).getByText("Anyone who can create a WardynWorkload in this namespace will launch runs as this service identity, in this pool.")).toBeInTheDocument();
    await userEvent.click(within(dialog).getByRole("combobox", { name: /Pool/ }));
    expect(screen.getAllByRole("option").map((o) => o.textContent)).toEqual(["Platform services", "Batch"]);
    await userEvent.click(screen.getByRole("option", { name: "Platform services" }));
    await userEvent.type(within(dialog).getByRole("textbox", { name: /Namespace/ }), "team-payments");
    await userEvent.click(within(dialog).getByRole("combobox", { name: /Service identity/ }));
    expect(screen.getAllByRole("option").map((o) => o.textContent)).toEqual(["service:billing", "service:search"]);
    await userEvent.click(screen.getByRole("option", { name: "service:billing" }));
    await userEvent.click(within(dialog).getByRole("button", { name: "Bind namespace" }));
    await waitFor(() => expect(createMock).toHaveBeenCalledWith({ pool: PLATFORM, namespace: "team-payments", service: "billing" }));
    expect(toast.success).toHaveBeenCalledWith("team-payments bound.");
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("a refused bind keeps the dialog and says the server's sentence", async () => {
    createMock.mockRejectedValue(new Error("That namespace is bound on this pool already. Delete that binding first."));
    render(<WorkloadBindingsTab />);
    await screen.findAllByTestId("binding-row");
    await userEvent.click(screen.getByRole("button", { name: "Bind namespace" }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.click(within(dialog).getByRole("combobox", { name: /Pool/ }));
    await userEvent.click(screen.getByRole("option", { name: "Batch" }));
    await userEvent.type(within(dialog).getByRole("textbox", { name: /Namespace/ }), "team-metrics");
    await userEvent.click(within(dialog).getByRole("combobox", { name: /Service identity/ }));
    await userEvent.click(screen.getByRole("option", { name: "service:search" }));
    await userEvent.click(within(dialog).getByRole("button", { name: "Bind namespace" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent("That namespace is bound on this pool already. Delete that binding first.");
  });

  it("Delete asks with the mock's title and body, then removes the binding", async () => {
    removeMock.mockResolvedValue(undefined);
    render(<WorkloadBindingsTab />);
    const rows = await screen.findAllByTestId("binding-row");
    await userEvent.click(within(rows[0]).getByRole("button", { name: "Delete team-payments" }));
    const confirm = await screen.findByRole("alertdialog");
    expect(within(confirm).getByText("Delete the binding for team-payments?")).toBeInTheDocument();
    expect(within(confirm).getByText("Runs already started keep running.")).toBeInTheDocument();
    await userEvent.click(within(confirm).getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(removeMock).toHaveBeenCalledWith("b1"));
  });
});
