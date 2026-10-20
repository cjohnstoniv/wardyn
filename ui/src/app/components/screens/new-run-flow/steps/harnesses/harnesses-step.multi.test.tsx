/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// F2MULTI-1 / F2MULTI-2: several agents on the Harnesses step, only where the server has the switch on.
import * as React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { setField } from "../../../../../../test/set-field";
import { NR_HARNESSES } from "../../../../wardyn/copy/new-run/harnesses";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { SCENARIO_HARNESSES } from "../../../../../lib/new-run-fixtures/scenarios/harnesses";
import { NewRunDraftProvider, useNewRunDispatch, useNewRunDraft } from "../../new-run-route";
import type { RunIntent } from "../../draft/run-intent";
import { HarnessesStep } from "./harnesses-step";
import type { Roster } from "./roster";

const [CLAUDE, CODEX] = SCENARIO_HARNESSES.full;
const GEMINI = { ...CODEX, id: "gemini-cli", display: "Gemini CLI" };
const ROSTER: Roster = { harnesses: [CLAUDE, CODEX, GEMINI], sets: [["claude-code", "codex-cli"]], multiAgent: true };
const harness = (id: string, task?: string) => ({ id, kind: "harness" as const, ...(task ? { task } : {}) });
const TWO: RunIntent = {
  shape: "agent_task",
  experience: "background",
  tools: [harness("claude-code"), harness("codex-cli", "Write the retry tests.")],
  workload: { kind: "agent_task", agent: "claude-code", task: "Fix the flaky login test." },
};
const ONE: RunIntent = { ...TWO, tools: [harness("claude-code")] };
const CONTINUE = NR_SHARED.CONTINUE_TO(NR_SHARED.STEP_FILES);

function Seeded({ intent, children }: { intent: RunIntent; children: React.ReactNode }) {
  const dispatch = useNewRunDispatch();
  const [ready, setReady] = React.useState(false);
  React.useEffect(() => {
    dispatch({ type: "edit", apply: () => intent });
    setReady(true);
  }, [dispatch, intent]);
  return ready ? children : null;
}
function Probe() {
  return <p data-testid="intent">{JSON.stringify(useNewRunDraft().intent)}</p>;
}
const intentNow = (): RunIntent => JSON.parse(screen.getByTestId("intent").textContent ?? "{}");

function show(intent: RunIntent, roster: Roster = ROSTER) {
  const go = vi.fn();
  render(
    <MemoryRouter>
      <NewRunDraftProvider owner="alice">
        <Seeded intent={intent}>
          <HarnessesStep location={{ screen: "step", step: "harnesses" }} go={go} startFromScratch={() => {}} deps={{ load: async () => roster }} />
          <Probe />
        </Seeded>
      </NewRunDraftProvider>
    </MemoryRouter>,
  );
  return go;
}
const taskBox = (n: number) => screen.getAllByRole("textbox", { name: new RegExp(NR_HARNESSES.TASK_LABEL) })[n];

afterEach(cleanup);

describe("Harnesses step: several agents", () => {
  it("switch on: Add another harness is live, each card has its own task, and the note shows from two agents", async () => {
    const user = userEvent.setup();
    show(ONE);
    expect(await screen.findByText(NR_HARNESSES.LEDE_AUTONOMOUS_MULTI)).toBeInTheDocument();
    expect(screen.queryByText(NR_SHARED.COMING_SOON)).not.toBeInTheDocument();
    expect(screen.queryByText(NR_HARNESSES.MULTI_NOTE)).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: NR_HARNESSES.ADD_ANOTHER }));
    await waitFor(() => expect(intentNow().tools?.map((t) => t.id)).toEqual(["claude-code", "codex-cli"]));
    expect(screen.getByText(NR_HARNESSES.MULTI_NOTE)).toBeInTheDocument();
    setField(taskBox(0), "Fix the flaky login test.");
    setField(taskBox(1), "Write the retry tests.");
    // The run's own agent keeps workload.task; every other harness carries its own.
    expect(intentNow().workload).toEqual({ kind: "agent_task", agent: "claude-code", task: "Fix the flaky login test." });
    expect(intentNow().tools).toEqual([harness("claude-code"), harness("codex-cli", "Write the retry tests.")]);
  });

  it("Continue with one task empty stops on that card with the per-card error", async () => {
    const user = userEvent.setup();
    const go = show({ ...TWO, tools: [harness("claude-code"), harness("codex-cli")] });
    await screen.findByText(NR_HARNESSES.MULTI_NOTE);
    await user.click(screen.getByRole("button", { name: CONTINUE }));
    expect(go).not.toHaveBeenCalled();
    expect(taskBox(1)).toHaveFocus();
    expect(await screen.findByText(NR_HARNESSES.AGENT_TASK_ERROR)).toBeInTheDocument();
    expect(screen.queryByText(NR_HARNESSES.TASK_ERROR)).not.toBeInTheDocument();
    setField(taskBox(1), "x");
    expect(screen.queryByText(NR_HARNESSES.AGENT_TASK_ERROR)).not.toBeInTheDocument();
  });

  it("Remove on the run's own agent hands the run's task to the next agent's", async () => {
    const user = userEvent.setup();
    show(TWO);
    await screen.findByText(NR_HARNESSES.MULTI_NOTE);
    await user.click(screen.getAllByRole("button", { name: NR_SHARED.REMOVE })[0]);
    expect(intentNow().tools).toEqual([harness("codex-cli")]);
    expect(intentNow().workload).toEqual({ kind: "agent_task", agent: "codex-cli", task: "Write the retry tests." });
  });

  it("a harness no image carries with the others is refused on the cards, and Continue holds on it", async () => {
    const user = userEvent.setup();
    const go = show(TWO);
    await screen.findByText(NR_HARNESSES.MULTI_NOTE);
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    await user.selectOptions(screen.getAllByRole("combobox", { name: NR_HARNESSES.HARNESS_LABEL })[1], "gemini-cli");
    expect(await screen.findByText(NR_HARNESSES.NO_COMBINATION("Claude Code", "Gemini CLI"))).toBeInTheDocument();
    expect(screen.getByText(NR_HARNESSES.NO_COMBINATION("Gemini CLI", "Claude Code"))).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: CONTINUE }));
    expect(go).not.toHaveBeenCalled();
    expect(screen.getAllByRole("combobox", { name: NR_HARNESSES.HARNESS_LABEL })[0]).toHaveFocus();
  });

  it("switch off: today's step, one agent, Add another harness Coming soon", async () => {
    show(ONE, { ...ROSTER, multiAgent: false });
    expect(await screen.findByText(NR_HARNESSES.LEDE_AUTONOMOUS)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: NR_HARNESSES.ADD_ANOTHER })).toBeDisabled();
    expect(screen.getByText(NR_SHARED.COMING_SOON)).toBeInTheDocument();
  });
});
