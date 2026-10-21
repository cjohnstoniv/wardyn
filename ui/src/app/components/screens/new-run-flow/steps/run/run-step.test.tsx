/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, isInaccessible, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { setField } from "../../../../../../test/set-field";
import { SCENARIO_POOLS, SHARED, TEAM_LAPTOPS, barrierOptions, effectiveLimits, unavailablePool } from "../../../../../lib/new-run-fixtures/scenarios/pools";
import { RUNNER_POOL_REFUSAL } from "../../../../../lib/runner-pool-refusals";
import type { RunnerPoolCatalogueEntry, RunnerPoolList } from "../../../../../lib/types/runner-pools";
import { NR_RUN } from "../../../../wardyn/copy/new-run/run";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { ModelAccessProvider } from "../../../../wardyn/model-access-context";
import { browserZone, todayDate, type StartChoice } from "../../../../../lib/schedule";
import type { SetupStatus } from "../../../../../lib/types/setup";
import type { RunIntent } from "../../draft/run-intent";
import { poolScope } from "../../lenses/run-lens";
import { NewRunDraftProvider, useNewRunDispatch, useNewRunDraft } from "../../new-run-route";
import { RunStep } from "./run-step";

const AGENT: RunIntent = { shape: "agent_task", experience: "interactive" };
const MEMBER = SCENARIO_POOLS.member;

// Text queries ignore CSS display; the barrier table renders a table and a card set and CSS shows one.
const WIDE = ".hidden{display:none}.md\\:block{display:block}.md\\:hidden{display:none}";
const sheet = document.createElement("style");
sheet.textContent = WIDE;

function Seeded({ intent, schedule, children }: { intent: RunIntent; schedule?: StartChoice; children: React.ReactNode }) {
  const dispatch = useNewRunDispatch();
  const [ready, setReady] = React.useState(false);
  React.useEffect(() => {
    dispatch({ type: "edit", apply: () => intent });
    if (schedule) dispatch({ type: "schedule", schedule });
    setReady(true);
  }, [dispatch, intent, schedule]);
  return ready ? children : null;
}
const Probe = () => <p data-testid="intent">{JSON.stringify(useNewRunDraft().intent)}</p>;
const ScheduleProbe = () => <p data-testid="schedule">{JSON.stringify(useNewRunDraft().schedule ?? null)}</p>;
const scheduleNow = () => JSON.parse(screen.getByTestId("schedule").textContent ?? "null");
const intentNow = (): RunIntent => JSON.parse(screen.getByTestId("intent").textContent ?? "{}");

function show(intent: RunIntent, list: RunnerPoolList = MEMBER, status: SetupStatus | null = null, schedule?: StartChoice) {
  document.head.append(sheet);
  const go = vi.fn();
  const load = vi.fn(async () => list);
  render(
    <MemoryRouter>
      <ModelAccessProvider status={status} onRefresh={() => {}}>
        <NewRunDraftProvider owner="alice">
          <Seeded intent={intent} schedule={schedule}>
            <RunStep location={{ screen: "step", step: "run" }} go={go} startFromScratch={() => {}} deps={{ load }} />
            <Probe />
            <ScheduleProbe />
          </Seeded>
        </NewRunDraftProvider>
      </ModelAccessProvider>
    </MemoryRouter>,
  );
  return { go, load };
}
// The select is on screen while the answer is still loading; it is enabled once the answer is in.
const poolSelect = async () => {
  const select = await screen.findByRole("combobox", { name: NR_RUN.POOL_LABEL });
  await waitFor(() => expect(select).toBeEnabled());
  return select;
};
const tally = (runs: number) => ({ runs, cpu_millis: 0, memory_mib: 0, disk_mib: 0 });
const optionLabels = (select: HTMLElement) => within(select).getAllByRole("option").map((o) => o.textContent);
const list = (pools: RunnerPoolCatalogueEntry[], over: Partial<RunnerPoolList> = {}): RunnerPoolList => ({ ...MEMBER, pools, ...over });
const barrierRadio = (barrier: string) => screen.getAllByRole("radio", { name: barrier }).find((r) => !isInaccessible(r)) as HTMLInputElement;

afterEach(() => {
  cleanup();
  sheet.remove();
});

describe("Run step: pool select", () => {
  it("lists the answer minus a not-found row, preselects the default and writes no pool until the person changes it", async () => {
    const hidden = unavailablePool({ ...SHARED, id: "gone", name: "Gone" }, "runner_pool_not_found");
    show(AGENT, list([SHARED, TEAM_LAPTOPS, hidden], { default: { pool_id: SHARED.id, selection: "organisation_default" } }));
    const select = await poolSelect();
    expect(optionLabels(select)).toEqual([NR_RUN.POOL_OPTION_REASON("Shared pool", NR_RUN.FIT_STARTS_NOW), NR_RUN.POOL_OPTION_REASON("Team laptops", NR_RUN.FIT_STARTS_NOW)]);
    expect(select).toHaveValue(SHARED.id);
    expect(intentNow().runner_pool_id).toBeUndefined();
    await userEvent.selectOptions(select, TEAM_LAPTOPS.id);
    expect(intentNow().runner_pool_id).toBe(TEAM_LAPTOPS.id);
  });

  it("says so on the step when the draft's pool is no longer in the list, and not when the person simply has none yet", async () => {
    show({ ...AGENT, runner_pool_id: "gone" }, list([SHARED, TEAM_LAPTOPS]));
    expect(await screen.findByText(RUNNER_POOL_REFUSAL.NOT_FOUND())).toBeInTheDocument();
    cleanup();
    show(AGENT, list([SHARED, TEAM_LAPTOPS]));
    await poolSelect();
    expect(screen.queryByText(RUNNER_POOL_REFUSAL.NOT_FOUND())).not.toBeInTheDocument();
  });

  it.each([
    ["runner_pool_unavailable", NR_RUN.REASON_SWITCHED_OFF],
    ["placement_denied", NR_RUN.REASON_NOT_ALLOWED_PROFILE],
    ["placement_unavailable", NR_RUN.REASON_RUNNERS_OFF],
    ["runner_pool_no_eligible_member", NR_RUN.REASON_NO_RUNNERS],
    ["runner_pool_no_online_runner", NR_RUN.REASON_NO_ONLINE],
  ] as const)("keeps a pool with %s visible, disabled, with its reason", async (reason, label) => {
    show(AGENT, list([SHARED, unavailablePool(TEAM_LAPTOPS, reason)]));
    const option = await screen.findByRole("option", { name: NR_RUN.POOL_OPTION_REASON("Team laptops", label) });
    expect(option).toBeDisabled();
  });

  it("says on each usable pool whether a run starts now or waits, and nothing where the list says neither", async () => {
    const full = { ...SHARED, id: "full", name: "CI pool", slots: { used: 10, waiting: 3, max: 10, verdict: "waits" as const } };
    const noRoom = {
      ...TEAM_LAPTOPS, id: "busy", name: "Busy laptops",
      capacity: { total: tally(2), in_use: tally(2), available: tally(0), largest_per_run: { cpu_millis: 2000, memory_mib: 4096, disk_mib: 0 }, runners: 1, runners_online: 1 },
    };
    const unknown = { ...SHARED, id: "quiet", name: "Quiet pool", slots: undefined };
    show(AGENT, list([SHARED, full, noRoom, unknown]));
    expect(optionLabels(await poolSelect())).toEqual([
      NR_RUN.POOL_OPTION_REASON("Shared pool", NR_RUN.FIT_STARTS_NOW),
      NR_RUN.POOL_OPTION_REASON("CI pool", NR_RUN.FIT_WAITS),
      NR_RUN.POOL_OPTION_REASON("Busy laptops", NR_RUN.FIT_WAITS),
      "Quiet pool",
    ]);
  });

  it("names the run type the pool does not take, from the type asked", async () => {
    const refused = unavailablePool(TEAM_LAPTOPS, "runner_pool_run_type_not_allowed");
    show({ ...AGENT, experience: "background", workload: { kind: "agent_task", task: "x" } }, list([SHARED, refused]));
    expect(await screen.findByRole("option", { name: NR_RUN.POOL_OPTION_REASON("Team laptops", NR_RUN.REASON_INTERACTIVE_ONLY) })).toBeDisabled();
    cleanup();
    show(AGENT, list([SHARED, refused]));
    expect(await screen.findByRole("option", { name: NR_RUN.POOL_OPTION_REASON("Team laptops", NR_RUN.REASON_BACKGROUND_ONLY) })).toBeDisabled();
  });

  it("asks for the run type of the draft, and again when the mode changes", async () => {
    const { load } = show(AGENT);
    await poolSelect();
    expect(load).toHaveBeenLastCalledWith("interactive", expect.any(AbortSignal));
    await userEvent.click(screen.getByRole("radio", { name: NR_RUN.MODE_AUTONOMOUS }));
    await waitFor(() => expect(load).toHaveBeenLastCalledWith("background", expect.any(AbortSignal)));
    expect(intentNow().experience).toBe("background");
  });

  it("names Interactive in the draft when an agent run reaches the step with no mode, so the server is never left to infer one", async () => {
    const { load } = show({ shape: "agent_task" });
    await poolSelect();
    expect(intentNow().experience).toBe("interactive");
    expect(load).toHaveBeenCalledTimes(1);
    expect(load).toHaveBeenCalledWith("interactive", expect.any(AbortSignal));
  });

  it("keeps a composer draft's task and names it Autonomous when the step mounts with no mode", async () => {
    const { load } = show({ shape: "agent_task", workload: { kind: "agent_task", task: "fix the flaky test" } });
    await poolSelect();
    expect(intentNow()).toMatchObject({ experience: "background", workload: { kind: "agent_task", task: "fix the flaky test" } });
    expect(intentNow().startup).toBeUndefined();
    expect(screen.getByRole("radio", { name: NR_RUN.MODE_AUTONOMOUS })).toBeChecked();
    expect(load).toHaveBeenCalledTimes(1);
    expect(load).toHaveBeenCalledWith("background", expect.any(AbortSignal));
  });

  it("never shows the answer for the run type it replaced, however late it arrives", async () => {
    const answers: Record<string, (l: RunnerPoolList) => void> = {};
    const load = vi.fn((runType?: string) => new Promise<RunnerPoolList>((resolve) => { answers[runType ?? ""] = resolve; }));
    render(
      <MemoryRouter>
        <NewRunDraftProvider owner="alice">
          <Seeded intent={AGENT}>
            <RunStep location={{ screen: "step", step: "run" }} go={vi.fn()} startFromScratch={() => {}} deps={{ load }} />
          </Seeded>
        </NewRunDraftProvider>
      </MemoryRouter>,
    );
    await waitFor(() => expect(load).toHaveBeenCalledWith("interactive", expect.any(AbortSignal)));
    await userEvent.click(screen.getByRole("radio", { name: NR_RUN.MODE_AUTONOMOUS }));
    await waitFor(() => expect(load).toHaveBeenCalledWith("background", expect.any(AbortSignal)));
    answers.background(list([{ ...SHARED, name: "Background pool" }]));
    expect(await screen.findByRole("option", { name: NR_RUN.POOL_OPTION_REASON("Background pool", NR_RUN.FIT_STARTS_NOW) })).toBeInTheDocument();
    answers.interactive(list([{ ...SHARED, name: "Interactive pool" }]));
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.queryByRole("option", { name: NR_RUN.POOL_OPTION_REASON("Interactive pool", NR_RUN.FIT_STARTS_NOW) })).not.toBeInTheDocument();
    expect(screen.getByRole("option", { name: NR_RUN.POOL_OPTION_REASON("Background pool", NR_RUN.FIT_STARTS_NOW) })).toBeInTheDocument();
  });

  it("with no default and no choice, Continue focuses the pool and says one is required", async () => {
    const { go } = show(AGENT, list([SHARED, TEAM_LAPTOPS], { default: {} }));
    const select = await poolSelect();
    expect(select).toHaveValue("");
    await userEvent.click(screen.getByRole("button", { name: NR_SHARED.CONTINUE_TO(NR_SHARED.STEP_HARNESSES) }));
    expect(select).toHaveFocus();
    expect(await screen.findByText(RUNNER_POOL_REFUSAL.REQUIRED())).toBeInTheDocument();
    expect(go).not.toHaveBeenCalled();
    await userEvent.selectOptions(select, SHARED.id);
    await userEvent.click(screen.getByRole("button", { name: NR_SHARED.CONTINUE_TO(NR_SHARED.STEP_HARNESSES) }));
    expect(go).toHaveBeenCalledWith({ screen: "step", step: "harnesses" });
  });

  it.each([
    ["an organisation default", SCENARIO_POOLS.member, NR_RUN.LEDE_ORG],
    ["a personal default", SCENARIO_POOLS.admin, NR_RUN.LEDE_PERSONAL],
    ["no default", SCENARIO_POOLS.offline, NR_RUN.LEDE_NONE],
  ])("ledes %s", async (_name, answer, lede) => {
    show(AGENT, answer);
    expect(await screen.findByText(lede)).toBeInTheDocument();
  });
});

describe("Run step: pool summary", () => {
  it("shows total capacity as a meter against the pool's maximum, and the maximum", async () => {
    show(AGENT, list([{ ...SHARED, slots: { used: 3, waiting: 0, max: 10, verdict: "starts_now" } }]));
    const meter = await screen.findByRole("meter", { name: NR_RUN.CAPACITY_ARIA });
    expect(meter).toHaveAttribute("aria-valuetext", NR_RUN.USED_OF_MAX(3, 10));
    expect(screen.getByText(NR_RUN.MAX_RUNS(10))).toBeInTheDocument();
  });

  it("with no pool maximum, shows the running count as text and no meter", async () => {
    show(AGENT, list([{ ...SHARED, slots: { used: 4, waiting: 0, verdict: "starts_now" } }]));
    expect(await screen.findByText(NR_RUN.RUNNING_ONLY(4))).toBeInTheDocument();
    expect(screen.getByText(NR_RUN.POOL_LIMIT_FROM_RUNNERS)).toBeInTheDocument();
    expect(screen.queryByRole("meter", { name: NR_RUN.CAPACITY_ARIA })).not.toBeInTheDocument();
  });

  it("hides total capacity and the waiting cell until the server sends slots", async () => {
    show(AGENT, list([{ ...SHARED, slots: undefined }]));
    await screen.findByText(NR_RUN.RUN_TYPES_LABEL);
    expect(screen.queryByText(NR_RUN.CAPACITY_LABEL)).not.toBeInTheDocument();
    expect(screen.queryByText(NR_RUN.WAITING_LABEL)).not.toBeInTheDocument();
  });

  it("counts the allowance across all pools, singular and plural, and says when there is no personal limit", async () => {
    show(AGENT, list([SHARED], { allowance: { active: 1, max: 5 } }));
    const meter = await screen.findByRole("meter", { name: NR_RUN.ALLOWANCE_ARIA });
    expect(meter).toHaveAttribute("aria-valuetext", NR_RUN.ALLOWANCE_USED(1, 5));
    expect(screen.getByText(NR_RUN.ALLOWANCE_INCLUDES_ONE)).toBeInTheDocument();
    cleanup();
    show(AGENT, list([SHARED], { allowance: { active: 3, max: 5 } }));
    expect(await screen.findByText(NR_RUN.ALLOWANCE_INCLUDES(3))).toBeInTheDocument();
    cleanup();
    show(AGENT, list([SHARED], { allowance: { active: 3 } }));
    expect(await screen.findByText(NR_RUN.NO_PERSONAL_LIMIT)).toBeInTheDocument();
    cleanup();
    show(AGENT, list([SHARED], { allowance: undefined }));
    await screen.findByText(NR_RUN.RUN_TYPES_LABEL);
    expect(screen.queryByText(NR_RUN.ALLOWANCE_LABEL)).not.toBeInTheDocument();
  });

  it("reads waiting from the count and the verdict: full with nobody waiting still says new runs join the queue", async () => {
    show(AGENT, list([{ ...SHARED, slots: { used: 10, waiting: 0, max: 10, verdict: "waits" } }]));
    expect(await screen.findByText(NR_RUN.NO_RUNS_WAITING)).toBeInTheDocument();
    expect(screen.getByText(NR_RUN.JOIN_QUEUE)).toBeInTheDocument();
    cleanup();
    show(AGENT, SCENARIO_POOLS.full);
    expect(await screen.findByText(NR_RUN.IN_QUEUE(3))).toBeInTheDocument();
    cleanup();
    show(AGENT, MEMBER);
    expect(await screen.findByText(NR_RUN.START_WHEN_LAUNCHED)).toBeInTheDocument();
  });

  it.each([
    [["interactive", "background"], "pool", NR_RUN.RUN_TYPES_BOTH, NR_RUN.SET_BY_POOL],
    [["interactive", "background"], "none", NR_RUN.RUN_TYPES_BOTH, NR_RUN.NO_RESTRICTION],
    [["background"], "pool", NR_RUN.RUN_TYPES_BACKGROUND, NR_RUN.SET_BY_POOL],
    [["interactive"], "pool", NR_RUN.RUN_TYPES_INTERACTIVE, NR_RUN.SET_BY_POOL],
  ] as const)("shows run types %j from source %s", async (types, source, value, sub) => {
    show(AGENT, list([{ ...SHARED, run_types: [...types], run_types_source: source }]));
    const cell = (await screen.findByText(NR_RUN.RUN_TYPES_LABEL)).parentElement as HTMLElement;
    expect(within(cell).getByText(value)).toBeInTheDocument();
    expect(within(cell).getByText(sub)).toBeInTheDocument();
  });

  it.each([
    ["a named profile", { active: 0, max: 5, profile_name: "Platform team" }, NR_RUN.LIMITS_NOTE("Platform team")],
    ["no profile name", { active: 0, max: 5 }, NR_RUN.LIMITS_NOTE_ORG],
    ["no allowance", undefined, NR_RUN.LIMITS_NOTE_UNGOVERNED],
  ])("heads the per-run limits for %s", async (_name, allowance, note) => {
    show(AGENT, list([SHARED], { allowance }));
    expect(await screen.findByText(note)).toBeInTheDocument();
  });

  it("shows each per-run limit with its own source, and an unbounded one as no maximum", async () => {
    const limits = effectiveLimits("interactive", {
      cpu_millis: { default: { value: 1000 }, cap: { value: 2000 }, cap_source: "governance" },
      memory_mib: { default: { value: 1024 }, cap: { value: 4096 }, cap_source: "runner" },
      lifetime_sec: { default: { value: 0, unlimited: true }, cap: { value: 0, unlimited: true } },
      disk_mib: { default: { value: 5120 }, cap: { value: 10240 }, cap_source: "pool" },
    });
    show(AGENT, list([{ ...SHARED, limits: [limits] }], { allowance: { active: 0, max: 5, profile_name: "Platform team" } }));
    await screen.findByText(NR_RUN.LIMITS_LABEL);
    const cell = (key: string) => document.querySelector(`[data-limit="${key}"]`) as HTMLElement;
    expect(within(cell("cpu")).getByText("2 CPU")).toBeInTheDocument();
    expect(within(cell("cpu")).getByText(NR_RUN.SET_BY("Platform team"))).toBeInTheDocument();
    expect(within(cell("memory")).getByText("4,096 MiB")).toBeInTheDocument();
    expect(within(cell("memory")).getByText(NR_RUN.SET_BY_RUNNERS)).toBeInTheDocument();
    expect(within(cell("lifetime")).getByText(NR_RUN.LIMIT_NO_MAXIMUM)).toBeInTheDocument();
    expect(within(cell("drive")).getByText(NR_SHARED.GIB("10"))).toBeInTheDocument();
    expect(within(cell("drive")).getByText(NR_RUN.LIMIT_EXTRA_DRIVES)).toBeInTheDocument();
    expect(within(cell("drive")).getByText(NR_RUN.SET_BY_POOL)).toBeInTheDocument();
  });

  it("says a limit the deployment bounds is set by this server", async () => {
    const limits = effectiveLimits("interactive", { memory_mib: { default: { value: 1024 }, cap: { value: 4096 }, cap_source: "deployment" } });
    show(AGENT, list([{ ...SHARED, limits: [limits] }]));
    await screen.findByText(NR_RUN.LIMITS_LABEL);
    expect(within(document.querySelector('[data-limit="memory"]') as HTMLElement).getByText(NR_RUN.SET_BY_SERVER)).toBeInTheDocument();
  });

  it("reads the limits of the run type asked, not the pool's other type", async () => {
    const pool = { ...SHARED, limits: [effectiveLimits("background"), effectiveLimits("interactive", { cpu_millis: { default: { value: 500 }, cap: { value: 1000 }, cap_source: "pool" } })] };
    show(AGENT, list([pool]));
    await screen.findByText(NR_RUN.LIMITS_LABEL);
    expect(within(document.querySelector('[data-limit="cpu"]') as HTMLElement).getByText("1 CPU")).toBeInTheDocument();
  });
});

describe("Run step: barrier", () => {
  it("always shows the three barriers and selects the server's pick without writing it", async () => {
    show(AGENT);
    await poolSelect();
    for (const name of ["Fence", "Wall", "Vault"]) expect(barrierRadio(name)).toBeInTheDocument();
    expect(barrierRadio("Wall")).toBeChecked();
    expect(intentNow().confinement_class).toBeUndefined();
    expect(screen.getAllByText(NR_RUN.BADGE_DEFAULT).length).toBeGreaterThan(0);
    expect(screen.getAllByText(NR_RUN.BADGE_EXPERIMENTAL).length).toBeGreaterThan(0);
    expect(screen.getByText(NR_RUN.POLICY_NOTE)).toBeInTheDocument();
  });

  it("writes confinement_class only once the person picks a different barrier", async () => {
    show(AGENT);
    await poolSelect();
    await userEvent.click(barrierRadio("Vault"));
    expect(intentNow().confinement_class).toBe("CC3");
  });

  it("names the organisation on its default, and the floor that rules a barrier out", async () => {
    show(AGENT, SCENARIO_POOLS.governed);
    await poolSelect();
    expect(screen.getAllByText(NR_RUN.BADGE_ORG_DEFAULT).length).toBeGreaterThan(0);
    expect(barrierRadio("Fence")).toBeDisabled();
    expect(screen.getAllByText(NR_RUN.BARRIER_REQUIRED("Wall")).length).toBeGreaterThan(0);
  });

  it.each([
    ["below_floor", { floor: "CC2" }, NR_RUN.BARRIER_REQUIRED("Wall")],
    ["policy_floor", { floor: "CC2" }, NR_RUN.BARRIER_POLICY_REQUIRES("Wall")],
    ["access_floor", { floor: "CC3" }, NR_RUN.BARRIER_ACCESS_VAULT],
    ["pool_not_allowed", {}, NR_RUN.BARRIER_POOL_DISALLOWS("Shared pool", "Fence")],
    ["not_offered", {}, NR_RUN.BARRIER_NONE_OFFERS("Fence")],
    ["not_offered", { reason_detail: "kvm_missing" }, NR_RUN.BARRIER_NO_KVM],
    ["unknown", {}, NR_RUN.BARRIER_KNOWN_WHEN_ONLINE],
  ] as const)("says %s %j as its sentence on a disabled Fence", async (code, extra, sentence) => {
    const options = barrierOptions({ CC1: { available: false, reason_code: code, ...extra } });
    show(AGENT, list([{ ...SHARED, barrier_options: options }]));
    await poolSelect();
    expect(barrierRadio("Fence")).toBeDisabled();
    expect(screen.getAllByText(sentence).length).toBeGreaterThan(0);
  });

  it("keeps three disabled rows, not known yet, while no pool is chosen", async () => {
    show(AGENT, SCENARIO_POOLS.offline);
    await poolSelect();
    for (const name of ["Fence", "Wall", "Vault"]) expect(barrierRadio(name)).toBeDisabled();
    expect(screen.getAllByText(NR_RUN.BARRIER_UNKNOWN).length).toBeGreaterThan(0);
  });

  it("shows where it runs for a User pool only", async () => {
    show(AGENT, list([SHARED, TEAM_LAPTOPS]));
    const select = await poolSelect();
    expect(screen.queryByText(NR_RUN.ROW_WHERE)).not.toBeInTheDocument();
    await userEvent.selectOptions(select, TEAM_LAPTOPS.id);
    expect((await screen.findAllByText(NR_RUN.ROW_WHERE)).length).toBeGreaterThan(0);
  });

  it("reads the pool's scope as the console's pool word", () => {
    expect(poolScope("organization")).toBe("organization");
    expect(poolScope("user")).toBe("user");
  });
});

describe("Run step: mode, timing and command", () => {
  it("offers Interactive and Autonomous for an agent, and Schedule for later only as Coming soon", async () => {
    show({ shape: "agent_task", experience: "background", workload: { kind: "agent_task", task: "x" } });
    await poolSelect();
    expect(screen.getByRole("radio", { name: NR_RUN.MODE_AUTONOMOUS })).toBeChecked();
    expect(screen.getByRole("radio", { name: NR_RUN.TIMING_NOW })).toBeChecked();
    const later = screen.getByRole("radio", { name: NR_RUN.TIMING_LATER });
    // Sentence case like every other option in the flow (#2867).
    expect(screen.getByRole("radio", { name: "Run now" })).toBe(screen.getByRole("radio", { name: NR_RUN.TIMING_NOW }));
    expect(later).toHaveAccessibleName("Schedule for later");
    expect(later).toBeDisabled();
    expect(within(later.closest("label") as HTMLElement).getByText(NR_SHARED.COMING_SOON)).toBeInTheDocument();
  });

  it("shows no timing for an Interactive run and no mode for an environment", async () => {
    show(AGENT);
    await poolSelect();
    expect(screen.queryByText(NR_RUN.TIMING_LABEL)).not.toBeInTheDocument();
    cleanup();
    show({ shape: "environment", experience: "interactive" });
    await poolSelect();
    expect(screen.queryByText(NR_RUN.MODE_LABEL)).not.toBeInTheDocument();
    expect(screen.getByRole("heading", { name: NR_RUN.TITLE_ENV })).toBeInTheDocument();
  });

  it("takes a command for an application, with Application / server Coming soon", async () => {
    show({ shape: "application", experience: "background", workload: { kind: "command", command: "" } });
    await poolSelect();
    expect(screen.getByRole("heading", { name: NR_RUN.TITLE_APP })).toBeInTheDocument();
    const server = screen.getByRole("radio", { name: NR_RUN.APP_SERVER });
    expect(server).toBeDisabled();
    expect(within(server.closest("label") as HTMLElement).getByText(NR_SHARED.COMING_SOON)).toBeInTheDocument();
    setField(screen.getByLabelText(new RegExp(NR_RUN.COMMAND_LABEL)), "make test");
    expect(intentNow().workload).toEqual({ kind: "command", command: "make test" });
    expect(screen.getByText(NR_RUN.COMMAND_HELP)).toBeInTheDocument();
  });

  it("keeps Continue on an application until the command is written", async () => {
    const { go } = show({ shape: "application", experience: "background", workload: { kind: "command", command: "" } });
    await poolSelect();
    await userEvent.click(screen.getByRole("button", { name: NR_SHARED.CONTINUE_TO(NR_SHARED.STEP_HARNESSES) }));
    expect(screen.getByLabelText(new RegExp(NR_RUN.COMMAND_LABEL))).toHaveFocus();
    expect(go).not.toHaveBeenCalled();
  });
});

describe("Run step: Schedule for later", () => {
  const ON = { features: { scheduled_runs: true } } as SetupStatus;
  const AUTONOMOUS: RunIntent = { shape: "agent_task", experience: "background", workload: { kind: "agent_task", task: "x" } };
  const pick = async () => {
    await poolSelect();
    await userEvent.click(screen.getByRole("radio", { name: NR_RUN.TIMING_LATER }));
  };

  it("stays Coming soon, with no fields, when features.scheduled_runs is off or absent", async () => {
    for (const status of [null, { features: { scheduled_runs: false } } as SetupStatus, {} as SetupStatus]) {
      show(AUTONOMOUS, MEMBER, status);
      await poolSelect();
      expect(screen.getByRole("radio", { name: NR_RUN.TIMING_LATER })).toBeDisabled();
      expect(screen.queryByLabelText(NR_RUN.START_DATE)).not.toBeInTheDocument();
      cleanup();
    }
  });

  it("shows no start when the switch is off, even for a draft that holds one", async () => {
    function Holds() {
      const dispatch = useNewRunDispatch();
      React.useEffect(() => dispatch({ type: "schedule", schedule: { date: "2026-10-21", time: "02:00", zone: "UTC" } }), [dispatch]);
      return null;
    }
    document.head.append(sheet);
    render(
      <MemoryRouter>
        <ModelAccessProvider status={{} as SetupStatus} onRefresh={() => {}}>
          <NewRunDraftProvider owner="alice">
            <Seeded intent={AUTONOMOUS}>
              <RunStep location={{ screen: "step", step: "run" }} go={vi.fn()} startFromScratch={() => {}} deps={{ load: async () => MEMBER }} />
              <Holds />
            </Seeded>
          </NewRunDraftProvider>
        </ModelAccessProvider>
      </MemoryRouter>,
    );
    await poolSelect();
    expect(screen.getByRole("radio", { name: NR_RUN.TIMING_NOW })).toBeChecked();
    expect(screen.queryByLabelText(NR_RUN.START_DATE)).not.toBeInTheDocument();
  });

  it("opens Date, Time and Time zone when on, losing its Coming soon, and tells nothing until a field is touched", async () => {
    show(AUTONOMOUS, MEMBER, ON);
    await pick();
    const later = screen.getByRole("radio", { name: NR_RUN.TIMING_LATER });
    expect(later).toBeEnabled();
    expect(later).toBeChecked();
    expect(within(later.closest("label") as HTMLElement).queryByText(NR_SHARED.COMING_SOON)).not.toBeInTheDocument();
    expect(screen.getByLabelText(NR_RUN.START_DATE)).toBeInTheDocument();
    expect(screen.getByLabelText(NR_RUN.START_TIME)).toBeInTheDocument();
    expect(screen.getByLabelText(NR_RUN.START_ZONE)).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    for (const label of [NR_RUN.START_DATE, NR_RUN.START_TIME]) expect(screen.getByLabelText(label)).not.toHaveAttribute("aria-invalid", "true");
    expect(screen.getByText(NR_RUN.START_HELP)).toBeInTheDocument();
    expect(scheduleNow()).toMatchObject({ date: "", time: "" });
  });

  it("tells the missing date once a field is touched, as an alert tied to the date and time by aria-describedby", async () => {
    show(AUTONOMOUS, MEMBER, ON);
    await pick();
    setField(screen.getByLabelText(NR_RUN.START_TIME), "02:00");
    const alert = screen.getByRole("alert");
    expect(alert).toHaveTextContent(NR_RUN.START_MISSING);
    const help = screen.getByText(NR_RUN.START_HELP);
    for (const label of [NR_RUN.START_DATE, NR_RUN.START_TIME]) {
      const input = screen.getByLabelText(label);
      expect(input).toHaveAttribute("aria-invalid", "true");
      expect(input.getAttribute("aria-describedby")?.split(" ")).toEqual([alert.id, help.id]);
    }
  });

  it("ties the help to every field, and the error to none while there is no error", async () => {
    show(AUTONOMOUS, MEMBER, ON);
    await pick();
    const help = screen.getByText(NR_RUN.START_HELP);
    expect(help.id).not.toBe("");
    for (const label of [NR_RUN.START_DATE, NR_RUN.START_TIME, NR_RUN.START_ZONE]) {
      expect(screen.getByLabelText(label).getAttribute("aria-describedby")).toBe(help.id);
    }
  });

  it("comes back with values already typed telling their problem at once", async () => {
    show(AUTONOMOUS, MEMBER, ON, { date: "2020-01-01", time: "02:00", zone: "UTC" });
    await poolSelect();
    expect(screen.getByRole("alert")).toHaveTextContent(NR_RUN.START_PAST);
  });

  it("sets the earliest date from the picked zone's today and keeps the browser's zone first in the list", async () => {
    show(AUTONOMOUS, MEMBER, ON);
    await pick();
    const date = screen.getByLabelText(NR_RUN.START_DATE);
    await userEvent.selectOptions(screen.getByLabelText(NR_RUN.START_ZONE), "Pacific/Kiritimati");
    expect(date).toHaveAttribute("min", todayDate("Pacific/Kiritimati"));
    const east = date.getAttribute("min");
    await userEvent.selectOptions(screen.getByLabelText(NR_RUN.START_ZONE), "Pacific/Pago_Pago");
    expect(date).toHaveAttribute("min", todayDate("Pacific/Pago_Pago"));
    expect(date.getAttribute("min")).not.toBe(east);
    const zone = screen.getByLabelText(NR_RUN.START_ZONE) as HTMLSelectElement;
    expect(zone.value).toBe("Pacific/Pago_Pago");
    expect(zone.options[0].value).toBe(browserZone());
  });

  it("starts the zone at the browser's, and says the start once it is a good one", async () => {
    show(AUTONOMOUS, MEMBER, ON);
    await pick();
    await userEvent.selectOptions(screen.getByLabelText(NR_RUN.START_ZONE), "UTC");
    const date = new Date(Date.now() + 2 * 86_400_000).toISOString().slice(0, 10);
    setField(screen.getByLabelText(NR_RUN.START_DATE), date);
    setField(screen.getByLabelText(NR_RUN.START_TIME), "02:00");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent(/^Starts [A-Z][a-z]{2} \d{1,2} [A-Z][a-z]{2}, 02:00 \(UTC\)$/);
    expect(scheduleNow()).toEqual({ date, time: "02:00", zone: "UTC" });
  });

  it("refuses a past time and a start past 7 days less a background run's 24 hours, in the board's sentences", async () => {
    show(AUTONOMOUS, MEMBER, ON);
    await pick();
    await userEvent.selectOptions(screen.getByLabelText(NR_RUN.START_ZONE), "UTC");
    setField(screen.getByLabelText(NR_RUN.START_DATE), "2020-01-01");
    setField(screen.getByLabelText(NR_RUN.START_TIME), "02:00");
    expect(screen.getByRole("alert")).toHaveTextContent(NR_RUN.START_PAST);
    expect(screen.getByLabelText(NR_RUN.START_DATE)).toHaveAttribute("aria-invalid", "true");
    setField(screen.getByLabelText(NR_RUN.START_DATE), new Date(Date.now() + 9 * 86_400_000).toISOString().slice(0, 10));
    expect(screen.getByRole("alert")).toHaveTextContent(NR_RUN.START_FAR);
    setField(screen.getByLabelText(NR_RUN.START_DATE), new Date(Date.now() + 5 * 86_400_000).toISOString().slice(0, 10));
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("goes back to Run now, dropping the start", async () => {
    show(AUTONOMOUS, MEMBER, ON);
    await pick();
    await userEvent.click(screen.getByRole("radio", { name: NR_RUN.TIMING_NOW }));
    expect(scheduleNow()).toBeNull();
    expect(screen.queryByLabelText(NR_RUN.START_DATE)).not.toBeInTheDocument();
  });
});
