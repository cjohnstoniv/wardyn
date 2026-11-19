/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// KM-1: the Run step's "Between runs" group. Each test names the board frame it draws.

import * as React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { runs } from "../../../../../lib/api/runs";
import { shortDate } from "../../../../../lib/format";
import { SCENARIO_POOLS } from "../../../../../lib/new-run-fixtures/scenarios/pools";
import { aheadByHours } from "../../../../../lib/test-clock";
import type { KeptMachine, MyMachines } from "../../../../../lib/types";
import type { SetupStatus } from "../../../../../lib/types/setup";
import { gbOf, keptCount, KEEP_NEW_RUN as T } from "../../../../wardyn/copy/keep-machine";
import { MAC_BASES } from "../../../../wardyn/copy/mac-bases";
import { NR_MACHINE } from "../../../../wardyn/copy/new-run/machine";
import { NR_RUN } from "../../../../wardyn/copy/new-run/run";
import { ModelAccessProvider } from "../../../../wardyn/model-access-context";
import { OperatorProvider } from "../../../../wardyn/operator-context";
import type { RunIntent } from "../../draft/run-intent";
import { NewRunDraftProvider, useNewRunDispatch, useNewRunDraft } from "../../new-run-route";
import { KeptMachinesContext, type KeptMachinesRead } from "./kept-machines-provider";
import { MacOSOfferContext, type MacOSOffer } from "./macos-offer";
import { RunStep } from "./run-step";

const GB = 1024 ** 3;
const MAC = "Maya's MacBook";
const STATUS = { features: { macos_vms: true } } as SetupStatus;
const OFFER: MacOSOffer = { ok: true, guestOS: "macOS 26.1", minCPUs: 4, minMemoryMiB: 8192, harnesses: ["none"], runner: MAC, poolScope: "user" };
const INTENT: RunIntent = { shape: "agent_task", experience: "interactive", tools: [{ kind: "harness", id: "claude-code" }], no_repositories_or_drives: true, machine_kind: "macos_vm" };

const machine = (over: Partial<KeptMachine> = {}): KeptMachine => ({
  id: "m-dev", name: "dev", owner_subject: "me", runner_id: "r1", base: "Standard", base_prepared_at: aheadByHours(-500), state: "kept", scrubbed: true,
  disk_bytes: 12 * GB, created_at: aheadByHours(-600), last_used_at: aheadByHours(-48), expires_at: aheadByHours(24 * 28), runner_name: MAC, ...over,
});
const ready = (machines: KeptMachine[] = [machine()], over: Partial<MyMachines> = {}): KeptMachinesRead => ({
  status: "ready", data: { machines, allowed: true, count: machines.length, limit: 2, worst_bytes: 51 * GB, ...over },
});

beforeEach(() => {
  vi.spyOn(runs, "previewRunPolicyWire").mockResolvedValue({ spec: {}, source: { kind: "inline" }, warnings: [], pending: [], repository_access: [] } as never);
  vi.spyOn(runs, "getRun").mockResolvedValue({ id: "run_1234567890", title: "ui-polish" } as never);
});
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function Seeded({ intent, children }: { intent: RunIntent; children: React.ReactNode }) {
  const dispatch = useNewRunDispatch();
  const [done, setDone] = React.useState(false);
  React.useEffect(() => {
    dispatch({ type: "edit", apply: () => intent });
    setDone(true);
  }, [dispatch, intent]);
  return done ? children : null;
}
const Probe = () => <p data-testid="intent">{JSON.stringify(useNewRunDraft().intent)}</p>;
const intentNow = (): RunIntent => JSON.parse(screen.getByTestId("intent").textContent ?? "{}");

async function show(read: KeptMachinesRead, over: { intent?: RunIntent; offer?: MacOSOffer | undefined } = {}) {
  render(
    <MemoryRouter>
      <OperatorProvider operator={false} principal="alice">
        <ModelAccessProvider status={STATUS} onRefresh={() => {}}>
          <MacOSOfferContext.Provider value={"offer" in over ? over.offer : OFFER}>
            <KeptMachinesContext.Provider value={read}>
              <NewRunDraftProvider owner="alice">
                <Seeded intent={over.intent ?? INTENT}>
                  <RunStep location={{ screen: "step", step: "run" }} go={vi.fn()} startFromScratch={() => {}} deps={{ load: vi.fn(async () => SCENARIO_POOLS.member) }} />
                  <Probe />
                </Seeded>
              </NewRunDraftProvider>
            </KeptMachinesContext.Provider>
          </MacOSOfferContext.Provider>
        </ModelAccessProvider>
      </OperatorProvider>
    </MemoryRouter>,
  );
  await screen.findByRole("combobox", { name: NR_RUN.POOL_LABEL });
}
const group = () => screen.queryByTestId("nr-kept-machine");
const radio = (name: string) => within(group()!).getByRole("radio", { name });

describe("Run step: Between runs", () => {
  it("km1-default: Delete is chosen, Keep and one Resume are offered, with the count line", async () => {
    await show(ready());
    expect(within(group()!).getByText(T.LABEL)).toBeInTheDocument();
    expect(radio(T.DELETE)).toBeChecked();
    expect(radio(T.KEEP)).not.toBeChecked();
    expect(radio(T.RESUME("dev"))).not.toBeChecked();
    expect(within(group()!).getByText(T.RESUME_META(shortDate(machine().last_used_at), "Standard", 12))).toBeInTheDocument();
    expect(within(group()!).getByText(keptCount(1, 2))).toBeInTheDocument();
    expect(intentNow().machine).toBeUndefined();
    expect(screen.getByText(NR_MACHINE.CHANGES_SIGN_INS_RUN_DISK)).toBeInTheDocument();
  });

  it("km1-keep: choosing Keep says where and how much, shows the name field and the secrets note, and the draft keeps", async () => {
    await show(ready());
    await userEvent.click(radio(T.KEEP));
    expect(intentNow().machine).toEqual({ keep: true });
    expect(within(group()!).getByText(T.KEEP_DETAIL(MAC, gbOf(51 * GB)))).toBeInTheDocument();
    expect(within(group()!).getByText(T.NAME_HINT)).toBeInTheDocument();
    expect(within(group()!).getByText(T.SECRETS)).toBeInTheDocument();
    // K-14 replaces MV-C04 in the changes panel.
    expect(screen.getByText(T.SIGN_INS(MAC))).toBeInTheDocument();
    expect(screen.queryByText(NR_MACHINE.CHANGES_SIGN_INS_RUN_DISK)).toBeNull();
  });

  it("km1-keep: the idle days show when the server sends them", async () => {
    await show(ready([machine()], { idle_days: 7 }));
    await userEvent.click(radio(T.KEEP));
    expect(within(group()!).getByText(T.KEEP_DETAIL(MAC, gbOf(51 * GB), 7))).toBeInTheDocument();
  });

  it("km1-keep-name: a typed name rides the draft; emptied, it is dropped", async () => {
    await show(ready());
    await userEvent.click(radio(T.KEEP));
    const field = within(group()!).getByLabelText(T.NAME);
    await userEvent.type(field, "scratch-1");
    expect(intentNow().machine).toEqual({ keep: true, name: "scratch-1" });
    await userEvent.clear(field);
    expect(intentNow().machine).toEqual({ keep: true });
  });

  it("km1-resume: Resume dev puts the machine's id in the draft, with Start fresh unticked and K-14 in the panel", async () => {
    await show(ready());
    await userEvent.click(radio(T.RESUME("dev")));
    expect(intentNow().machine).toEqual({ resume: "m-dev" });
    expect(within(group()!).getByRole("checkbox", { name: T.FRESH })).not.toBeChecked();
    expect(screen.getByText(T.SIGN_INS(MAC))).toBeInTheDocument();
  });

  it("km1-resume-fresh: Start fresh adds fresh, says what it does and that the base stays; the base picker is not offered", async () => {
    await show(ready(), { offer: { ...OFFER, bases: [{ name: "Xcode 26", state: "ready" }] } });
    expect(screen.getByRole("button", { name: MAC_BASES.B01 })).toBeInTheDocument();
    await userEvent.click(radio(T.RESUME("dev")));
    expect(screen.queryByRole("button", { name: MAC_BASES.B01 })).toBeNull();
    await userEvent.click(within(group()!).getByRole("checkbox", { name: T.FRESH }));
    expect(intentNow().machine).toEqual({ resume: "m-dev", fresh: true });
    expect(within(group()!).getByText(T.FRESH_HINT("dev", MAC))).toBeInTheDocument();
    expect(within(group()!).getByText(T.FRESH_BASE("Standard"))).toBeInTheDocument();
  });

  it("km1-resume-fresh: choosing Keep or Delete afterwards drops the fresh start", async () => {
    await show(ready());
    await userEvent.click(radio(T.RESUME("dev")));
    await userEvent.click(within(group()!).getByRole("checkbox", { name: T.FRESH }));
    await userEvent.click(radio(T.DELETE));
    expect(intentNow().machine).toBeUndefined();
  });

  it("km1-resume-not-scrubbed: a machine whose last stop was abrupt says Wardyn cleans it when it starts", async () => {
    await show(ready([machine({ scrubbed: false })]));
    expect(within(group()!).getByText(T.NOT_SCRUBBED)).toBeInTheDocument();
    expect(radio(T.RESUME("dev"))).toBeEnabled();
  });

  it("km1-in-use: a machine a run holds can't be resumed, and the row names that run", async () => {
    await show(ready([machine({ state: "in_use", run_id: "run_1234567890" })]));
    expect(radio(T.RESUME("dev"))).toBeDisabled();
    expect(await within(group()!).findByText(T.IN_USE("ui-polish"))).toBeInTheDocument();
  });

  it("a disabled option is described by the line that says why", async () => {
    await show(ready([machine({ state: "in_use", run_id: "run_1234567890" })]));
    const why = await within(group()!).findByText(T.IN_USE("ui-polish"));
    expect(radio(T.RESUME("dev"))).toHaveAccessibleDescription(why.parentElement!.textContent ?? "");
    cleanup();
    await show(ready([machine()], { allowed: false, reason: "kept_machine_person_limit", limit: 1 }));
    expect(radio(T.KEEP)).toHaveAccessibleDescription(keptCount(1, 1));
  });

  it("a machine being made can't be resumed either", async () => {
    await show(ready([machine({ state: "creating", run_id: "run_1234567890" })]));
    expect(radio(T.RESUME("dev"))).toBeDisabled();
  });

  it("km1-at-limit: Keep is disabled at the person's limit, the count line says so, and Resume still works", async () => {
    await show(ready([machine()], { allowed: false, reason: "kept_machine_person_limit", limit: 1 }));
    expect(radio(T.KEEP)).toBeDisabled();
    expect(within(group()!).getByText(keptCount(1, 1))).toBeInTheDocument();
    expect(radio(T.RESUME("dev"))).toBeEnabled();
  });

  it("km1-none-kept: two options and no Resume row", async () => {
    await show(ready([]));
    expect(within(group()!).getAllByRole("radio")).toHaveLength(2);
  });

  it.each([
    ["km1-absent-site-off", ready([], { allowed: false, reason: "kept_machine_site_off" })],
    ["governance denies it", ready([], { allowed: false, reason: "governance_unit" })],
    ["km1-absent-other-mac", ready([], { allowed: false, reason: "kept_machine_not_your_mac" })],
    ["a caller the server says has no list", { status: "absent" } as KeptMachinesRead],
    ["a read still loading", { status: "loading" } as KeptMachinesRead],
  ])("%s: no section", async (_n, read) => {
    await show(read);
    expect(group()).toBeNull();
    expect(screen.getByText(NR_MACHINE.CHANGES_SIGN_INS_RUN_DISK)).toBeInTheDocument();
  });

  it("km1-absent-pool: an organisation pool, or one of several Macs, draws no section", async () => {
    await show(ready(), { offer: { ...OFFER, poolScope: "organization" } });
    expect(group()).toBeNull();
    cleanup();
    await show(ready(), { offer: { ...OFFER, runner: undefined } });
    expect(group()).toBeNull();
  });

  it("km1-absent-linux: a Linux VM draws no section", async () => {
    await show(ready(), { intent: { ...INTENT, machine_kind: "linux_vm" } });
    expect(group()).toBeNull();
  });

  it("a background run draws no section", async () => {
    await show(ready(), { intent: { ...INTENT, experience: "background" } });
    expect(group()).toBeNull();
  });

  it("a keep the server's answer proves invalid is cleared, and the step says so", async () => {
    await show(ready([], { allowed: false, reason: "kept_machine_site_off" }), { intent: { ...INTENT, machine: { keep: true } } });
    await waitFor(() => expect(intentNow().machine).toBeUndefined());
    expect(await screen.findByTestId("nr-kept-machine-cleared")).toHaveTextContent(T.UNAVAILABLE);
  });

  it("a keep at the person's limit is cleared with the limit sentence", async () => {
    await show(ready([machine()], { allowed: false, reason: "kept_machine_person_limit", limit: 1 }), { intent: { ...INTENT, machine: { keep: true } } });
    await waitFor(() => expect(intentNow().machine).toBeUndefined());
    expect(await screen.findByText(T.AT_LIMIT)).toBeInTheDocument();
  });

  it("a resume of a machine the list proves gone is cleared with the visible line; one still listed stays", async () => {
    await show(ready(), { intent: { ...INTENT, machine: { resume: "m-gone" } } });
    await waitFor(() => expect(intentNow().machine).toBeUndefined());
    expect(await screen.findByText(T.GONE)).toBeInTheDocument();
    cleanup();
    await show(ready(), { intent: { ...INTENT, machine: { resume: "m-dev" } } });
    expect(radio(T.RESUME("dev"))).toBeChecked();
    expect(intentNow().machine).toEqual({ resume: "m-dev" });
    expect(screen.queryByTestId("nr-kept-machine-cleared")).toBeNull();
  });

  it("choosing again ends the notice", async () => {
    await show(ready(), { intent: { ...INTENT, machine: { resume: "m-gone" } } });
    await screen.findByText(T.GONE);
    await userEvent.click(radio(T.KEEP));
    expect(screen.queryByText(T.GONE)).toBeNull();
    expect(intentNow().machine).toEqual({ keep: true });
  });

  it("a read that failed keeps the choice, says so with a retry, and the retry is the read's", async () => {
    const retry = vi.fn();
    await show({ status: "failed", retry }, { intent: { ...INTENT, machine: { resume: "m-dev" } } });
    expect(screen.getByTestId("nr-kept-machine-failed")).toHaveTextContent(T.READ_FAILED);
    await userEvent.click(screen.getByRole("button", { name: T.RETRY }));
    expect(retry).toHaveBeenCalledOnce();
    expect(intentNow().machine).toEqual({ resume: "m-dev" });
    expect(screen.queryByTestId("nr-kept-machine-cleared")).toBeNull();
  });

  it("a failed read with nothing chosen still says so where the section would be; elsewhere it says nothing", async () => {
    await show({ status: "failed", retry: vi.fn() });
    expect(screen.getByTestId("nr-kept-machine-failed")).toBeInTheDocument();
    cleanup();
    await show({ status: "failed", retry: vi.fn() }, { offer: { ...OFFER, poolScope: "organization" } });
    expect(screen.queryByTestId("nr-kept-machine-failed")).toBeNull();
  });

  it("a read still loading keeps the choice", async () => {
    await show({ status: "loading" }, { intent: { ...INTENT, machine: { keep: true } } });
    expect(intentNow().machine).toEqual({ keep: true });
    expect(screen.queryByTestId("nr-kept-machine-cleared")).toBeNull();
  });

  it("a pool offer still loading leaves the draft's machine alone", async () => {
    await show(ready(), { intent: { ...INTENT, machine: { keep: true } }, offer: undefined });
    expect(intentNow().machine).toEqual({ keep: true });
  });
});
