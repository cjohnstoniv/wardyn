/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Choosing a shape that is a background run takes a kept or resumed machine out of the draft, and says so.

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { KEEP_NEW_RUN } from "../../../../wardyn/copy/keep-machine";
import { NR_SHAPE } from "../../../../wardyn/copy/new-run/shape";
import { OperatorProvider } from "../../../../wardyn/operator-context";
import type { RunIntent } from "../../draft/run-intent";
import { NEW_RUN_SCREENS, NewRunRoute, useNewRunDispatch, useNewRunDraft, type NewRunScreenName, type NewRunScreenProps } from "../../new-run-route";
import { ShapeStep } from "./shape-step";

const wfetchMock = vi.fn();
vi.mock("../../../../../lib/api/core", async (orig) => ({ ...(await orig<typeof import("../../../../../lib/api/core")>()), wfetch: (...a: unknown[]) => wfetchMock(...a) }));

const KEPT: RunIntent = { shape: "environment", experience: "interactive", machine_kind: "macos_vm", machine: { keep: true } };

// The draft is seeded by a button beside the step, as a rerun carries no machine.
function Probe() {
  const draft = useNewRunDraft();
  const dispatch = useNewRunDispatch();
  return (
    <>
      <button onClick={() => dispatch({ type: "edit", apply: () => KEPT })}>seed</button>
      <p data-testid="draft">{JSON.stringify({ machine: draft.intent.machine ?? null, cleared: draft.guided.machineCleared ?? null })}</p>
    </>
  );
}
const read = (): { machine: unknown; cleared: string | null } => JSON.parse(screen.getByTestId("draft").textContent ?? "{}");
const inert = Object.fromEntries((Object.keys(NEW_RUN_SCREENS) as NewRunScreenName[]).map((n) => [n, () => <Probe />])) as unknown as typeof NEW_RUN_SCREENS;
const WithMachine = (p: NewRunScreenProps) => <><ShapeStep {...p} /><Probe /></>;

function show() {
  wfetchMock.mockImplementation(async (path: string) => {
    if (path === "/templates") return new Response(JSON.stringify({ templates: [], publish: {} }), { status: 200, headers: { "content-type": "application/json" } });
    if (path === "/me/launch-intents") return new Response(JSON.stringify({ intents: [] }), { status: 200, headers: { "content-type": "application/json" } });
    throw new Error(`unexpected ${path}`);
  });
  render(
    <MemoryRouter initialEntries={["/runs/new?step=shape"]}>
      <OperatorProvider operator={false} principal="alice">
        <Routes>
          <Route path="/runs/new" element={<NewRunRoute loaders={{ template: () => Promise.reject(new Error("no")), again: () => Promise.reject(new Error("no")) }} screens={{ ...inert, shape: WithMachine }} />} />
        </Routes>
      </OperatorProvider>
    </MemoryRouter>,
  );
}
afterEach(() => {
  cleanup();
  wfetchMock.mockReset();
});

describe("Shape step: a kept machine", () => {
  it("a background shape clears it and says why", async () => {
    show();
    await userEvent.click(await screen.findByRole("button", { name: "seed" }));
    expect(read().machine).toEqual({ keep: true });
    await userEvent.click(await screen.findByRole("button", { name: NR_SHAPE.APP_TITLE }));
    expect(read()).toEqual({ machine: null, cleared: KEEP_NEW_RUN.INTERACTIVE_ONLY });
  });

  it("an interactive shape keeps it and says nothing", async () => {
    show();
    await userEvent.click(await screen.findByRole("button", { name: "seed" }));
    await userEvent.click(await screen.findByRole("button", { name: NR_SHAPE.ENV_TITLE }));
    expect(read()).toEqual({ machine: { keep: true }, cleared: null });
  });
});
