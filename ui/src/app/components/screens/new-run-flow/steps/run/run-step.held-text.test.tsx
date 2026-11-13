/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Run step and the agent text held beside the draft: what a switch to a VM holds, and when the fact is replaced.

import * as React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { SCENARIO_POOLS } from "../../../../../lib/new-run-fixtures/scenarios/pools";
import { NR_MACHINE } from "../../../../wardyn/copy/new-run/machine";
import { NR_RUN } from "../../../../wardyn/copy/new-run/run";
import { ModelAccessProvider } from "../../../../wardyn/model-access-context";
import type { NewRunOrigin, RunIntent } from "../../draft/run-intent";
import { NewRunDraftProvider, useNewRunDispatch, useNewRunDraft } from "../../new-run-route";
import { RunStep } from "./run-step";

afterEach(cleanup);

function Seeded({ intent, origin, children }: { intent: RunIntent; origin?: NewRunOrigin; children: React.ReactNode }) {
  const dispatch = useNewRunDispatch();
  const [ready, setReady] = React.useState(false);
  React.useEffect(() => {
    dispatch(origin ? { type: "seed", origin, intent } : { type: "edit", apply: () => intent });
    setReady(true);
  }, [dispatch, intent, origin]);
  return ready ? children : null;
}
const Probe = () => {
  const draft = useNewRunDraft();
  return <p data-testid="draft">{JSON.stringify({ intent: draft.intent, held: draft.guided.heldText ?? null })}</p>;
};
const now = (): { intent: RunIntent; held: Record<string, string> | null } => JSON.parse(screen.getByTestId("draft").textContent ?? "{}");

async function show(intent: RunIntent, origin?: NewRunOrigin) {
  render(
    <MemoryRouter>
      <ModelAccessProvider status={null} onRefresh={() => {}}>
        <NewRunDraftProvider owner="alice">
          <Seeded intent={intent} origin={origin}>
            <RunStep location={{ screen: "step", step: "run" }} go={vi.fn()} startFromScratch={() => {}} deps={{ load: vi.fn(async () => SCENARIO_POOLS.member) }} />
            <Probe />
          </Seeded>
        </NewRunDraftProvider>
      </ModelAccessProvider>
    </MemoryRouter>,
  );
  await waitFor(() => expect(screen.getByRole("combobox", { name: NR_RUN.POOL_LABEL })).toBeEnabled());
}
const press = (name: string) => userEvent.click(screen.getByRole("button", { name }));
const mode = (name: string) => userEvent.click(screen.getByRole("radio", { name }));

const codex = { id: "codex-cli", kind: "harness" as const };
const TASK = "fix the flaky test";
const TEMPLATE: NewRunOrigin = { kind: "template", id: "d1111111-1111-4111-8111-111111111111", revision: 1, name: "On a VM", templateKind: "template" };

describe("Run step: agent text held beside the draft", () => {
  // A VM starts no harness, so choosing one clears the start: its first message is held, and Autonomous gives it back.
  it("holds the started harness's first message across a switch to a VM", async () => {
    await show({ shape: "agent_task", experience: "interactive", tools: [codex], startup: { kind: "harness", tool: "codex-cli", prompt: TASK } });
    await press(NR_MACHINE.VM);
    expect(now()).toEqual({ intent: { shape: "agent_task", experience: "interactive", tools: [codex], machine_kind: "linux_vm" }, held: { "codex-cli": TASK } });
    await mode(NR_RUN.MODE_AUTONOMOUS);
    expect(now().intent).toMatchObject({ experience: "background", workload: { kind: "agent_task", agent: "codex-cli", task: TASK } });
  });

  it("a switch to a VM keeps what is already held for another agent", async () => {
    const other = { id: "claude-code", kind: "harness" as const };
    await show({ shape: "agent_task", experience: "background", tools: [codex, { ...other, task: "review" }], workload: { kind: "agent_task", agent: "codex-cli", task: TASK } });
    await mode(NR_RUN.MODE_INTERACTIVE);
    expect(now()).toMatchObject({ intent: { startup: { kind: "harness", tool: "codex-cli", prompt: TASK } }, held: { "claude-code": "review" } });
    await press(NR_MACHINE.VM);
    expect(now().held).toEqual({ "claude-code": "review", "codex-cli": TASK });
    await mode(NR_RUN.MODE_AUTONOMOUS);
    expect(now().intent).toMatchObject({ tools: [codex, { ...other, task: "review" }], workload: { kind: "agent_task", agent: "codex-cli", task: TASK } });
  });

  // A template on a VM that starts a harness and names no run mode: the Run step names it Interactive, which starts no
  // harness on a VM, and the first message is held for Autonomous to give back.
  it("a VM template's first message survives the Run step naming its mode", async () => {
    await show({ shape: "agent_task", machine_kind: "linux_vm", tools: [codex], startup: { kind: "harness", tool: "codex-cli", prompt: TASK } }, TEMPLATE);
    expect(now()).toEqual({ intent: { shape: "agent_task", experience: "interactive", tools: [codex], machine_kind: "linux_vm" }, held: { "codex-cli": TASK } });
    await mode(NR_RUN.MODE_AUTONOMOUS);
    expect(now().intent).toMatchObject({ experience: "background", workload: { kind: "agent_task", agent: "codex-cli", task: TASK } });
  });

  // The same template with a workload beside the start is named Autonomous, and the start's first message is its task.
  it("a VM template that names a workload beside its start keeps the first message as the task", async () => {
    await show({ shape: "agent_task", machine_kind: "linux_vm", tools: [codex], workload: { kind: "agent_task", agent: "codex-cli", task: "" }, startup: { kind: "harness", tool: "codex-cli", prompt: TASK } }, TEMPLATE);
    expect(now().intent).toEqual({ shape: "agent_task", experience: "background", tools: [codex], machine_kind: "linux_vm", workload: { kind: "agent_task", agent: "codex-cli", task: TASK } });
    await mode(NR_RUN.MODE_INTERACTIVE);
    await mode(NR_RUN.MODE_AUTONOMOUS);
    expect(now().intent.workload).toEqual({ kind: "agent_task", agent: "codex-cli", task: TASK });
  });

  // The fact is left in place on a switch to Autonomous, so each switch to Interactive must replace it: text it carries
  // in the run is held no longer, and is never given to a harness started later.
  it("a switch to Interactive that leaves no text out holds none", async () => {
    await show({ shape: "agent_task", experience: "background", machine_kind: "linux_vm", tools: [codex], workload: { kind: "agent_task", agent: "codex-cli", task: TASK } });
    await mode(NR_RUN.MODE_INTERACTIVE);
    expect(now().held).toEqual({ "codex-cli": TASK });
    await mode(NR_RUN.MODE_AUTONOMOUS);
    await press(NR_MACHINE.CONTAINER);
    await mode(NR_RUN.MODE_INTERACTIVE);
    expect(now()).toEqual({ intent: { shape: "agent_task", experience: "interactive", tools: [codex], startup: { kind: "harness", tool: "codex-cli", prompt: TASK } }, held: null });
  });
});
