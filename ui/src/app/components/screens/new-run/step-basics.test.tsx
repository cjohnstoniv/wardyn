/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { StepBasics } from "./step-basics";
import { initialWizardState } from "./wizard-types";
import type { Workspace } from "../../../lib/types";

// Basics must (1) offer "Agent run" vs "Governed command" as a first-class
// choice defaulting to "Agent run" (task_mode omitted => harness), (2) hide
// the Agent picker for a governed command (it's irrelevant — no model runs),
// and (3) surface the bring-your-own-image field directly, not tucked behind
// a collapsed <details> the operator has to know to open.
describe("StepBasics — run type + sandbox image", () => {
  function renderStep(overrides?: Partial<Parameters<typeof StepBasics>[0]>) {
    return render(
      <StepBasics
        state={initialWizardState()}
        patch={() => {}}
        workspaces={[] as Workspace[]}
        workspacesLoading={false}
        profileLoading={false}
        onSelectProfile={() => {}}
        onClearProfile={() => {}}
        onAddWorkspace={() => {}}
        {...overrides}
      />,
    );
  }

  it("defaults to Agent run, with the Agent picker shown", () => {
    renderStep();
    const runType = screen.getByTestId("basics-run-type");
    const agentRadio = within(runType).getByRole("radio", { name: /agent run/i });
    expect(agentRadio).toHaveAttribute("data-state", "checked");
    expect(screen.getByText("Agent")).toBeInTheDocument();
  });

  it("picking Governed command patches runType", async () => {
    let patched: Partial<ReturnType<typeof initialWizardState>> | null = null;
    renderStep({ patch: (p) => { patched = p; } });
    const user = userEvent.setup();

    await user.click(screen.getByRole("radio", { name: /governed command/i }));
    expect(patched).toEqual({ runType: "command" });
  });

  it("hides the Agent picker for a governed command", () => {
    renderStep({ state: { ...initialWizardState(), runType: "command" } });
    expect(screen.queryByText("Agent")).toBeNull();
  });

  it("shows the sandbox image field directly, with no collapsed <details>", () => {
    const { container } = renderStep();
    expect(screen.getByLabelText("Sandbox image")).toBeInTheDocument();
    expect(container.querySelector("details")).toBeNull();
  });

  it("relabels the task field as Command for a governed command", () => {
    renderStep({ state: { ...initialWizardState(), runType: "command", mode: "batch" } });
    expect(screen.getByText("Command")).toBeInTheDocument();
    expect(screen.queryByText("Task", { exact: true })).toBeNull();
  });
});
