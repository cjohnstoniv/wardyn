/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { setField } from "../../../../../../test/set-field";
import { MemoryRouter } from "react-router-dom";
import { NR_HARNESSES } from "../../../../wardyn/copy/new-run/harnesses";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { SCENARIO_HARNESSES, SCENARIO_HARNESS_SETS } from "../../../../../lib/new-run-fixtures/scenarios/harnesses";
import { NewRunDraftProvider, useNewRunDispatch, useNewRunDraft } from "../../new-run-route";
import type { NewRunOrigin, RunIntent } from "../../draft/run-intent";
import { HarnessesStep } from "./harnesses-step";
import type { Roster } from "./roster";

const ROSTER: Roster = { harnesses: SCENARIO_HARNESSES.full, sets: SCENARIO_HARNESS_SETS.full };
const CODEX_ROW = ROSTER.harnesses[1];
const AUTONOMOUS: RunIntent = { shape: "agent_task", experience: "background", workload: { kind: "agent_task", task: "" } };
const INTERACTIVE: RunIntent = { shape: "agent_task", experience: "interactive" };
const harness = (id: string) => ({ id, kind: "harness" as const });

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
const optionNames = (select: HTMLElement) => within(select).getAllByRole("option").map((o) => o.textContent);
const rowOf = (index: number) => ({ ...ROSTER.harnesses[index] });

afterEach(cleanup);

describe("Harnesses step: roster rows", () => {
  it("lists the server's roster, including Your own tools enabled on a stock install, and no Custom harness", async () => {
    show({ ...INTERACTIVE, tools: [harness("claude-code")] });
    const select = await screen.findByRole("combobox", { name: NR_HARNESSES.HARNESS_LABEL });
    await waitFor(() => expect(optionNames(select)).toEqual(["Claude Code", "Codex CLI", "Your own tools"]));
    expect(screen.queryByText(/custom harness/i)).not.toBeInTheDocument();
    expect(screen.getByRole("option", { name: "Your own tools" })).toBeEnabled();
    expect(screen.queryByText(NR_HARNESSES.NOT_INSTALLED("Your own tools"))).not.toBeInTheDocument();
  });

  it("keeps a row the server cannot offer visible, disabled, with its reason as text", async () => {
    const roster: Roster = {
      sets: [],
      harnesses: [
        { ...rowOf(0) },
        { ...rowOf(1), id: "off", display: "Off one", enabled: false },
        { ...rowOf(1), id: "bare", display: "Bare one", installed: false },
        { ...rowOf(1), id: "nogrant", display: "Ungranted one", providers_ungranted: true },
      ],
    };
    show({ ...INTERACTIVE, tools: [harness("claude-code")] }, roster);
    await screen.findByText("Off one", { selector: "strong" });
    const list = document.getElementById("nr-harness-unavailable") as HTMLElement;
    expect(within(list).getByText(NR_HARNESSES.NOT_ENABLED)).toBeInTheDocument();
    expect(within(list).getByText(NR_HARNESSES.NOT_INSTALLED("Bare one"))).toBeInTheDocument();
    expect(within(list).getByText(NR_HARNESSES.NO_PROVIDER("Ungranted one"))).toBeInTheDocument();
    for (const name of ["Off one", "Bare one", "Ungranted one"]) expect(screen.getByRole("option", { name })).toBeDisabled();
  });

  it("disables a row whose combination no image carries, and says so", async () => {
    show({ ...INTERACTIVE, tools: [harness("claude-code")] }, { ...ROSTER, sets: [] });
    expect(await screen.findByText(NR_HARNESSES.NO_COMBINATION("Claude Code", "Codex CLI"))).toBeInTheDocument();
    expect(screen.getByRole("button", { name: NR_HARNESSES.ADD_ANOTHER })).toBeDisabled();
  });

  it("offers a combination a larger declared set carries (DR-0225)", async () => {
    const user = userEvent.setup();
    const triple = [ROSTER.harnesses[0].id, ROSTER.harnesses[1].id, ROSTER.harnesses[2].id];
    show({ ...INTERACTIVE, tools: [harness("claude-code")] }, { ...ROSTER, sets: [triple] });
    await user.click(await screen.findByRole("button", { name: NR_HARNESSES.ADD_ANOTHER }));
    expect(intentNow().tools?.map((t) => t.id)).toEqual(["claude-code", "codex-cli"]);
    expect(screen.queryByText(NR_HARNESSES.NO_COMBINATION("Claude Code", "Codex CLI"))).not.toBeInTheDocument();
  });

  it("adds the first addable row, then the next", async () => {
    const user = userEvent.setup();
    show({ ...INTERACTIVE, tools: [harness("claude-code")] });
    await user.click(await screen.findByRole("button", { name: NR_HARNESSES.ADD_ANOTHER }));
    expect(intentNow().tools?.map((t) => t.id)).toEqual(["claude-code", "codex-cli"]);
    // Your own tools is in no set with both, so the button now says why it cannot add.
    expect(screen.getByRole("button", { name: NR_HARNESSES.ADD_ANOTHER })).toBeDisabled();
    expect(screen.getByText(NR_HARNESSES.NO_COMBINATION("Claude Code, Codex CLI", "Your own tools"))).toBeInTheDocument();
  });

  it("moves focus to the new harness's select when the Add button disables", async () => {
    const user = userEvent.setup();
    show({ ...INTERACTIVE, tools: [harness("claude-code")] });
    await user.click(await screen.findByRole("button", { name: NR_HARNESSES.ADD_ANOTHER }));
    await waitFor(() => expect(document.getElementById("nr-harness-1")).toHaveFocus());
  });

  it("hints from the row's facts: approval hold, Codex, a roster agent without a hold, availability", async () => {
    show({ ...INTERACTIVE, tools: [harness("claude-code"), harness("codex-cli"), harness("none")] });
    expect(await screen.findByText(NR_HARNESSES.AVAILABLE("Claude Code"))).toBeInTheDocument();
    expect(screen.getByText(NR_HARNESSES.CODEX_HINT)).toBeInTheDocument();
    expect(screen.getByText(NR_HARNESSES.CUSTOM_HINT("Your own tools"))).toBeInTheDocument();
  });

  it("shows why an already chosen harness cannot run", async () => {
    show({ ...INTERACTIVE, tools: [harness("codex-cli")] }, { ...ROSTER, harnesses: [{ ...CODEX_ROW, installed: false }] });
    expect((await screen.findAllByText(NR_HARNESSES.NOT_INSTALLED("Codex CLI"))).length).toBeGreaterThan(0);
  });
});

describe("Harnesses step: Interactive", () => {
  it("starts in at most one harness and gives only that one a starting prompt", async () => {
    const user = userEvent.setup();
    show({ ...INTERACTIVE, tools: [harness("claude-code"), harness("codex-cli")] });
    const [first, second] = await screen.findAllByRole("switch", { name: NR_HARNESSES.START_IN_HARNESS });
    expect(screen.queryByLabelText(new RegExp(NR_HARNESSES.STARTING_PROMPT))).not.toBeInTheDocument();
    await user.click(first);
    setField(await screen.findByLabelText(new RegExp(NR_HARNESSES.STARTING_PROMPT)), "look around");
    expect(intentNow().startup).toEqual({ kind: "harness", tool: "claude-code", prompt: "look around" });
    await user.click(second);
    expect(first).toHaveAttribute("aria-checked", "false");
    expect(intentNow().startup).toEqual({ kind: "harness", tool: "codex-cli" });
    expect(screen.getAllByLabelText(new RegExp(NR_HARNESSES.STARTING_PROMPT))).toHaveLength(1);
    await user.click(second);
    expect(intentNow().startup).toBeUndefined();
  });

  it("gives Your own tools no starting prompt box, since it starts no CLI", async () => {
    show({ ...INTERACTIVE, tools: [harness("none")], startup: { kind: "harness", tool: "none" } });
    expect(await screen.findByRole("switch", { name: NR_HARNESSES.START_IN_HARNESS })).toHaveAttribute("aria-checked", "true");
    expect(screen.queryByLabelText(new RegExp(NR_HARNESSES.STARTING_PROMPT))).not.toBeInTheDocument();
  });

  it("removes the started harness with its start, announcing nothing it did not do", async () => {
    const user = userEvent.setup();
    show({ ...INTERACTIVE, tools: [harness("claude-code"), harness("codex-cli")], startup: { kind: "harness", tool: "claude-code", prompt: "p" } });
    const removes = await screen.findAllByRole("button", { name: NR_SHARED.REMOVE });
    await user.click(removes[0]);
    expect(intentNow().tools?.map((t) => t.id)).toEqual(["codex-cli"]);
    expect(intentNow().startup).toBeUndefined();
    expect(screen.getByRole("button", { name: NR_SHARED.REMOVE })).toBeDisabled();
  });
});

describe("Harnesses step: Autonomous", () => {
  it("holds one harness and one task, with Add another harness Coming soon", async () => {
    show({ ...AUTONOMOUS, tools: [harness("claude-code")], workload: { kind: "agent_task", agent: "claude-code", task: "" } });
    expect(await screen.findByText(NR_HARNESSES.LEDE_AUTONOMOUS)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: NR_HARNESSES.ADD_ANOTHER })).toBeDisabled();
    expect(screen.getByText(NR_SHARED.COMING_SOON)).toBeInTheDocument();
    expect(screen.queryByRole("switch")).not.toBeInTheDocument();
    setField(screen.getByRole("textbox", { name: new RegExp(NR_HARNESSES.TASK_LABEL) }), "ship it");
    expect(intentNow().workload).toEqual({ kind: "agent_task", agent: "claude-code", task: "ship it" });
    expect(intentNow().startup).toBeUndefined();
  });

  it("a new Autonomous task gets the roster's first harness as Make's Harness 1, and the select swaps it", async () => {
    const user = userEvent.setup();
    show(AUTONOMOUS);
    await waitFor(() => expect(intentNow().workload?.agent).toBe("claude-code"));
    expect(intentNow().startup).toBeUndefined();
    await user.selectOptions(screen.getByRole("combobox", { name: NR_HARNESSES.HARNESS_LABEL }), "codex-cli");
    expect(intentNow().tools?.map((t) => t.id)).toEqual(["codex-cli"]);
    expect(intentNow().workload?.agent).toBe("codex-cli");
  });

  it("Continue without a task shows the canon error on the task field", async () => {
    const user = userEvent.setup();
    const go = show({ ...AUTONOMOUS, tools: [harness("claude-code")], workload: { kind: "agent_task", agent: "claude-code", task: "" } });
    const task = await screen.findByRole("textbox", { name: new RegExp(NR_HARNESSES.TASK_LABEL) });
    expect(screen.queryByText(NR_HARNESSES.TASK_ERROR)).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: NR_SHARED.CONTINUE_TO(NR_SHARED.STEP_FILES) }));
    expect(go).not.toHaveBeenCalled();
    expect(task).toHaveFocus();
    expect(await screen.findByText(NR_HARNESSES.TASK_ERROR)).toBeInTheDocument();
    setField(task, "x");
    expect(screen.queryByText(NR_HARNESSES.TASK_ERROR)).not.toBeInTheDocument();
  });
});

describe("Harnesses step: Make's Harness 1", () => {
  it("a new Interactive agent task gets the roster's first harness, started, as in Make", async () => {
    show(INTERACTIVE);
    expect(await screen.findByRole("switch", { name: NR_HARNESSES.START_IN_HARNESS })).toBeChecked();
    expect(intentNow().tools?.map((t) => t.id)).toEqual(["claude-code"]);
    expect(intentNow().startup).toEqual({ kind: "harness", tool: "claude-code" });
  });
});

describe("Harnesses step: Environment and Application", () => {
  it.each([
    ["environment", { shape: "environment", experience: "interactive" } as RunIntent],
    ["application", { shape: "application", experience: "background", workload: { kind: "command", command: "make" } } as RunIntent],
  ])("an %s takes optional harnesses and starts none", async (_shape, base) => {
    const user = userEvent.setup();
    const go = show(base);
    expect(await screen.findByText(NR_HARNESSES.LEDE_OPTIONAL)).toBeInTheDocument();
    expect(screen.getByText(NR_HARNESSES.EMPTY_TITLE)).toBeInTheDocument();
    expect(screen.getByText(NR_HARNESSES.EMPTY_BODY)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: NR_HARNESSES.ADD_HARNESS }));
    expect(screen.getByText(NR_HARNESSES.OPTIONAL_BEHAVIOUR)).toBeInTheDocument();
    expect(screen.queryByRole("switch")).not.toBeInTheDocument();
    expect(intentNow().tools?.map((t) => t.id)).toEqual(["claude-code"]);
    expect(intentNow().startup).toBeUndefined();
    await user.click(screen.getByRole("button", { name: NR_SHARED.REMOVE }));
    expect(intentNow().tools).toEqual([]);
    await user.click(screen.getByRole("button", { name: NR_SHARED.CONTINUE_TO(NR_SHARED.STEP_FILES) }));
    expect(go).toHaveBeenCalledWith({ screen: "step", step: "files" });
  });

  it.each<[string, NewRunOrigin, string[] | undefined]>([
    ["scratch", { kind: "scratch" }, ["claude-code"]],
    ["template", { kind: "template", id: "t1", revision: 1, name: "T", templateKind: "template" }, undefined],
    ["again", { kind: "again", runId: "r1", title: "R" }, undefined],
    ["import", { kind: "import" }, undefined],
    ["composer", { kind: "composer" }, undefined],
  ])("gives an Agent task with no harness its first harness only from scratch (%s)", async (_name, origin, tools) => {
    const load = vi.fn(async () => ROSTER);
    function SeededFrom({ children }: { children: React.ReactNode }) {
      const dispatch = useNewRunDispatch();
      const [ready, setReady] = React.useState(false);
      React.useEffect(() => {
        dispatch({ type: "seed", origin, intent: INTERACTIVE });
        setReady(true);
      }, [dispatch]);
      return ready ? children : null;
    }
    render(
      <MemoryRouter>
        <NewRunDraftProvider owner="alice">
          <SeededFrom>
            <HarnessesStep location={{ screen: "step", step: "harnesses" }} go={vi.fn()} startFromScratch={() => {}} deps={{ load }} />
            <Probe />
          </SeededFrom>
        </NewRunDraftProvider>
      </MemoryRouter>,
    );
    // The roster has answered and every effect it set off has run before the draft is read.
    await waitFor(() => expect(load).toHaveBeenCalled());
    await act(() => new Promise((r) => setTimeout(r, 20)));
    expect(intentNow().tools?.map((t) => t.id)).toEqual(tools);
  });
});
