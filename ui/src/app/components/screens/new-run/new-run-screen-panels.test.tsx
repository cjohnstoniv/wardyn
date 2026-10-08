/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The four-panel New Run flow (#1922): the panel nav's semantics, where focus
// lands, the issue links, and the Run panel's own rules. Approved copy is
// compared byte for byte against the prototype's strings.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
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
const createRunMock = vi.fn();
vi.mock("../../../lib/api/policy-preview", () => ({ previewRunPolicy: vi.fn().mockResolvedValue({ spec: {}, pending: [], warnings: [], repository_access: [] }) }));
vi.mock("../../../lib/api/runs", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../../lib/api/runs")>();
  return {
    ...actual,
    runs: {
      createRun: (...a: unknown[]) => createRunMock(...a),
      listRuns: () => Promise.resolve([]),
      preflightRun: () => Promise.resolve({ enforced_confinement_class: "CC1", warnings: [] }),
      gradePolicy: () => Promise.resolve({ risk_assessment: [], overall_risk: "low" }),
    },
  };
});
const listWorkspacesMock = vi.fn();
vi.mock("../../../lib/api/workspaces", () => ({
  workspaces: { listWorkspaces: (...a: unknown[]) => listWorkspacesMock(...a) },
}));

import { NewRunScreen } from "./new-run-screen";
import { baseStatus } from "../../../lib/test-fixtures";
import { OperatorProvider } from "../../wardyn/operator-context";
import { DENIED } from "../../../lib/permissions-copy";
import { NEW_RUN_FLOW } from "../../wardyn/copy/new-run-flow";
import { setField } from "../../../../test/set-field";
import { goToPanel } from "../../../../test/new-run-panel";

const user = userEvent.setup({ pointerEventsCheck: 0 });

function renderScreen() {
  return render(
    <MemoryRouter>
      <OperatorProvider principal="test-owner" operator>
        <NewRunScreen />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

const nav = () => within(screen.getByRole("navigation", { name: "New run" }));
const heading = (name: string) => screen.getByRole("heading", { level: 2, name });
const launch = () => screen.getByRole("button", { name: "Launch run" });

beforeEach(() => {
  getSetupStatusMock.mockReset().mockResolvedValue(baseStatus());
  listWorkspacesMock.mockReset().mockResolvedValue([]);
  createRunMock.mockReset().mockResolvedValue({ id: "run_1" });
});

describe("New Run — the panel nav", () => {
  it("is a nav of four buttons in order, with aria-current on the one on screen", async () => {
    renderScreen();
    await screen.findByLabelText("Title");
    const buttons = nav().getAllByRole("button");
    expect(buttons.map((b) => b.textContent)).toEqual(["Run 1 issue", "Workspace", "Access", "Policy"]);
    expect(buttons.map((b) => b.getAttribute("aria-current"))).toEqual(["step", null, null, null]);
    // Steps taken in any order, not a tablist.
    expect(screen.queryByRole("tablist")).toBeNull();
    expect(screen.queryByRole("tab")).toBeNull();

    await user.click(nav().getByRole("button", { name: "Policy" }));
    expect(nav().getAllByRole("button").map((b) => b.getAttribute("aria-current"))).toEqual([null, null, null, "step"]);
  });

  it("shows one panel at a time and keeps the others' state", async () => {
    renderScreen();
    setField(await screen.findByLabelText("Title"), "Refund flow");
    expect(heading("Run")).toBeVisible();
    expect(screen.queryByRole("heading", { level: 2, name: "Workspace" })).toBeNull();

    goToPanel("Workspace");
    expect(heading("Workspace")).toBeVisible();
    expect(screen.queryByRole("heading", { level: 2, name: "Run" })).toBeNull();
    expect(screen.getByRole("combobox", { name: "Workspace" })).toBeVisible();

    goToPanel("Run");
    expect(screen.getByLabelText("Title")).toHaveValue("Refund flow");
  });

  it("counts a panel's issues in words", async () => {
    renderScreen();
    await screen.findByLabelText("Title");
    // A fresh interactive form holds Launch for the title alone.
    expect(nav().getByRole("button", { name: "Run 1 issue" })).toBeInTheDocument();
    // An autonomous run also needs its task: two issues, the task named first.
    await user.click(screen.getByRole("radio", { name: /^Autonomous/ }));
    expect(nav().getByRole("button", { name: "Run 2 issues" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "An autonomous run needs a task to perform." })).toBeInTheDocument();
    expect(screen.queryByText(NEW_RUN_FLOW.TITLE_REQUIRED)).toBeNull();
    // The task's first line fills the title: both are answered at once.
    setField(screen.getByLabelText("Task"), "Fix the flaky test");
    expect(nav().getByRole("button", { name: "Run" })).toBeInTheDocument();
    expect(launch()).toBeEnabled();
  });
});

describe("New Run — moving between panels", () => {
  it("opens with focus on Title", async () => {
    renderScreen();
    expect(await screen.findByLabelText("Title")).toHaveFocus();
  });

  it("Continue and Back are outline and ghost, focus the panel's heading, and Policy has Back only", async () => {
    renderScreen();
    await screen.findByLabelText("Title");
    const cont = screen.getByRole("button", { name: "Continue to Workspace" });
    expect(cont).toHaveClass("border");
    expect(cont).not.toHaveClass("bg-primary");
    expect(screen.queryByRole("button", { name: "Back" })).toBeNull();

    await user.click(cont);
    expect(heading("Workspace")).toHaveFocus();
    expect(heading("Workspace")).toHaveAttribute("tabindex", "-1");
    const back = screen.getByRole("button", { name: "Back" });
    expect(back).not.toHaveClass("bg-primary");
    expect(back).not.toHaveClass("border");

    await user.click(screen.getByRole("button", { name: "Continue to Access" }));
    expect(heading("Access")).toHaveFocus();
    await user.click(screen.getByRole("button", { name: "Continue to Policy" }));
    expect(heading("Policy")).toHaveFocus();
    expect(screen.queryByRole("button", { name: /^Continue to/ })).toBeNull();

    await user.click(screen.getByRole("button", { name: "Back" }));
    expect(heading("Access")).toHaveFocus();
  });

  it("the nav focuses the panel's heading too", async () => {
    renderScreen();
    await screen.findByLabelText("Title");
    await user.click(nav().getByRole("button", { name: "Access" }));
    expect(heading("Access")).toHaveFocus();
  });

  it("Launch is the one primary button, and it is on screen from every panel", async () => {
    renderScreen();
    await screen.findByLabelText("Title");
    for (const panel of ["Run", "Workspace", "Access", "Policy"] as const) {
      goToPanel(panel);
      const primary = screen.getAllByRole("button").filter((b) => b.className.split(" ").includes("bg-primary"));
      expect(primary).toEqual([launch()]);
    }
  });

  it("launches from any panel once nothing holds it", async () => {
    renderScreen();
    setField(await screen.findByLabelText("Title"), "Refund flow");
    goToPanel("Policy");
    await user.click(launch());
    await waitFor(() => expect(createRunMock).toHaveBeenCalledOnce());
  });
});

describe("New Run — issue links", () => {
  it("the line above Launch is a link that focuses the control it names", async () => {
    renderScreen();
    const title = await screen.findByLabelText("Title");
    goToPanel("Policy");
    await user.click(screen.getByRole("button", { name: NEW_RUN_FLOW.TITLE_REQUIRED }));
    expect(heading("Run")).toBeVisible();
    expect(title).toHaveFocus();
  });

  it("an empty required task is invalid and described by that line; a filled one is neither", async () => {
    renderScreen();
    await user.click(await screen.findByRole("radio", { name: /^Autonomous/ }));
    const task = screen.getByLabelText("Task");
    const line = screen.getByText("An autonomous run needs a task to perform.").closest("p")!;
    expect(task).toHaveAttribute("aria-invalid", "true");
    expect(task.getAttribute("aria-describedby")?.split(" ")).toContain(line.id);

    goToPanel("Workspace");
    await user.click(screen.getByRole("button", { name: "An autonomous run needs a task to perform." }));
    expect(task).toHaveFocus();

    setField(task, "Fix the flaky test");
    expect(task).not.toHaveAttribute("aria-invalid");
  });

  it("a workspace that is not available is an error beside its Select, and a link from every other panel — never both", async () => {
    listWorkspacesMock.mockResolvedValue([
      { id: "ws1", name: "trading-desk", kind: "local_dir", source: "/data/t", status: "scanned", available_to_you: false },
    ]);
    renderScreen();
    setField(await screen.findByLabelText("Title"), "Refund flow");
    goToPanel("Workspace");
    const select = screen.getByRole("combobox", { name: "Workspace" });
    await user.click(select);
    await user.click(await screen.findByRole("option", { name: /trading-desk/ }));

    // On its own panel: beside the control, in error tone with an icon.
    const note = screen.getByText(DENIED.WORKSPACE_NOT_AVAILABLE);
    expect(note).toHaveClass("text-danger");
    expect(note.querySelector("svg")).not.toBeNull();
    expect(select).toHaveAttribute("aria-invalid", "true");
    expect(select.getAttribute("aria-describedby")).toBe(note.id);
    expect(screen.queryByRole("button", { name: DENIED.WORKSPACE_NOT_AVAILABLE })).toBeNull();
    expect(nav().getByRole("button", { name: "Workspace 1 issue" })).toBeInTheDocument();
    expect(launch()).toBeDisabled();

    // From another panel: above Launch, once, and it leads back to the Select.
    goToPanel("Run");
    expect(screen.getAllByText(DENIED.WORKSPACE_NOT_AVAILABLE)).toHaveLength(1);
    await user.click(screen.getByRole("button", { name: DENIED.WORKSPACE_NOT_AVAILABLE }));
    expect(heading("Workspace")).toBeVisible();
    expect(select).toHaveFocus();
  });
});

describe("New Run — the Run panel", () => {
  it("puts Run details first, always on screen, with the approved sentence", async () => {
    renderScreen();
    const title = await screen.findByLabelText("Title");
    const details = screen.getByRole("group", { name: "Run details" });
    expect(details).toContainElement(title);
    expect(details).toContainElement(screen.getByLabelText("Description"));
    expect(details).toHaveTextContent(
      "For people, not the agent. A title and description help you find and understand this run later. They are never sent into the sandbox.",
    );
    expect(title).toBeRequired();
    expect(title).toHaveAttribute("maxlength", "200");
    expect(title).toHaveAttribute("placeholder", "Refactor the payments module");
    expect(title).toHaveAttribute("list", "nr-known-titles");
    expect(screen.getByLabelText("Description")).not.toBeRequired();
    // Before the run type, in reading order.
    const runType = screen.getByRole("radiogroup", { name: "Run type" });
    expect(details.compareDocumentPosition(runType) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("Command is a single-line command input behind a decorative prompt", async () => {
    renderScreen();
    await user.click(await screen.findByRole("radio", { name: "Shell command" }));
    const command = screen.getByLabelText("Command");
    expect(command.tagName).toBe("INPUT");
    expect(command).toHaveAttribute("id", "nr-task");
    expect(command).toHaveAttribute("placeholder", "make test");
    expect(command).toHaveClass("font-mono");
    expect(command).toBeRequired();
    const prompt = command.parentElement!.querySelector("span")!;
    expect(prompt).toHaveTextContent("$");
    expect(prompt).toHaveAttribute("aria-hidden", "true");
    // The field's name is the label alone.
    expect(screen.getByRole("textbox", { name: "Command" })).toBe(command);
  });

  it("Startup command is the same single-line input", async () => {
    renderScreen();
    await user.click(await screen.findByRole("radio", { name: /^Terminal/ }));
    const startup = screen.getByLabelText("Startup command (optional)");
    expect(startup.tagName).toBe("INPUT");
    expect(startup).toHaveAttribute("id", "nr-seed");
    expect(startup).toHaveClass("font-mono");
  });

  it("Task, Command and Startup command each keep their own value", async () => {
    renderScreen();
    await user.click(await screen.findByRole("radio", { name: /^Autonomous/ }));
    setField(screen.getByLabelText("Task"), "Fix the flaky refund test");

    await user.click(screen.getByRole("radio", { name: "Shell command" }));
    expect(screen.getByLabelText("Command")).toHaveValue("");
    setField(screen.getByLabelText("Command"), "make test");

    await user.click(screen.getByRole("radio", { name: "Agent task" }));
    expect(screen.getByLabelText("Task")).toHaveValue("Fix the flaky refund test");

    await user.click(screen.getByRole("radio", { name: /^Interactive/ }));
    // Initial prompt shares the Task's text, as it always has.
    expect(screen.getByLabelText("Initial prompt (optional)")).toHaveValue("Fix the flaky refund test");
    await user.click(screen.getByRole("radio", { name: /^Terminal/ }));
    expect(screen.getByLabelText("Startup command (optional)")).toHaveValue("");
    setField(screen.getByLabelText("Startup command (optional)"), "npm run dev");

    await user.click(screen.getByRole("radio", { name: "Shell command" }));
    expect(screen.getByLabelText("Command")).toHaveValue("make test");
  });

  it("launches the shell command that was typed as a Command, never the Task", async () => {
    renderScreen();
    await user.click(await screen.findByRole("radio", { name: /^Autonomous/ }));
    setField(screen.getByLabelText("Task"), "Fix the flaky refund test");
    await user.click(screen.getByRole("radio", { name: "Shell command" }));
    // The Task is not a command: Launch waits for one.
    expect(launch()).toBeDisabled();
    setField(screen.getByLabelText("Command"), "make test");
    await user.click(launch());
    await waitFor(() => expect(createRunMock).toHaveBeenCalledOnce());
    expect((createRunMock.mock.calls[0][0] as { task: string }).task).toBe("make test");
  });

  it("prefills the title from the Command's first line, and leaves a typed title alone", async () => {
    renderScreen();
    await user.click(await screen.findByRole("radio", { name: "Shell command" }));
    setField(screen.getByLabelText("Command"), "make test");
    expect(screen.getByLabelText("Title")).toHaveValue("make test");

    setField(screen.getByLabelText("Title"), "Nightly tests");
    setField(screen.getByLabelText("Command"), "make lint");
    expect(screen.getByLabelText("Title")).toHaveValue("Nightly tests");
  });

  it("a terminal start has nothing to derive a title from, so Launch waits for one", async () => {
    renderScreen();
    await user.click(await screen.findByRole("radio", { name: /^Terminal/ }));
    setField(screen.getByLabelText("Startup command (optional)"), "npm run dev");
    expect(screen.getByLabelText("Title")).toHaveValue("");
    expect(launch()).toBeDisabled();
    expect(screen.getByRole("button", { name: NEW_RUN_FLOW.TITLE_REQUIRED })).toBeInTheDocument();
  });

  it("Enter never launches: not from Title, Command, Startup command or the panel nav", async () => {
    renderScreen();
    setField(await screen.findByLabelText("Title"), "Refund flow");
    expect(launch()).toBeEnabled();

    // A real Enter key press on the focused field: keydown, keypress, keyup,
    // and the implicit submission a form would get from it.
    const enter = async (field: HTMLElement) => {
      field.focus();
      await user.keyboard("{Enter}");
    };
    await enter(screen.getByLabelText("Title"));
    await user.click(screen.getByRole("radio", { name: /^Terminal/ }));
    setField(screen.getByLabelText("Startup command (optional)"), "npm run dev");
    await enter(screen.getByLabelText("Startup command (optional)"));
    expect(screen.getByLabelText("Startup command (optional)")).toHaveValue("npm run dev");
    await user.click(screen.getByRole("radio", { name: "Shell command" }));
    setField(screen.getByLabelText("Command"), "make test");
    await enter(screen.getByLabelText("Command"));
    expect(screen.getByLabelText("Command")).toHaveValue("make test");
    await enter(nav().getByRole("button", { name: "Workspace" }));
    expect(heading("Workspace")).toBeVisible();

    expect(createRunMock).not.toHaveBeenCalled();
  });
});

describe("New Run — Tool approvals under Hold", () => {
  const HOLD = "Hold in Wardyn — tool calls wait for approval, by tool rule";

  it("names Hold in the approved words, and shows the tool rules line and link only under Hold", async () => {
    renderScreen();
    await user.click(await screen.findByRole("radio", { name: /^Autonomous/ }));
    expect(screen.queryByRole("button", { name: "Tool rules" })).toBeNull();

    await user.click(screen.getByRole("radio", { name: HOLD }));
    expect(screen.getByRole("button", { name: "Tool rules" })).toBeInTheDocument();
  });

  it("the Tool rules link shows Policy and focuses the tool rules section", async () => {
    renderScreen();
    await user.click(await screen.findByRole("radio", { name: /^Autonomous/ }));
    await user.click(screen.getByRole("radio", { name: HOLD }));
    await user.click(screen.getByRole("button", { name: "Tool rules" }));

    expect(heading("Policy")).toBeVisible();
    const section = document.getElementById("policy-tool-rules-run")!;
    expect(section).toContainElement(document.activeElement as HTMLElement);
  });

  it("the line under Hold is the rail's own tool-rules sentence", async () => {
    renderScreen();
    await user.click(await screen.findByRole("radio", { name: /^Autonomous/ }));
    await user.click(screen.getByRole("radio", { name: HOLD }));
    fireEvent.change(screen.getByLabelText("Spec (JSON)"), {
      target: { value: JSON.stringify({ min_confinement_class: "CC1", tool_rules: [{ tool: "Bash", effect: "hold" }] }) },
    });
    const sentence = "1 rule · Bash held. Anything else is held.";
    // Once under Hold on the Run panel, once in the rail's Tool rules section.
    await waitFor(() => expect(screen.getAllByText(sentence)).toHaveLength(2));
    expect(within(screen.getByRole("complementary")).getByText(sentence)).toBeInTheDocument();
  });

  it("Codex CLI keeps Hold disabled and shows nothing extra", async () => {
    renderScreen();
    await user.click(await screen.findByRole("radio", { name: /^Autonomous/ }));
    await user.click(screen.getByRole("combobox", { name: "Agent" }));
    await user.click(await screen.findByRole("option", { name: /Codex/ }));
    expect(screen.getByRole("radio", { name: HOLD })).toBeDisabled();
    expect(screen.queryByRole("button", { name: "Tool rules" })).toBeNull();
  });
});

describe("New Run — the rail below lg", () => {
  it("has one rail, one Launch, and a toggle that says whether the summary is open", async () => {
    renderScreen();
    await screen.findByLabelText("Title");
    expect(screen.getAllByRole("complementary")).toHaveLength(1);
    expect(screen.getAllByRole("button", { name: "Launch run" })).toHaveLength(1);

    const toggle = screen.getByRole("button", { name: "What this run can do" });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    const sections = document.getElementById(toggle.getAttribute("aria-controls")!)!;
    expect(sections).toHaveClass("hidden");

    await user.click(toggle);
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    expect(toggle).toHaveFocus();
    expect(sections).not.toHaveClass("hidden");
    // The summary precedes the decision block and Launch in reading order.
    expect(sections.compareDocumentPosition(launch()) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("the verdict, the check state and the reason Launch is held sit directly above Launch", async () => {
    renderScreen();
    await screen.findByLabelText("Title");
    const rail = screen.getByRole("complementary");
    const line = within(rail).getByText(NEW_RUN_FLOW.TITLE_REQUIRED).closest("p")!;
    // Nothing but the Launch row follows the reason.
    expect(line.nextElementSibling).toContainElement(launch());
  });
});
