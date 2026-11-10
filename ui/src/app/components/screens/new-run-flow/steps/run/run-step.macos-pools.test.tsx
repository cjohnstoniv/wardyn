/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A3.4 (board NR-4): while the chosen pool refuses macOS, the pools that offer it are buttons under the refusal.

import * as React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { SHARED, TEAM_LAPTOPS, unavailablePool } from "../../../../../lib/new-run-fixtures/scenarios/pools";
import type { RunnerPoolCatalogueEntry, RunnerPoolList } from "../../../../../lib/types/runner-pools";
import type { SetupStatus } from "../../../../../lib/types/setup";
import { NR_MACHINE } from "../../../../wardyn/copy/new-run/machine";
import { NR_RUN } from "../../../../wardyn/copy/new-run/run";
import { ModelAccessProvider } from "../../../../wardyn/model-access-context";
import type { RunIntent } from "../../draft/run-intent";
import { NewRunDraftProvider, useNewRunDispatch, useNewRunDraft } from "../../new-run-route";
import { MachineKindSection } from "./machine-kind-section";
import { MacOSOfferContext, type MacOSOffer } from "./macos-offer";
import { MacOSOfferProvider } from "./macos-offer-provider";
import { RunStep } from "./run-step";

afterEach(cleanup);

const ON = { features: { macos_vms: true } } as SetupStatus;
const mac = (id: string, name: string, min_cpus: number, min_memory_mib: number): RunnerPoolCatalogueEntry => ({
  ...SHARED, id, name, macos_vm: { offered: true, guest_os: "macOS 26.6.2", min_cpus, min_memory_mib, harnesses: ["none"], runner: `${name} Mac` },
});
const CLUSTER: RunnerPoolCatalogueEntry = { ...SHARED, name: "Cluster pool", macos_vm: { offered: false, refusals: [] } };
const STUDIO = mac("c3333333-3333-4333-8333-333333333333", "Studio Macs", 4, 8192);
const BUILD = mac("c4444444-4444-4444-8444-444444444444", "Build Macs", 6, 16384);
const list = (...pools: RunnerPoolCatalogueEntry[]): RunnerPoolList => ({ pools, allowance: { active: 1, max: 5 }, default: { pool_id: pools[0].id, selection: "organisation_default" } });
const VM: RunIntent = { shape: "agent_task", experience: "interactive", machine_kind: "linux_vm" };

function Seeded({ intent, children }: { intent: RunIntent; children: React.ReactNode }) {
  const dispatch = useNewRunDispatch();
  const [ready, setReady] = React.useState(false);
  React.useEffect(() => {
    dispatch({ type: "edit", apply: () => intent });
    setReady(true);
  }, [dispatch, intent]);
  return ready ? children : null;
}
const Probe = () => <p data-testid="intent">{JSON.stringify(useNewRunDraft().intent)}</p>;
const intentNow = (): RunIntent => JSON.parse(screen.getByTestId("intent").textContent ?? "{}");

function show(pools: RunnerPoolList, intent: RunIntent = VM) {
  const load = vi.fn(async () => pools);
  render(
    <MemoryRouter>
      <ModelAccessProvider status={ON} onRefresh={() => {}}>
        <NewRunDraftProvider owner="alice">
          <Seeded intent={intent}>
            <MacOSOfferProvider load={load}>
              <RunStep location={{ screen: "step", step: "run" }} go={vi.fn()} startFromScratch={() => {}} deps={{ load }} />
              <Probe />
            </MacOSOfferProvider>
          </Seeded>
        </NewRunDraftProvider>
      </ModelAccessProvider>
    </MemoryRouter>,
  );
}
const osGroup = () => screen.getByRole("group", { name: NR_MACHINE.OS_LABEL });
const offerRow = () => screen.queryByTestId("nr-macos-pools");
const pick = (pool: string) => screen.getByRole("button", { name: NR_MACHINE.MACOS_POOL_PICK(pool) });
const select = () => screen.getByRole("combobox", { name: NR_RUN.POOL_LABEL }) as HTMLSelectElement;

describe("Run step: pools that offer macOS", () => {
  it("lists the other pools that offer macOS, in the pool list's order, under the refusal", async () => {
    show(list(CLUSTER, STUDIO, BUILD));
    await waitFor(() => expect(offerRow()).not.toBeNull());
    expect(within(offerRow()!).getByText(NR_MACHINE.MACOS_POOLS_OFFER)).toBeInTheDocument();
    expect(within(offerRow()!).getAllByRole("button").map((b) => b.textContent)).toEqual(["Studio Macs", "Build Macs"]);
    expect(within(osGroup()).getByRole("button", { name: NR_MACHINE.OS_MACOS })).toBeDisabled();
    expect(NR_MACHINE.MACOS_POOLS_OFFER).toBe("Pools that offer macOS");
    expect(NR_MACHINE.MACOS_POOL_PICK("Studio Macs")).toBe("Use Studio Macs and macOS");
  });

  it("picking a pool chooses it and macOS, and starts CPU and memory at the PICKED pool's minimum", async () => {
    show(list(CLUSTER, STUDIO, BUILD));
    await waitFor(() => expect(offerRow()).not.toBeNull());
    await userEvent.click(pick("Build Macs"));
    expect(intentNow()).toMatchObject({ runner_pool_id: BUILD.id, machine_kind: "macos_vm", resources: { cpu_millis: 6000, memory_mib: 16384 } });
    await waitFor(() => expect(select().value).toBe(BUILD.id));
    expect(offerRow()).toBeNull();
    expect(within(osGroup()).getByRole("button", { name: NR_MACHINE.OS_MACOS })).toHaveAttribute("aria-pressed", "true");
  });

  it("moves focus to the selected macOS control when the pick lifts the refusal", async () => {
    show(list(CLUSTER, STUDIO));
    await waitFor(() => expect(offerRow()).not.toBeNull());
    pick("Studio Macs").focus();
    await userEvent.keyboard("{Enter}");
    await waitFor(() => expect(offerRow()).toBeNull());
    expect(document.activeElement).toBe(within(osGroup()).getByRole("button", { name: NR_MACHINE.OS_MACOS }));
  });

  it("moves focus to the pool selector when the pick leaves macOS still refused and the button is gone", async () => {
    const refusal: MacOSOffer = { ok: false, refusal: { why: "pool_not_offered" } };
    const props = { kind: "linux_vm" as const, onChoose: vi.fn(), onChooseBase: vi.fn(), onPickMacPool: vi.fn() };
    const view = (macPools: { id: string; name: string }[]) => (
      <ModelAccessProvider status={ON} onRefresh={() => {}}>
        <MacOSOfferContext.Provider value={refusal}>
          <MachineKindSection {...props} kind="linux_vm" macPools={macPools} />
          <select id="nr-pool" aria-label="Pool" />
        </MacOSOfferContext.Provider>
      </ModelAccessProvider>
    );
    const { rerender } = render(view([{ id: "p1", name: "Studio Macs" }]));
    await userEvent.click(screen.getByRole("button", { name: NR_MACHINE.MACOS_POOL_PICK("Studio Macs") }));
    expect(props.onPickMacPool).toHaveBeenCalledWith("p1");
    rerender(view([]));
    expect(document.activeElement).toBe(document.getElementById("nr-pool"));
  });

  it("lists no pool that is unavailable, the chosen one, or one that does not offer macOS, and draws no row when none is left", async () => {
    const down = unavailablePool(mac("c5555555-5555-4555-8555-555555555555", "Down Macs", 4, 8192), "runner_pool_no_online_runner");
    show(list(CLUSTER, down, { ...TEAM_LAPTOPS, macos_vm: { offered: false, refusals: [] } }));
    await waitFor(() => expect(within(osGroup()).getByRole("button", { name: NR_MACHINE.OS_MACOS })).toBeDisabled());
    expect(offerRow()).toBeNull();
    expect(screen.queryByText(NR_MACHINE.MACOS_POOLS_OFFER)).toBeNull();
  });

  it("draws no row while macOS is offered by the chosen pool", async () => {
    show(list(STUDIO, BUILD));
    await waitFor(() => expect(within(osGroup()).getByRole("button", { name: NR_MACHINE.OS_MACOS })).toBeEnabled());
    expect(offerRow()).toBeNull();
  });
});
