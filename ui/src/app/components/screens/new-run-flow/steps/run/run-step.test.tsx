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
import { NR_MACHINE } from "../../../../wardyn/copy/new-run/machine";
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

function show(intent: RunIntent, list: RunnerPoolList = MEMBER) {
  document.head.append(sheet);
  const go = vi.fn();
  const load = vi.fn(async () => list);
  render(
    <MemoryRouter>
      <NewRunDraftProvider owner="alice">
        <Seeded intent={intent}>
          <RunStep location={{ screen: "step", step: "run" }} go={go} startFromScratch={() => {}} deps={{ load }} />
          <Probe />
        </Seeded>
      </NewRunDraftProvider>
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

  it("takes a command for an application, with both cards available", async () => {
    show({ shape: "application", experience: "background", workload: { kind: "command", command: "" } });
    await poolSelect();
    expect(screen.getByRole("heading", { name: NR_RUN.TITLE_APP })).toBeInTheDocument();
    const server = screen.getByRole("radio", { name: NR_RUN.APP_SERVER });
    expect(server).toBeEnabled();
    expect(within(server.closest("label") as HTMLElement).getByText(NR_RUN.APP_SERVER_BODY)).toBeInTheDocument();
    expect(within(server.closest("label") as HTMLElement).queryByText(NR_SHARED.COMING_SOON)).not.toBeInTheDocument();
    expect(screen.getByRole("radio", { name: NR_RUN.APP_AUTOMATION })).toBeChecked();
    setField(screen.getByLabelText(new RegExp(NR_RUN.COMMAND_LABEL)), "make test");
    expect(intentNow().workload).toEqual({ kind: "command", command: "make test" });
    expect(screen.getByText(NR_RUN.COMMAND_HELP)).toBeInTheDocument();
    expect(screen.queryByText(NR_RUN.PORTS_LABEL)).not.toBeInTheDocument();
  });

  describe("Application / server (#1994)", () => {
    const SERVER: RunIntent = {
      shape: "application", experience: "interactive", startup: { kind: "command", command: "npm run dev" },
      inline_policy: { ui_apps: [{ name: "web", port: 3000, path: "/" }, { name: "api", port: 3000, path: "/api" }] },
    };
    const rows = () => Array.from(document.querySelectorAll<HTMLElement>("[data-app-row]"));
    const nameField = (row: number) => within(rows()[row]).getByLabelText(NR_RUN.APP_NAME_ARIA(row + 1));
    const portField = (row: number, who: string) => within(rows()[row]).getByLabelText(NR_RUN.APP_PORT_ARIA(who));
    const pathField = (row: number, who: string) => within(rows()[row]).getByLabelText(NR_RUN.APP_PATH_ARIA(who));

    it("choosing the server card moves the draft to an interactive run that starts the command, with one port to edit", async () => {
      show({ shape: "application", experience: "background", workload: { kind: "command", command: "make dev" } });
      await poolSelect();
      await userEvent.click(screen.getByRole("radio", { name: NR_RUN.APP_SERVER }));
      expect(screen.getByRole("heading", { name: NR_RUN.TITLE_APP_SERVER })).toBeInTheDocument();
      expect(intentNow()).toMatchObject({ experience: "interactive", startup: { kind: "command", command: "make dev" }, inline_policy: { ui_apps: [{ name: "web", port: 3000, path: "/" }] } });
      expect(intentNow().workload).toBeUndefined();
      expect(screen.getByLabelText(new RegExp(NR_RUN.START_COMMAND_LABEL))).toHaveValue("make dev");
      expect(screen.getByText(NR_RUN.START_COMMAND_HELP)).toBeInTheDocument();
      expect(screen.getByText(NR_RUN.PORTS_HELP)).toBeInTheDocument();
      expect(screen.getByText(NR_RUN.APP_TERMINAL_NOTE)).toBeInTheDocument();
      await userEvent.click(screen.getByRole("radio", { name: NR_RUN.APP_AUTOMATION }));
      expect(intentNow()).toMatchObject({ experience: "background", workload: { kind: "command", command: "make dev" } });
      expect(intentNow().inline_policy).toBeUndefined();
    });

    it("writes the start command, name, port and path of a row to the policy's ui_apps", async () => {
      show(SERVER);
      await poolSelect();
      setField(screen.getByLabelText(new RegExp(NR_RUN.START_COMMAND_LABEL)), "pnpm dev");
      expect(intentNow().startup).toEqual({ kind: "command", command: "pnpm dev" });
      setField(nameField(1), "backend");
      setField(portField(1, "backend"), "8080");
      setField(pathField(1, "backend"), "/v1");
      expect(intentNow().inline_policy?.ui_apps).toEqual([{ name: "web", port: 3000, path: "/" }, { name: "backend", port: 8080, path: "/v1" }]);
    });

    it("says a port outside 1 to 65535 and a port two rows share, on the row, as an alert", async () => {
      show({ ...SERVER, inline_policy: { ui_apps: [...(SERVER.inline_policy?.ui_apps ?? []), { name: "admin", port: 70000, path: "/admin" }] } });
      await poolSelect();
      expect(within(rows()[0]).queryByRole("alert")).not.toBeInTheDocument();
      expect(within(rows()[1]).getByRole("alert")).toHaveTextContent(NR_RUN.PORT_DUPLICATE(3000));
      expect(within(rows()[2]).getByRole("alert")).toHaveTextContent(NR_RUN.PORT_INVALID);
      expect(portField(2, "admin")).toHaveAttribute("aria-invalid", "true");
      setField(portField(1, "api"), "8080");
      expect(within(rows()[1]).queryByRole("alert")).not.toBeInTheDocument();
    });

    it("adds a row named for its place, removes one, and stops at eight with its reason", async () => {
      show(SERVER);
      await poolSelect();
      await userEvent.click(screen.getByRole("button", { name: NR_RUN.PORT_ADD }));
      expect(rows()).toHaveLength(3);
      expect(nameField(2)).toHaveValue("app3");
      expect(portField(2, "app3")).toHaveValue("");
      expect(within(rows()[2]).getByRole("alert")).toHaveTextContent(NR_RUN.PORT_INVALID);
      await userEvent.click(within(rows()[0]).getByRole("button", { name: NR_RUN.APP_REMOVE_ARIA("web") }));
      expect(rows()).toHaveLength(2);
      expect(intentNow().inline_policy?.ui_apps?.map((a) => a.name)).toEqual(["api", "app3"]);
      for (let n = 0; n < 6; n++) await userEvent.click(screen.getByRole("button", { name: NR_RUN.PORT_ADD }));
      expect(rows()).toHaveLength(8);
      expect(screen.getByRole("button", { name: NR_RUN.PORT_ADD })).toBeDisabled();
      expect(screen.getByText(NR_RUN.PORTS_FULL)).toBeInTheDocument();
    });

    it("says a port is needed when the last row is removed", async () => {
      show({ ...SERVER, inline_policy: { ui_apps: [{ name: "web", port: 3000, path: "/" }] } });
      await poolSelect();
      expect(screen.queryByText(NR_RUN.PORT_REQUIRED)).not.toBeInTheDocument();
      await userEvent.click(within(rows()[0]).getByRole("button", { name: NR_RUN.APP_REMOVE_ARIA("web") }));
      expect(screen.getByRole("alert")).toHaveTextContent("An application needs at least one port to open.");
      expect(intentNow().inline_policy).toBeUndefined();
    });

    it("names each control of a row for a screen reader, and calls an unnamed row by its place", async () => {
      show(SERVER);
      await poolSelect();
      expect(nameField(1)).toHaveValue("api");
      expect(portField(1, "api")).toHaveValue("3000");
      expect(pathField(1, "api")).toHaveValue("/api");
      expect(within(rows()[1]).getByRole("button", { name: NR_RUN.APP_REMOVE_ARIA("api") })).toBeInTheDocument();
      expect(NR_RUN.APP_NAME_ARIA(2)).toBe("Name for row 2");
      expect(NR_RUN.APP_PORT_ARIA("api")).toBe("Port for api");
      expect(NR_RUN.APP_PATH_ARIA("api")).toBe("Path for api");
      expect(NR_RUN.APP_REMOVE_ARIA("api")).toBe("Remove api");
      setField(nameField(0), "");
      expect(portField(0, NR_RUN.APP_ROW(1)).getAttribute("aria-label")).toBe("Port for row 1");
      expect(pathField(0, NR_RUN.APP_ROW(1)).getAttribute("aria-label")).toBe("Path for row 1");
      expect(within(rows()[0]).getByRole("button", { name: "Remove row 1" })).toBeInTheDocument();
    });

    it("moves focus to the Add button when the last row is removed", async () => {
      show({ ...SERVER, inline_policy: { ui_apps: [{ name: "web", port: 3000, path: "/" }] } });
      await poolSelect();
      await userEvent.click(within(rows()[0]).getByRole("button", { name: NR_RUN.APP_REMOVE_ARIA("web") }));
      expect(rows()).toHaveLength(0);
      expect(screen.getByRole("button", { name: NR_RUN.PORT_ADD })).toHaveFocus();
    });

    it("moves focus to the row above when the bottom row of several is removed", async () => {
      show({ ...SERVER, inline_policy: { ui_apps: [{ name: "web", port: 3000, path: "/" }, { name: "api", port: 3001, path: "/" }] } });
      await poolSelect();
      await userEvent.click(within(rows()[1]).getByRole("button", { name: NR_RUN.APP_REMOVE_ARIA("api") }));
      expect(rows()).toHaveLength(1);
      expect(within(rows()[0]).getByRole("button", { name: NR_RUN.APP_REMOVE_ARIA("web") })).toHaveFocus();
    });

    it("says a repeated name on the later row, a bad name and a bad path on their own rows", async () => {
      show({
        ...SERVER,
        inline_policy: { ui_apps: [{ name: "web", port: 3000, path: "/" }, { name: "web", port: 3001, path: "/" }, { name: "Bad_Name", port: 3002, path: "http://evil/" }] },
      });
      await poolSelect();
      expect(within(rows()[0]).queryByRole("alert")).not.toBeInTheDocument();
      expect(within(rows()[1]).getByRole("alert")).toHaveTextContent("Two applications are named web. Give each its own name.");
      expect(nameField(1)).toHaveAttribute("aria-invalid", "true");
      const bad = within(rows()[2]).getAllByRole("alert").map((a) => a.textContent);
      expect(bad).toEqual([
        "Use lower-case letters, numbers and dashes, up to 32 characters, starting and ending with a letter or number.",
        'Start the path with a single /, with no host and no "..".',
      ]);
      expect(pathField(2, "Bad_Name")).toHaveAttribute("aria-invalid", "true");
      setField(nameField(1), "api");
      expect(within(rows()[1]).queryByRole("alert")).not.toBeInTheDocument();
    });

    it("keeps Continue on a repeated name and on a bad path, and puts focus on that field", async () => {
      const { go } = show({ ...SERVER, inline_policy: { ui_apps: [{ name: "web", port: 3000, path: "/" }, { name: "web", port: 3001, path: "/" }] } });
      await poolSelect();
      await userEvent.click(screen.getByRole("button", { name: NR_SHARED.CONTINUE_TO(NR_SHARED.STEP_HARNESSES) }));
      expect(nameField(1)).toHaveFocus();
      setField(nameField(1), "api");
      setField(pathField(1, "api"), "//host");
      await userEvent.click(screen.getByRole("button", { name: NR_SHARED.CONTINUE_TO(NR_SHARED.STEP_HARNESSES) }));
      expect(pathField(1, "api")).toHaveFocus();
      expect(go).not.toHaveBeenCalled();
    });

    it("keeps only digits in a port", async () => {
      show(SERVER);
      await poolSelect();
      setField(portField(0, "web"), "80a80");
      expect(intentNow().inline_policy?.ui_apps?.[0].port).toBe(8080);
    });

    it("keeps Continue on a bad port, and puts focus on that row's port", async () => {
      const { go } = show({ ...SERVER, inline_policy: { ui_apps: [{ name: "web", port: 3000, path: "/" }, { name: "api", port: 70000, path: "/" }] } });
      await poolSelect();
      await userEvent.click(screen.getByRole("button", { name: NR_SHARED.CONTINUE_TO(NR_SHARED.STEP_HARNESSES) }));
      expect(portField(1, "api")).toHaveFocus();
      expect(go).not.toHaveBeenCalled();
    });

    it("asks for the start command before it continues", async () => {
      const { go } = show({ ...SERVER, startup: { kind: "command", command: "" } });
      await poolSelect();
      await userEvent.click(screen.getByRole("button", { name: NR_SHARED.CONTINUE_TO(NR_SHARED.STEP_HARNESSES) }));
      expect(screen.getByLabelText(new RegExp(NR_RUN.START_COMMAND_LABEL))).toHaveFocus();
      expect(go).not.toHaveBeenCalled();
    });
  });

  it("keeps Continue on an application until the command is written", async () => {
    const { go } = show({ shape: "application", experience: "background", workload: { kind: "command", command: "" } });
    await poolSelect();
    await userEvent.click(screen.getByRole("button", { name: NR_SHARED.CONTINUE_TO(NR_SHARED.STEP_HARNESSES) }));
    expect(screen.getByLabelText(new RegExp(NR_RUN.COMMAND_LABEL))).toHaveFocus();
    expect(go).not.toHaveBeenCalled();
  });
});

describe("Run step: machine kind (M5-Revised)", () => {
  it("starts on Container, with the barriers, and writes nothing until the person picks", async () => {
    show(AGENT);
    await poolSelect();
    expect(screen.getByRole("button", { name: NR_MACHINE.CONTAINER })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByText(NR_MACHINE.CONTAINER_HELP)).toBeInTheDocument();
    expect(barrierRadio("Wall")).toBeInTheDocument();
    expect(intentNow().machine_kind).toBeUndefined();
  });

  it("a virtual machine names Linux, says the VM is the barrier, drops the container barrier and comes back", async () => {
    show({ ...AGENT, confinement_class: "CC3" });
    await poolSelect();
    await userEvent.click(screen.getByRole("button", { name: NR_MACHINE.VM }));
    expect(intentNow().machine_kind).toBe("linux_vm");
    expect(intentNow().confinement_class).toBeUndefined();
    expect(screen.getByText(NR_MACHINE.VM_HELP)).toBeInTheDocument();
    expect(within(screen.getByRole("group", { name: NR_MACHINE.OS_LABEL })).getByRole("button", { name: NR_MACHINE.OS_LINUX })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByText(NR_MACHINE.OS_LATER)).toBeInTheDocument();
    const fact = screen.getByTestId("nr-vm-barrier");
    expect(within(fact).getByText(NR_MACHINE.BARRIER_VM)).toBeInTheDocument();
    expect(within(fact).getByText(NR_MACHINE.BARRIER_VM_WHY)).toBeInTheDocument();
    const changes = screen.getByRole("region", { name: NR_MACHINE.CHANGES_TITLE });
    expect(within(changes).getByText(NR_MACHINE.CHANGES_NETWORK_BODY)).toBeInTheDocument();
    expect(within(changes).getByText(NR_MACHINE.CHANGES_CLIPBOARD_BODY)).toBeInTheDocument();
    expect(screen.queryAllByRole("radio", { name: "Wall" }).filter((r) => !isInaccessible(r))).toHaveLength(0);
    // The pool stays: a VM still lands on a pool.
    expect(await poolSelect()).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: NR_MACHINE.CONTAINER }));
    expect(intentNow().machine_kind).toBeUndefined();
    expect(barrierRadio("Wall")).toBeInTheDocument();
  });
});
