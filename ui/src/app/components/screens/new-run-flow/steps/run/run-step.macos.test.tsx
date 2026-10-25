/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Run step's macOS option (mock M-N1 frames 1 to 8b, M-N2 frames 1 to 4).

import * as React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { SCENARIO_POOLS } from "../../../../../lib/new-run-fixtures/scenarios/pools";
import type { SetupStatus } from "../../../../../lib/types/setup";
import { NR_MACHINE } from "../../../../wardyn/copy/new-run/machine";
import { NR_RUN } from "../../../../wardyn/copy/new-run/run";
import { ModelAccessProvider } from "../../../../wardyn/model-access-context";
import type { RunIntent } from "../../draft/run-intent";
import { NewRunDraftProvider, useNewRunDispatch, useNewRunDraft } from "../../new-run-route";
import { MacOSOfferContext, type MacOSOffer, type MacOSRefusal } from "./macos-offer";
import { RunStep } from "./run-step";

afterEach(cleanup);

const ON = { features: { macos_vms: true } } as SetupStatus;
const READY: MacOSOffer = { ok: true, guestOS: "macOS 27.0.1 (26A434)", minCPUs: 4, minMemoryMiB: 8192, harnesses: ["none"] };
const AGENT: RunIntent = { shape: "agent_task", experience: "interactive" };

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

function show(intent: RunIntent, status: SetupStatus | null, offer?: MacOSOffer) {
  render(
    <MemoryRouter>
      <ModelAccessProvider status={status} onRefresh={() => {}}>
        <MacOSOfferContext.Provider value={offer}>
          <NewRunDraftProvider owner="alice">
            <Seeded intent={intent}>
              <RunStep location={{ screen: "step", step: "run" }} go={vi.fn()} startFromScratch={() => {}} deps={{ load: vi.fn(async () => SCENARIO_POOLS.member) }} />
              <Probe />
            </Seeded>
          </NewRunDraftProvider>
        </MacOSOfferContext.Provider>
      </ModelAccessProvider>
    </MemoryRouter>,
  );
}
const osGroup = () => screen.getByRole("group", { name: NR_MACHINE.OS_LABEL });
const macosButton = () => within(osGroup()).getByRole("button", { name: NR_MACHINE.OS_MACOS });
// A refusal's `code` spans are drawn as <code>, so the paragraph reads the text without its backticks.
const line = (text: string) => within(osGroup()).getByText((_, el) => el?.tagName === "P" && el.textContent === text.split("`").join(""));
const chooseVM = async () => {
  await screen.findByRole("combobox", { name: NR_RUN.POOL_LABEL });
  await userEvent.click(screen.getByRole("button", { name: NR_MACHINE.VM }));
};
const changes = () => screen.getByRole("region", { name: NR_MACHINE.CHANGES_TITLE });

describe("OS row: macOS offered (frames 1 and 3)", () => {
  it("is today's row while the organisation switch is off or absent: Linux only, with the macOS and Windows line", async () => {
    for (const status of [null, { features: { macos_vms: false } } as SetupStatus]) {
      show(AGENT, status);
      await chooseVM();
      expect(within(osGroup()).queryByRole("button", { name: NR_MACHINE.OS_MACOS })).toBeNull();
      expect(screen.getByText(NR_MACHINE.OS_LATER)).toBeInTheDocument();
      cleanup();
    }
  });

  it("adds macOS beside Linux when the switch is on, and the line names Windows alone", async () => {
    show(AGENT, ON, READY);
    await chooseVM();
    expect(within(osGroup()).getByRole("button", { name: NR_MACHINE.OS_LINUX })).toHaveAttribute("aria-pressed", "true");
    expect(macosButton()).toBeEnabled();
    expect(screen.getByText(NR_MACHINE.OS_WINDOWS_LATER)).toBeInTheDocument();
    expect(screen.queryByText(NR_MACHINE.OS_LATER)).toBeNull();
  });
});

describe("OS row: macOS chosen (frame 2, M-N2 frame 1)", () => {
  it("writes macos_vm, drops the container barrier, says what is set up and starts the ask at the minimum", async () => {
    show({ ...AGENT, confinement_class: "CC3" }, ON, READY);
    await chooseVM();
    await userEvent.click(macosButton());
    expect(intentNow()).toMatchObject({ machine_kind: "macos_vm", resources: { cpu_millis: 4000, memory_mib: 8192 } });
    expect(intentNow().confinement_class).toBeUndefined();
    expect(macosButton()).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByText(NR_MACHINE.MACOS_ABOUT("macOS 27.0.1 (26A434)"))).toBeInTheDocument();
    expect(screen.getByText(NR_MACHINE.MACOS_SIZE("4", "8192"))).toBeInTheDocument();
    expect(screen.queryByText(NR_MACHINE.OS_WINDOWS_LATER)).toBeNull();
    const panel = within(changes());
    expect(panel.getByText(NR_MACHINE.CHANGES_NETWORK_BODY)).toBeInTheDocument();
    expect(panel.getByText(NR_MACHINE.CHANGES_CLIPBOARD_MACOS_NONE)).toBeInTheDocument();
    expect(panel.getByText(NR_MACHINE.CHANGES_SIGN_INS)).toBeInTheDocument();
    expect(panel.getByText(NR_MACHINE.CHANGES_SIGN_INS_RUN_DISK)).toBeInTheDocument();
    expect(panel.getByText(NR_MACHINE.CHANGES_MAC)).toBeInTheDocument();
    expect(panel.getByText(NR_MACHINE.CHANGES_MAC_MANAGED)).toBeInTheDocument();
  });

  it("leaves out the guest line when the pool's Macs hold different builds", async () => {
    show(AGENT, ON, { ...READY, guestOS: "" });
    await chooseVM();
    await userEvent.click(macosButton());
    expect(screen.getByText(NR_MACHINE.MACOS_SIZE("4", "8192"))).toBeInTheDocument();
    expect(screen.queryByText(/set up on the Mac itself/)).toBeNull();
  });

  it("keeps an ask already above the minimum, and takes back only an untouched pre-fill when Linux is chosen again", async () => {
    show({ ...AGENT, resources: { cpu_millis: 6000, memory_mib: 16384 } }, ON, READY);
    await chooseVM();
    await userEvent.click(macosButton());
    expect(intentNow().resources).toEqual({ cpu_millis: 6000, memory_mib: 16384 });
    await userEvent.click(within(osGroup()).getByRole("button", { name: NR_MACHINE.OS_LINUX }));
    expect(intentNow()).toMatchObject({ machine_kind: "linux_vm", resources: { cpu_millis: 6000, memory_mib: 16384 } });
    cleanup();

    show(AGENT, ON, READY);
    await chooseVM();
    await userEvent.click(macosButton());
    await userEvent.click(within(osGroup()).getByRole("button", { name: NR_MACHINE.OS_LINUX }));
    expect(intentNow().machine_kind).toBe("linux_vm");
    expect(intentNow().resources).toBeUndefined();
  });

  it("keeps CPU and memory the person set to exactly the minimum before choosing macOS when Linux is chosen again", async () => {
    show({ ...AGENT, resources: { cpu_millis: 4000, memory_mib: 8192 } }, ON, READY);
    await chooseVM();
    await userEvent.click(macosButton());
    expect(intentNow().resources).toEqual({ cpu_millis: 4000, memory_mib: 8192 });
    await userEvent.click(within(osGroup()).getByRole("button", { name: NR_MACHINE.OS_LINUX }));
    expect(intentNow()).toMatchObject({ machine_kind: "linux_vm", resources: { cpu_millis: 4000, memory_mib: 8192 } });
  });

  it("takes back the pre-fill when the pool stops offering macOS before Linux is chosen again", async () => {
    const REFUSAL: MacOSOffer = { ok: false, refusal: { why: "preparing", runner: "mac-studio-07", percent: 42 } };
    function Flip({ children }: { children: React.ReactNode }) {
      const [offer, setOffer] = React.useState<MacOSOffer>(READY);
      return (
        <MacOSOfferContext.Provider value={offer}>
          {children}
          <button onClick={() => setOffer(REFUSAL)}>flip</button>
        </MacOSOfferContext.Provider>
      );
    }
    render(
      <MemoryRouter>
        <ModelAccessProvider status={ON} onRefresh={() => {}}>
          <Flip>
            <NewRunDraftProvider owner="alice">
              <Seeded intent={AGENT}>
                <RunStep location={{ screen: "step", step: "run" }} go={vi.fn()} startFromScratch={() => {}} deps={{ load: vi.fn(async () => SCENARIO_POOLS.member) }} />
                <Probe />
              </Seeded>
            </NewRunDraftProvider>
          </Flip>
        </ModelAccessProvider>
      </MemoryRouter>,
    );
    await chooseVM();
    await userEvent.click(macosButton());
    expect(intentNow().resources).toEqual({ cpu_millis: 4000, memory_mib: 8192 });
    await userEvent.click(screen.getByRole("button", { name: "flip" }));
    await userEvent.click(within(osGroup()).getByRole("button", { name: NR_MACHINE.OS_LINUX }));
    expect(intentNow().machine_kind).toBe("linux_vm");
    expect(intentNow().resources).toBeUndefined();
  });

  it("says an agent that disallows desktop sign-ins stops the run (M-N2 frame 3)", async () => {
    // The setup status's own harness row says it: the draft's agent has "Sign-ins in a desktop: Not allowed".
    const status = { ...ON, harnesses: [{ id: "release-bot", display: "Release bot", has_gateway: false, has_login: false, desktop_sign_ins_off: true }] } as SetupStatus;
    show({ ...AGENT, tools: [{ kind: "harness", id: "release-bot" }] } as RunIntent, status, READY);
    await chooseVM();
    await userEvent.click(macosButton());
    const panel = within(changes());
    expect(panel.getByText(NR_MACHINE.CHANGES_SIGN_INS_OFF("Release bot"))).toBeInTheDocument();
    expect(panel.queryByText(NR_MACHINE.CHANGES_SIGN_INS_RUN_DISK)).toBeNull();
    cleanup();

    // Another agent of the same roster keeps the run-disk line.
    show({ ...AGENT, tools: [{ kind: "harness", id: "claude-code" }] } as RunIntent, status, READY);
    await chooseVM();
    await userEvent.click(macosButton());
    expect(within(changes()).getByText(NR_MACHINE.CHANGES_SIGN_INS_RUN_DISK)).toBeInTheDocument();
  });

  it("keeps the Linux panel as it is: no sign-ins or Mac row, and the Linux clipboard words (M-N2 frame 4)", async () => {
    show(AGENT, ON, READY);
    await chooseVM();
    const panel = within(changes());
    expect(panel.getByText(NR_MACHINE.CHANGES_CLIPBOARD_BODY)).toBeInTheDocument();
    expect(panel.queryByText(NR_MACHINE.CHANGES_SIGN_INS)).toBeNull();
    expect(panel.queryByText(NR_MACHINE.CHANGES_MAC)).toBeNull();
  });
});

const REFUSED: [frame: string, refusal: MacOSRefusal, line: string][] = [
  ["4", { why: "not_offered", runner: "mac-studio-07" }, NR_MACHINE.MACOS_NOT_OFFERED("mac-studio-07")],
  ["4b", { why: "pool_not_offered" }, NR_MACHINE.MACOS_POOL_NOT_OFFERED],
  ["5", { why: "preparing", runner: "mac-studio-07", percent: 42 }, NR_MACHINE.MACOS_PREPARING("mac-studio-07", 42)],
  ["6", { why: "at_limit", runner: "mac-studio-07" }, NR_MACHINE.MACOS_AT_LIMIT("mac-studio-07")],
  ["7b", { why: "not_managed", runner: "mac-studio-07" }, NR_MACHINE.MACOS_NOT_MANAGED_MEMBER("mac-studio-07")],
  ["8", { why: "no_posture", runner: "mac-studio-07" }, NR_MACHINE.MACOS_NO_POSTURE("mac-studio-07", "/Library/Application Support/Wardyn/posture.json")],
  ["8b", { why: "pool_cant_now" }, NR_MACHINE.MACOS_POOL_CANT_NOW],
  ["E03", { why: "no_base", runner: "mac-studio-07" }, NR_MACHINE.MACOS_NO_BASE("mac-studio-07")],
  ["E04", { why: "no_base", runner: "mac-studio-07", error: "disk full" }, NR_MACHINE.MACOS_NO_BASE_ERROR("mac-studio-07", "disk full")],
  ["E05", { why: "no_room", runner: "mac-studio-07" }, NR_MACHINE.MACOS_NO_ROOM("mac-studio-07")],
];

describe("OS row: macOS disabled with its reason (frames 4 to 8b)", () => {
  it.each(REFUSED)("frame %s: macOS cannot be chosen, the reason stands under the row, and Windows follows", async (_frame, refusal, text) => {
    show(AGENT, ON, { ok: false, refusal });
    await chooseVM();
    expect(macosButton()).toBeDisabled();
    expect(line(text)).toBeInTheDocument();
    expect(line(NR_MACHINE.OS_WINDOWS_LATER)).toBeInTheDocument();
    await userEvent.click(macosButton());
    expect(intentNow().machine_kind).toBe("linux_vm");
  });

  it("frame 7a: a security operator is told the command, drawn as code", async () => {
    show(AGENT, ON, { ok: false, refusal: { why: "not_managed", runner: "mac-studio-07", markId: "rnr_7f3a" } });
    await chooseVM();
    expect(line(NR_MACHINE.MACOS_NOT_MANAGED_OPERATOR("mac-studio-07", "rnr_7f3a"))).toBeInTheDocument();
    expect(within(osGroup()).getByText("wardyn runner trust rnr_7f3a managed_device").tagName).toBe("CODE");
  });
});
