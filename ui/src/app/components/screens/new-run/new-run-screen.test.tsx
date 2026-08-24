/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The rail's honesty rules, pinned where the SetupStatus is controlled. e2e
// cannot assert these: whether a model provider exists there depends on whether
// the machine running the suite has a logged-in Claude CLI, which setupProviders
// detects — so the same assertion passes on a laptop and fails on CI.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
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
const preflightRunMock = vi.fn();
vi.mock("../../../lib/api/runs", () => ({
  runs: {
    createRun: vi.fn(),
    listRuns: () => Promise.resolve([]),
    preflightRun: (...a: unknown[]) => preflightRunMock(...a),
    // The PolicyPanel's SafetyMeter debounces a grade of the current spec; stub
    // it so the panel's meter has a resolvable call instead of hitting the net.
    gradePolicy: () => Promise.resolve({ risk_assessment: [], overall_risk: "low" }),
  },
}));
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
  preflightRunMock.mockReset();
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

// The Policy panel authors the spec; the run's own SELECTIONS are unioned in
// after the parse and named on screen. The governance claim the retired
// host-count tests made — count the UNION, not the sum — now lives here: what
// the JSON already carries must never be re-announced as something this screen
// added, and what only the selections know must never be silently merged.
describe("NewRunScreen — the additions line counts the union, not the sum", () => {
  // A grant whose injection host is already on the allowlist adds NOTHING. The
  // Minimal template is exactly that shape (api.anthropic.com, allowed), so a
  // fresh screen has nothing to announce.
  it("says nothing when the JSON already carries every implied host", async () => {
    renderScreen();
    await screen.findByLabelText(/Spec \(JSON\)/);
    expect(screen.queryByTestId("run-spec-additions")).not.toBeInTheDocument();
  });

  // The trap the merge rules exist for: proxy credential injection only rewrites
  // requests whose host is ALREADY on allowed_domains, and that holds under
  // allow_all_egress too. A hand-written api_key grant therefore needs its exact
  // host pinned — derived from the merged grant UNION, not from wizard state,
  // which knows nothing about a grant the operator typed.
  it("pins a hand-written api_key grant's host, allow-all included", async () => {
    renderScreen();
    const box = await screen.findByLabelText(/Spec \(JSON\)/);
    await user.clear(box);
    fireEvent.change(box, {
      target: {
        value: JSON.stringify({
          allowed_domains: [],
          allow_all_egress: true,
          first_use_approval: "always_deny",
          min_confinement_class: "CC1",
          eligible_grants: [
            {
              kind: "api_key",
              scope: { host: "llm.acme.internal", header: "x-api-key", secret_name: "acme-key" },
              requires_approval: false,
            },
          ],
        }),
      },
    });

    const added = await screen.findByTestId("run-spec-additions");
    expect(within(added).getByText("llm.acme.internal")).toBeInTheDocument();
  });
});

// Every successful parse re-reads the floor the document authors, and the Seg
// DISABLES every tier below it. A one-time up-clamp alone would re-open the
// below-floor 422 the moment the operator lowered the Seg afterwards.
describe("NewRunScreen — the barrier floor disables what it forbids", () => {
  it("names the floor as its own reason, separate from what the host can build", async () => {
    renderScreen();
    const box = await screen.findByLabelText(/Spec \(JSON\)/);
    fireEvent.change(box, {
      target: {
        value: JSON.stringify({
          allowed_domains: [],
          first_use_approval: "always_deny",
          min_confinement_class: "CC3",
        }),
      },
    });

    // The health mock reports CC1 only, so Wall/Vault are unavailable AND
    // below-floor — one reason each, never two — while Fence, which this host
    // builds fine, is disabled for the floor alone. Every tier disabled is
    // fail-closed on purpose; preflight and launch name the cause.
    expect(await screen.findByText(/Fence is below the policy's floor \(Vault\)/)).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: "Fence" })).toBeDisabled();
    expect(screen.getByText(/Wall isn't installed on this host/)).toBeInTheDocument();
    expect(screen.queryByText(/Wall is below the policy's floor/)).not.toBeInTheDocument();
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

// Phase 3: preflightRun wired into a real caller. Same request payload as
// Launch (buildRunInput), rendered inline next to the actions instead of
// blocking them.
describe("NewRunScreen — Preflight", () => {
  async function readyScreen() {
    renderScreen();
    await user.type(await screen.findByLabelText("Title"), "Refund flow");
    return screen.getByRole("button", { name: /^Preflight$/ });
  }

  it("renders the warnings, risk grade, and enforced confinement class on success", async () => {
    preflightRunMock.mockResolvedValue({
      setup_items: [],
      enforced_confinement_class: "CC2",
      overall_risk: "medium",
      warnings: ["Egress narrowed to api.anthropic.com by member policy."],
    });
    const button = await readyScreen();
    await user.click(button);

    const result = await screen.findByTestId("preflight-result");
    expect(within(result).getByText("Egress narrowed to api.anthropic.com by member policy.")).toBeInTheDocument();
    expect(within(result).getByText("Medium")).toBeInTheDocument();
    expect(within(result).getByText("Wall")).toBeInTheDocument();
  });

  it("collapses to a quiet line when there are no warnings", async () => {
    preflightRunMock.mockResolvedValue({
      setup_items: [],
      enforced_confinement_class: "CC1",
      overall_risk: "low",
      warnings: [],
    });
    const button = await readyScreen();
    await user.click(button);

    const result = await screen.findByTestId("preflight-result");
    expect(within(result).getByText("No adjustments.")).toBeInTheDocument();
    expect(within(result).getByText("Low")).toBeInTheDocument();
    expect(within(result).getByText("Fence")).toBeInTheDocument();
  });

  it("renders the server's field-path error verbatim on a 4xx", async () => {
    preflightRunMock.mockRejectedValue(new Error('workspaces[0]: unknown secret "prod-db"'));
    const button = await readyScreen();
    await user.click(button);

    expect(await screen.findByText('workspaces[0]: unknown secret "prod-db"')).toBeInTheDocument();
  });

  it("disables the button and shows a loading spinner while in flight", async () => {
    let resolve: (v: unknown) => void = () => {};
    preflightRunMock.mockReturnValue(
      new Promise((r) => {
        resolve = r;
      }),
    );
    const button = await readyScreen();
    await user.click(button);

    expect(button).toBeDisabled();
    resolve({ setup_items: [], enforced_confinement_class: "CC1", warnings: [] });
    await waitFor(() => expect(button).toBeEnabled());
  });
});
