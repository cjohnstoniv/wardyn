/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The rail's honesty rules, pinned where the SetupStatus is controlled. e2e
// cannot assert these: whether a model provider exists there depends on whether
// the machine running the suite has a logged-in Claude CLI, which setupProviders
// detects — so the same assertion passes on a laptop and fails on CI.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) },
}));
vi.mock("../../../lib/api/health", () => ({
  health: { health: () => Promise.resolve({ confinement_classes: ["CC1"] }) },
}));
vi.mock("../../../lib/api/policies", () => ({
  policies: { listPolicies: () => Promise.resolve([]), createPolicy: vi.fn() },
}));
vi.mock("../../../lib/api/runs", () => ({ runs: { createRun: vi.fn(), listRuns: () => Promise.resolve([]) } }));
const listWorkspacesMock = vi.fn();
vi.mock("../../../lib/api/workspaces", () => ({
  workspaces: { listWorkspaces: (...a: unknown[]) => listWorkspacesMock(...a) },
}));

import { NewRunScreen } from "./new-run-screen";
import { baseStatus } from "../../../lib/test-fixtures";

const user = userEvent.setup({ pointerEventsCheck: 0 });

function renderScreen() {
  return render(
    <MemoryRouter>
      <NewRunScreen />
    </MemoryRouter>,
  );
}

beforeEach(() => {
  getSetupStatusMock.mockReset().mockResolvedValue(baseStatus());
  listWorkspacesMock.mockReset().mockResolvedValue([]);
});

// Regression: useWorkspaceList does NOT fetch on mount — each caller loads it
// itself. This screen didn't, so its only fetch was the Add-workspace dialog's
// onCreated callback, and the Workspace select offered nothing but "Ephemeral
// scratch". A workspace onboarded in Getting started or on the Workspaces
// screen could not be attached to a run at all: the only way into the list was
// to re-add it from this page, in this session. Caught by the demo recording
// driver, which onboards a workspace in one act and attaches it in the next.
describe("NewRunScreen — an already-onboarded workspace is attachable", () => {
  it("loads the workspace list on mount", async () => {
    renderScreen();
    await waitFor(() => expect(listWorkspacesMock).toHaveBeenCalled());
  });
});

describe("NewRunScreen — the rail tells the truth about model access", () => {
  // An agent run with no provider launches and then fails its first model call.
  // Saying so at launch time is the difference between a warning and a support
  // ticket.
  it("warns when an AGENT run has no model provider", async () => {
    renderScreen();
    expect(await screen.findByText(/No model provider is connected/)).toBeInTheDocument();
  });

  it("says nothing when a provider IS connected", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({ secrets: { present: ["anthropic-api-key"], github_app: false } }),
    );
    renderScreen();
    await waitFor(() => expect(getSetupStatusMock).toHaveBeenCalled());
    expect(screen.queryByText(/No model provider is connected/)).not.toBeInTheDocument();
  });

  // A shell command needs no model at all — warning there would be noise.
  it("says nothing for a shell command, which needs no model", async () => {
    renderScreen();
    await screen.findByText(/No model provider is connected/);
    await user.click(screen.getByRole("radio", { name: "Shell command" }));
    expect(screen.queryByText(/No model provider is connected/)).not.toBeInTheDocument();
  });

  // getSetupStatus resolves a synthetic fallback rather than rejecting, so an
  // unknown answer must stay unknown — never an accusation on a blip.
  it("stays silent while the answer is unknown", async () => {
    // A deferred, not a never-settling promise: an unresolved fetch left
    // hanging times out vitest's teardown hook rather than the assertion.
    let settle: (v: unknown) => void = () => {};
    getSetupStatusMock.mockReturnValue(new Promise((r) => (settle = r)));
    renderScreen();
    expect(screen.queryByText(/No model provider is connected/)).not.toBeInTheDocument();
    settle(baseStatus());
    await waitFor(() => expect(screen.getByText(/No model provider is connected/)).toBeInTheDocument());
  });
});

describe("NewRunScreen — the rail counts hosts the way the policy does", () => {
  // impliedEgressHosts can name a host already in allowedDomains
  // (api.anthropic.com is in the default state) and buildSpec dedupes on the
  // wire — a rail saying 2 where the policy carries 1 would be a rail that lies.
  it("counts the union, not the sum", async () => {
    renderScreen();
    expect(await screen.findByText("1 host allowed")).toBeInTheDocument();
  });

  it("pluralises", async () => {
    renderScreen();
    await user.click(await screen.findByRole("radio", { name: /Common package registries/ }));
    expect(screen.getByText(/^1[0-9] hosts allowed$/)).toBeInTheDocument();
  });
});

// The form's fields FOLLOW the run mode. This screen used to show one Task box
// for every run, including interactive ones — where the server used to ignore
// task entirely, so the operator typed a prompt nothing would ever read. Task
// now rides as an interactive run's optional boot seed (Part A1), but it is
// still never the field literally labeled "Task" — that label stays batch-only,
// and an interactive run gets its own "Initial prompt" / "Startup command"
// field instead (new-run-screen.tsx's isInteractive branch).
describe("NewRunScreen — the form matches the run mode", () => {
  it("asks an interactive run what to start with, not for a task", async () => {
    renderScreen();
    // Interactive is the default (initialWizardState), so this is the state the
    // screen opens in.
    expect(await screen.findByRole("radiogroup", { name: "Start with" })).toBeInTheDocument();
    expect(screen.queryByLabelText("Task")).not.toBeInTheDocument();
  });

  it("asks an autonomous run for a task, and drops the startup choice", async () => {
    renderScreen();
    await user.click(await screen.findByRole("radio", { name: /^Autonomous/ }));
    expect(screen.getByLabelText("Task")).toBeInTheDocument();
    expect(screen.queryByRole("radiogroup", { name: "Start with" })).not.toBeInTheDocument();
  });

  // A shell command is unattended by definition. Offering "Interactive" for one
  // used to produce a run that silently never executed the command (the server
  // drops task_mode for an interactive run).
  it("hides the run mode for a shell command, which is always unattended", async () => {
    renderScreen();
    await user.click(await screen.findByRole("radio", { name: "Shell command" }));
    expect(screen.getByLabelText("Command")).toBeInTheDocument();
    expect(screen.queryByRole("radiogroup", { name: "Run mode" })).not.toBeInTheDocument();
  });
});

// Before this, the screen had NO client-side validation at all: an empty form
// launched, and the server's answer arrived after the fact.
describe("NewRunScreen — Launch says what it is waiting for", () => {
  it("is disabled without a title, and says so", async () => {
    renderScreen();
    const launch = await screen.findByRole("button", { name: /Launch run/ });
    expect(launch).toBeDisabled();
    expect(screen.getByText("Give this run a title.")).toBeInTheDocument();

    await user.type(screen.getByLabelText("Title"), "Refund flow");
    expect(launch).toBeEnabled();
  });

  it("still waits for the task on an autonomous run", async () => {
    renderScreen();
    await user.type(await screen.findByLabelText("Title"), "Refund flow");
    await user.click(screen.getByRole("radio", { name: /^Autonomous/ }));
    const launch = screen.getByRole("button", { name: /Launch run/ });
    expect(launch).toBeDisabled();
    expect(screen.getByText(/needs a task to perform/)).toBeInTheDocument();

    await user.type(screen.getByLabelText("Task"), "fix the flaky test");
    expect(launch).toBeEnabled();
  });
});
