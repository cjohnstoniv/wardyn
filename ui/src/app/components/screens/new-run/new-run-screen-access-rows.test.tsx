/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #1914 — the Access panel's rows on the New Run screen: they come from the
// policy preview's component facts, overlaid by this body's own preflight; a
// custom component that needs input holds Launch (D14), is counted on the nav
// and linked above Launch; and the run's components reach all three doors.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) },
}));
vi.mock("../../../lib/api/policies", () => ({
  policies: {
    listPolicies: () => Promise.resolve([]),
    createPolicy: vi.fn(),
    getDefaultPolicy: () => Promise.resolve({ min_confinement_class: "CC1" }),
  },
}));
const previewMock = vi.fn();
vi.mock("../../../lib/api/policy-preview", () => ({ previewRunPolicy: (...a: unknown[]) => previewMock(...a) }));
const preflightMock = vi.fn();
const createRunMock = vi.fn();
vi.mock("../../../lib/api/runs", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../../lib/api/runs")>();
  return {
    ...actual,
    runs: {
      createRun: (...a: unknown[]) => createRunMock(...a),
      listRuns: () => Promise.resolve([]),
      preflightRun: (...a: unknown[]) => preflightMock(...a),
      gradePolicy: () => Promise.resolve({ risk_assessment: [], overall_risk: "low" }),
    },
  };
});
vi.mock("../../../lib/api/workspaces", () => ({
  workspaces: { listWorkspaces: () => Promise.resolve([]) },
}));
vi.mock("../../../lib/capabilities", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/capabilities")>("../../../lib/capabilities");
  return { ...actual, useMyCapabilities: () => null };
});

import { NewRunScreen } from "./new-run-screen";
import { baseStatus } from "../../../lib/test-fixtures";
import { OperatorProvider } from "../../wardyn/operator-context";
import { ACCESS_ROWS as T } from "../../wardyn/copy/components";
import type { ComponentFact } from "../../../lib/types/components";
import { HttpError } from "../../../lib/api/core";
import { ADD_ACCESS } from "../../wardyn/copy/components";
import { goToPanel } from "../../../../test/new-run-panel";

const user = userEvent.setup({ pointerEventsCheck: 0 });

const needsInput: ComponentFact = {
  kind: "custom", id: "inline:0", name: "Report tool", reason: "inline", status: "needs_input", self_defined: true,
  requirements: [{ id: "secret:tok", kind: "secret", label: "Secret: tok", required_by: "a grant", status: "missing", fix: { action: "add_secret", secret_name: "tok" } }],
  hosts: ["reports.example"],
};
const ready: ComponentFact = { ...needsInput, status: "ready", requirements: [] };
const adoNeedsInput: ComponentFact = {
  kind: "git_provider", provider: "azure_devops", id: "git_provider:azure_devops:entra", reason: "workspace", status: "needs_input", requirements: [], lane: "entra",
};

const previewBody = (components?: ComponentFact[]) => ({ spec: {}, pending: [], warnings: [], repository_access: [], ...(components ? { components } : {}) });
const preflightBody = (components?: ComponentFact[]) => ({ setup_items: [], enforced_confinement_class: "CC1", overall_risk: "low", warnings: [], ...(components ? { components } : {}) });

function renderScreen(prefillComponents?: unknown[]) {
  return render(
    <MemoryRouter initialEntries={prefillComponents ? [{ pathname: "/runs/new", state: { prefill: { inlinePolicy: false, source: "composer", state: { components: prefillComponents } } } }] : ["/runs/new"]}>
      <OperatorProvider principal="test-owner" operator>
        <NewRunScreen />
      </OperatorProvider>
    </MemoryRouter>,
  );
}
const nav = () => within(screen.getByRole("navigation", { name: "New run" }));
const launch = () => screen.getByRole("button", { name: "Launch run" });

async function titled() {
  fireEvent.change(await screen.findByLabelText("Title"), { target: { value: "Refund flow" } });
}

beforeEach(() => {
  getSetupStatusMock.mockReset().mockResolvedValue(baseStatus());
  previewMock.mockReset().mockResolvedValue(previewBody());
  preflightMock.mockReset().mockResolvedValue(preflightBody());
  createRunMock.mockReset().mockResolvedValue({ id: "run_1" });
});

describe("NewRunScreen — Access rows", () => {
  it("shows one row per preview fact on the Access panel", async () => {
    previewMock.mockResolvedValue(previewBody([ready, adoNeedsInput]));
    renderScreen();
    await titled();
    goToPanel("Access");
    const list = await screen.findByRole("list", { name: T.LIST_LABEL });
    expect(list.querySelectorAll(":scope > li")).toHaveLength(2);
    expect(launch()).toBeEnabled();
  });

  it("holds Launch for a custom component that needs input: counted on the nav, linked above Launch", async () => {
    previewMock.mockResolvedValue(previewBody([needsInput]));
    renderScreen();
    await titled();
    expect(await nav().findByRole("button", { name: "Access 1 issue" })).toBeInTheDocument();
    expect(launch()).toBeDisabled();
    // From the Run panel the sentence is the link above Launch.
    const link = screen.getByRole("button", { name: T.ISSUE_NEEDS_INPUT("Report tool") });
    await user.click(link);
    // It shows the Access panel, opens that row and puts focus on it.
    expect(screen.getByRole("heading", { level: 2, name: "Access" })).toBeVisible();
    const row = screen.getByRole("button", { name: /^Report tool/ });
    expect(row).toHaveAttribute("aria-expanded", "true");
    expect(row).toHaveAttribute("aria-invalid", "true");
    expect(row).toHaveFocus();
    expect(screen.getByText("reports.example")).toBeInTheDocument();
  });

  it("prints the sentence once: beside the row while Access is on screen, not again above Launch", async () => {
    previewMock.mockResolvedValue(previewBody([needsInput]));
    renderScreen();
    await titled();
    await nav().findByRole("button", { name: "Access 1 issue" });
    goToPanel("Access");
    expect(screen.getAllByText(T.ISSUE_NEEDS_INPUT("Report tool"))).toHaveLength(1);
    expect(launch()).toBeDisabled();
  });

  it("does not hold Launch for a Git provider that needs input", async () => {
    previewMock.mockResolvedValue(previewBody([adoNeedsInput]));
    renderScreen();
    await titled();
    goToPanel("Access");
    await screen.findByRole("button", { name: /^Azure DevOps/ });
    expect(nav().getByRole("button", { name: "Access" })).toBeInTheDocument();
    expect(launch()).toBeEnabled();
  });

  it("lets a fresh preflight answer replace the preview's for the same component", async () => {
    previewMock.mockResolvedValue(previewBody([ready]));
    preflightMock.mockResolvedValue(preflightBody([needsInput]));
    renderScreen();
    await titled();
    goToPanel("Policy");
    await user.click(screen.getByRole("button", { name: /^Check again$/ }));
    expect(await nav().findByRole("button", { name: "Access 1 issue" })).toBeInTheDocument();
    expect(launch()).toBeDisabled();
  });

  it("frees Launch when the component no longer needs input", async () => {
    previewMock.mockResolvedValue(previewBody([needsInput]));
    renderScreen();
    await titled();
    await nav().findByRole("button", { name: "Access 1 issue" });
    previewMock.mockResolvedValue(previewBody([ready]));
    preflightMock.mockResolvedValue(preflightBody([ready]));
    goToPanel("Policy");
    await user.click(screen.getByRole("button", { name: /^Check again$/ }));
    expect(await nav().findByRole("button", { name: "Access" })).toBeInTheDocument();
    expect(launch()).toBeEnabled();
  });
});

describe("NewRunScreen — components reach the wire", () => {
  const refs = [{ id: "6f0c1d2e-0000-4000-8000-0000000c0301" }];

  it("sends the run's components to preview, preflight and create", async () => {
    renderScreen(refs);
    await titled();
    await vi.waitFor(() => expect(previewMock).toHaveBeenCalled());
    await vi.waitFor(() => expect(preflightMock).toHaveBeenCalled());
    expect(previewMock.mock.calls[0][0].components).toEqual(refs);
    expect(preflightMock.mock.calls[0][0].components).toEqual(refs);
    await vi.waitFor(() => expect(launch()).toBeEnabled());
    await user.click(launch());
    await vi.waitFor(() => expect(createRunMock).toHaveBeenCalled());
    expect(createRunMock.mock.calls[0][0].components).toEqual(refs);
  });

  it("sends none when the run carries none", async () => {
    renderScreen();
    await titled();
    await vi.waitFor(() => expect(previewMock).toHaveBeenCalled());
    expect(previewMock.mock.calls[0][0]).not.toHaveProperty("components");
  });
});

describe("NewRunScreen — a component the server refused", () => {
  const SENTENCE = "api.openai.com serves a model on this deployment, so a component can't reach it.";
  const bad = [{ inline: { hosts: ["api.openai.com"] }, name: "Bad one" }];
  // The server refuses every read while the ref is on the body, and answers once it is gone.
  const refuseWhileCarried = (body: unknown) => (body as { components?: unknown[] }).components?.length
    ? Promise.reject(new HttpError(422, SENTENCE, "component_host_serves_model"))
    : null;

  beforeEach(() => {
    previewMock.mockImplementation((b) => refuseWhileCarried(b) ?? Promise.resolve(previewBody()));
    preflightMock.mockImplementation((b) => refuseWhileCarried(b) ?? Promise.resolve(preflightBody()));
  });

  it("keeps a Refused row with the server's sentence and a Remove control, and holds Launch", async () => {
    renderScreen(bad);
    await titled();
    expect(await nav().findByRole("button", { name: "Access 1 issue" })).toBeInTheDocument();
    expect(launch()).toBeDisabled();
    goToPanel("Access");
    const row = screen.getByRole("button", { name: /^Bad one/ });
    expect(row).toHaveAttribute("aria-invalid", "true");
    expect(within(row).getByText(T.STATUS.refused)).toBeInTheDocument();
    expect(screen.getByText(T.ISSUE_REFUSED("Bad one"))).toBeInTheDocument();
    expect(within(row.closest("li")!).getByText(SENTENCE)).toBeInTheDocument();
    await user.click(row);
    expect(screen.getByRole("button", { name: ADD_ACCESS.REMOVE_NAMED("Bad one") })).toBeInTheDocument();
  });

  it("removing it takes the ref off every body and clears the refusal", async () => {
    renderScreen(bad);
    await titled();
    await nav().findByRole("button", { name: "Access 1 issue" });
    goToPanel("Access");
    await user.click(screen.getByRole("button", { name: /^Bad one/ }));
    await user.click(screen.getByRole("button", { name: ADD_ACCESS.REMOVE_NAMED("Bad one") }));
    expect(screen.queryByRole("button", { name: /^Bad one/ })).toBeNull();
    await vi.waitFor(() => expect(previewMock.mock.calls.at(-1)?.[0]).not.toHaveProperty("components"));
    expect(await nav().findByRole("button", { name: "Access" })).toBeInTheDocument();
    await vi.waitFor(() => expect(launch()).toBeEnabled());
  });

  it("builds no refused row from a refusal that is not about components", async () => {
    previewMock.mockRejectedValue(new HttpError(422, "barrier unavailable", "backend_unavailable"));
    preflightMock.mockRejectedValue(new HttpError(422, "barrier unavailable", "backend_unavailable"));
    renderScreen(bad);
    await titled();
    goToPanel("Access");
    await vi.waitFor(() => expect(previewMock).toHaveBeenCalled());
    expect(screen.queryByText(T.ISSUE_REFUSED("Bad one"))).toBeNull();
  });
});
