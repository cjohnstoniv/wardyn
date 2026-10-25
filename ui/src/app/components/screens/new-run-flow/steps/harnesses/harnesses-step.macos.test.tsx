/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Harnesses step for a macOS VM (mock M-N1 frames 9a to 9c).
import * as React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { NR_HARNESSES } from "../../../../wardyn/copy/new-run/harnesses";
import { NR_MACHINE } from "../../../../wardyn/copy/new-run/machine";
import { SCENARIO_HARNESSES, SCENARIO_HARNESS_SETS } from "../../../../../lib/new-run-fixtures/scenarios/harnesses";
import { NewRunDraftProvider, useNewRunDispatch } from "../../new-run-route";
import type { RunIntent } from "../../draft/run-intent";
import { MacOSOfferContext, type MacOSOffer } from "../run/macos-offer";
import { HarnessesStep } from "./harnesses-step";
import type { Roster } from "./roster";

afterEach(cleanup);

const ROSTER: Roster = { harnesses: SCENARIO_HARNESSES.full, sets: SCENARIO_HARNESS_SETS.full };
const OFFER = (runner?: string): Extract<MacOSOffer, { ok: true }> => ({ ok: true, guestOS: "macOS 27.0.1", minCPUs: 4, minMemoryMiB: 8192, harnesses: ["none"], ...(runner && { runner }) });
const MACOS: RunIntent = { shape: "agent_task", experience: "interactive", machine_kind: "macos_vm" };

function Seeded({ intent, children }: { intent: RunIntent; children: React.ReactNode }) {
  const dispatch = useNewRunDispatch();
  const [ready, setReady] = React.useState(false);
  React.useEffect(() => {
    dispatch({ type: "edit", apply: () => intent });
    setReady(true);
  }, [dispatch, intent]);
  return ready ? children : null;
}
function show(intent: RunIntent, offer: MacOSOffer) {
  render(
    <MemoryRouter>
      <MacOSOfferContext.Provider value={offer}>
        <NewRunDraftProvider owner="alice">
          <Seeded intent={intent}>
            <HarnessesStep location={{ screen: "step", step: "harnesses" }} go={vi.fn()} startFromScratch={() => {}} deps={{ load: async () => ROSTER }} />
          </Seeded>
        </NewRunDraftProvider>
      </MacOSOfferContext.Provider>
    </MemoryRouter>,
  );
}
const picker = () => screen.findByRole("combobox", { name: NR_HARNESSES.EYEBROW(1) });

describe("Harnesses step: macOS VM", () => {
  it("frame 9a: the picker keeps Your own tools and disables the harnesses the base cannot serve", async () => {
    show({ ...MACOS, tools: [{ id: "none", kind: "harness" }] }, OFFER("mac-studio-07"));
    const select = await picker();
    const options = within(select).getAllByRole("option") as HTMLOptionElement[];
    const by = (text: string) => options.find((o) => o.textContent === text) as HTMLOptionElement;
    expect(by("Your own tools")).toBeEnabled();
    expect(by("Claude Code")).toBeDisabled();
    expect(by("Codex CLI")).toBeDisabled();
  });

  it("frame 9b: a harness carried over from Linux says, for a pool of one Mac, which Mac and what to choose", async () => {
    show({ ...MACOS, tools: [{ id: "claude-code", kind: "harness" }] }, OFFER("mac-studio-07"));
    const select = await picker();
    expect(document.getElementById(select.getAttribute("aria-describedby") ?? "")).toHaveTextContent(NR_MACHINE.MACOS_HARNESS_ONE("mac-studio-07"));
  });

  it("frame 9c: for a pool of several Macs the same refusal names no runner", async () => {
    show({ ...MACOS, tools: [{ id: "claude-code", kind: "harness" }] }, OFFER());
    await picker();
    expect(screen.getByText(NR_MACHINE.MACOS_HARNESS_POOL("Claude Code"))).toBeInTheDocument();
  });

  it("serves every harness the base lists, and every harness on a Linux VM", async () => {
    show({ ...MACOS, tools: [{ id: "claude-code", kind: "harness" }] }, { ...OFFER("mac-studio-07"), harnesses: ["none", "claude-code"] });
    let select = await picker();
    expect(select.getAttribute("aria-describedby")).toBeNull();
    cleanup();
    show({ ...MACOS, machine_kind: "linux_vm", tools: [{ id: "claude-code", kind: "harness" }] }, OFFER("mac-studio-07"));
    select = await picker();
    expect(select.getAttribute("aria-describedby")).toBeNull();
  });
});
