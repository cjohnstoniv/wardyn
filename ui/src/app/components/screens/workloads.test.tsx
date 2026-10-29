/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Workloads screen and its drawer (mock r16 T3-a). The strings are written out, not read from the copy
// module, so a drift in it fails this suite.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { Workload } from "../../lib/types/workloads";
import { OperatorProvider } from "../wardyn/operator-context";
import { WorkloadsScreen } from "./workloads";

const listMock = vi.fn();
const restartMock = vi.fn();
const removeMock = vi.fn();
const historyMock = vi.fn();
vi.mock("../../lib/api/workloads", () => ({
  workloads: {
    list: (...a: unknown[]) => listMock(...a),
    restart: (...a: unknown[]) => restartMock(...a),
    remove: (...a: unknown[]) => removeMock(...a),
    history: (...a: unknown[]) => historyMock(...a),
  },
}));
const getRunMock = vi.fn();
vi.mock("../../lib/api/runs", () => ({ runs: { getRun: (...a: unknown[]) => getRunMock(...a) } }));
const toast = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn() }));
vi.mock("sonner", () => ({ toast }));

const MIN = 60_000;
const H = 60 * MIN;
const D = 24 * H;
const ago = (ms: number) => new Date(Date.now() - ms).toISOString();

const wl = (over: Partial<Workload>): Workload => ({
  name: "w",
  owner: "me",
  source: "api",
  intent: { experience: "background" },
  generation: 1,
  current_generation: 1,
  created_at: ago(9 * D),
  updated_at: ago(2.2 * MIN),
  ...over,
});

const ROWS: Workload[] = [
  wl({ name: "billing-api", owner: "service:billing", source: "k8s", namespace: "team-payments", generation: 4, current_generation: 4,
    run_id: "7c41e0a9-1111-2222-3333-444444444444", state: "RUNNING/unhealthy",
    last_refusal: { generation: 3, reason: "runner_pool_full", at: ago(3 * D) } }),
  wl({ name: "triage-bot", run_id: "3f9c2b17-1111-2222-3333-444444444444", state: "RUNNING/healthy", updated_at: ago(1.2 * H) }),
  wl({ name: "nightly-report", state: undefined, current_generation: 0, updated_at: ago(4.2 * MIN) }),
  wl({ name: "docs-preview", run_id: "51ab09d2-1111-2222-3333-444444444444", state: "COMPLETED", updated_at: ago(D + 2 * H) }),
  wl({ name: "search-indexer", owner: "service:search", source: "k8s", namespace: "team-search", generation: 2, current_generation: 1,
    run_id: "9e8d1c40-1111-2222-3333-444444444444", state: "RUNNING",
    last_refusal: { generation: 2, reason: "runner_pool_queue_full", message: "x", at: ago(20 * MIN) }, updated_at: ago(20.2 * MIN) }),
  wl({ name: "metrics-agg", owner: "service:metrics", source: "k8s", namespace: "team-metrics", generation: 2, current_generation: 0,
    state: undefined, last_refusal: { generation: 2, reason: "workload_binding_inactive", at: ago(3 * H) }, updated_at: ago(3.2 * H) }),
];

function renderScreen(path = "/workloads") {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <OperatorProvider operator={false} principal="me">
        <WorkloadsScreen />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  listMock.mockResolvedValue(ROWS);
  getRunMock.mockResolvedValue({ created_at: ago(2 * D) });
  historyMock.mockResolvedValue([]);
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("Workloads list", () => {
  it("has the mock's columns and a row for each workload, in every state", async () => {
    renderScreen();
    const table = await screen.findByRole("table", { name: "Workloads" });
    expect(within(table).getAllByRole("columnheader").map((c) => c.textContent)).toEqual(["Name", "Owner", "Source", "Run", "State", "Updated"]);
    const rows = within(table).getAllByTestId("workload-row");
    expect(rows).toHaveLength(6);
    const cells = (i: number) => within(rows[i]).getAllByRole("cell").map((c) => c.textContent);
    expect(cells(0)).toEqual(["billing-api", "service:billing", "Kubernetes: team-payments", "7c41e0a9", "Unhealthy", "2 min ago"]);
    expect(cells(1)).toEqual(["triage-bot", "You", "Wardyn CLI or API", "3f9c2b17", "Running", "1 h ago"]);
    expect(cells(2)).toEqual(["nightly-report", "You", "Wardyn CLI or API", "—", "Queued", "4 min ago"]);
    expect(cells(3)).toEqual(["docs-preview", "You", "Wardyn CLI or API", "51ab09d2", "Ended", "yesterday"]);
    expect(cells(4)).toEqual(["search-indexer", "service:search", "Kubernetes: team-search", "9e8d1c40", "Refused", "20 min ago"]);
    expect(cells(5)).toEqual(["metrics-agg", "service:metrics", "Kubernetes: team-metrics", "—", "Binding inactive", "3 h ago"]);
  });

  it("an empty list says how to apply one", async () => {
    listMock.mockResolvedValue([]);
    renderScreen();
    const empty = await screen.findByText((_, el) => el?.tagName === "P" && el.textContent === "No workloads yet. Apply one with wardyn apply -f workload.yaml.");
    expect(empty).toBeInTheDocument();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("a failed read shows the error with a retry", async () => {
    listMock.mockRejectedValueOnce(new Error("boom"));
    renderScreen();
    expect(await screen.findByText("boom")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("table")).toBeInTheDocument();
  });

  it("links a run to its page in the view's own path", async () => {
    renderScreen();
    const link = await screen.findByRole("link", { name: "7c41e0a9" });
    expect(link).toHaveAttribute("href", "/runs/7c41e0a9-1111-2222-3333-444444444444");
  });
});

describe("Workload drawer", () => {
  it("a Kubernetes workload shows its writer, the refusal as a sentence with the code in small print, and no Delete", async () => {
    renderScreen();
    await userEvent.click(await screen.findByRole("button", { name: "billing-api" }));
    const drawer = await screen.findByRole("dialog");
    expect(within(drawer).getByText("Managed by Kubernetes namespace", { exact: false })).toHaveTextContent(
      "Managed by Kubernetes namespace team-payments. Change it in Git.",
    );
    expect(within(drawer).getByText("Generation")).toBeInTheDocument();
    expect(within(drawer).getByText("Current run")).toBeInTheDocument();
    await waitFor(() => expect(within(drawer).getByText("started 2 d ago", { exact: false })).toBeInTheDocument());
    const refusal = within(drawer).getByTestId("workload-last-refusal");
    expect(refusal).toHaveTextContent("Generation 3 · The pool is running its limit of runs.");
    expect(refusal).not.toHaveTextContent("the previous run kept running.");
    expect(within(refusal).getByText("runner_pool_full")).toHaveClass("font-mono");
    expect(within(drawer).getByText("Spec")).toBeInTheDocument();
    expect(drawer.querySelector("code")?.textContent).toContain("name: billing-api");
    expect(within(drawer).getByRole("button", { name: "Restart" })).toBeInTheDocument();
    expect(within(drawer).queryByRole("button", { name: "Delete" })).toBeNull();
    expect(within(drawer).getByText("Delete is hidden: this workload is managed by Kubernetes.")).toBeInTheDocument();
  });

  it("a refused spec says the previous run kept running", async () => {
    renderScreen();
    await userEvent.click(await screen.findByRole("button", { name: "search-indexer" }));
    const refusal = within(await screen.findByRole("dialog")).getByTestId("workload-last-refusal");
    expect(refusal).toHaveTextContent("Generation 2 · The pool's queue is full. · the previous run kept running.");
  });

  it("a reason the console does not tell reads as the server's own sentence", async () => {
    listMock.mockResolvedValue([wl({ name: "odd", generation: 2, current_generation: 1, last_refusal: { generation: 2, reason: "some_new_reason", message: "The ceiling refused it.", at: ago(MIN) } })]);
    renderScreen();
    await userEvent.click(await screen.findByRole("button", { name: "odd" }));
    const refusal = within(await screen.findByRole("dialog")).getByTestId("workload-last-refusal");
    expect(refusal).toHaveTextContent("Generation 2 · The ceiling refused it.");
    expect(within(refusal).getByText("some_new_reason")).toBeInTheDocument();
  });

  it("a CLI workload offers Restart and Delete; Restart posts as the owner", async () => {
    restartMock.mockResolvedValue({ workload: ROWS[1], outcome: "unchanged", launch: { state: "launched" } });
    renderScreen();
    await userEvent.click(await screen.findByRole("button", { name: "triage-bot" }));
    const drawer = await screen.findByRole("dialog");
    expect(within(drawer).getByRole("button", { name: "Delete" })).toBeInTheDocument();
    await userEvent.click(within(drawer).getByRole("button", { name: "Restart" }));
    await waitFor(() => expect(restartMock).toHaveBeenCalledWith("triage-bot", "me"));
    expect(toast.success).toHaveBeenCalledWith("triage-bot restarted.");
  });

  it("a refused restart says why, in a sentence", async () => {
    restartMock.mockResolvedValue({ workload: ROWS[1], outcome: "unchanged", launch: { state: "refused", reason: "runner_pool_full", message: "x" } });
    renderScreen();
    await userEvent.click(await screen.findByRole("button", { name: "triage-bot" }));
    await userEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Restart" }));
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith("The pool is running its limit of runs."));
  });

  it("Delete asks first, then deletes and closes the drawer", async () => {
    removeMock.mockResolvedValue(undefined);
    renderScreen();
    await userEvent.click(await screen.findByRole("button", { name: "triage-bot" }));
    await userEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Delete" }));
    const confirm = await screen.findByRole("alertdialog");
    expect(within(confirm).getByText("Delete triage-bot?")).toBeInTheDocument();
    await userEvent.click(within(confirm).getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(removeMock).toHaveBeenCalledWith("triage-bot", "me"));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("the run header's link opens the drawer", async () => {
    renderScreen("/workloads?workload=billing-api&owner=service%3Abilling");
    expect(await screen.findByRole("dialog")).toBeInTheDocument();
  });
});
