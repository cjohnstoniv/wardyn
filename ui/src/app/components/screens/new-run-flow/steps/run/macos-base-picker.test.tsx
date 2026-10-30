/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The New Run macOS base picker (board J6, "Picker · several bases", "open, with reasons", "Only one base · no picker").
import * as React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { SCENARIO_POOLS } from "../../../../../lib/new-run-fixtures/scenarios/pools";
import { shortDate } from "../../../../../lib/format";
import { aheadByHours } from "../../../../../lib/test-clock";
import type { SetupStatus } from "../../../../../lib/types/setup";
import type { PoolMacOSBase } from "../../../../../lib/types/runner-pools";
import { MAC_BASES } from "../../../../wardyn/copy/mac-bases";
import { NR_MACHINE } from "../../../../wardyn/copy/new-run/machine";
import { NR_RUN } from "../../../../wardyn/copy/new-run/run";
import { ModelAccessProvider } from "../../../../wardyn/model-access-context";
import { intentToWire } from "../../draft/intent-to-wire";
import type { RunIntent } from "../../draft/run-intent";
import { NewRunDraftProvider, useNewRunDispatch, useNewRunDraft } from "../../new-run-route";
import { MacOSOfferContext, type MacOSOffer } from "./macos-offer";
import { RunStep } from "./run-step";

afterEach(cleanup);

const STATUS = { features: { macos_vms: true }, harnesses: [{ id: "claude-code", display: "Claude Code" }, { id: "none", display: "Your own tools" }] } as SetupStatus;
const MAC: RunIntent = { shape: "agent_task", experience: "interactive", machine_kind: "macos_vm" };
const STANDARD_AT = aheadByHours(-72);
const XCODE_AT = aheadByHours(-70);
const BASES: PoolMacOSBase[] = [
  { name: "xcode-16", state: "ready", guest_os: "macOS 26.6.2", harnesses: ["claude-code", "none"], created_at: XCODE_AT },
  { name: "ios-sim", state: "missing", guest_os: "macOS 26.6.2", harnesses: ["none"], created_at: XCODE_AT },
  { name: "qa-profiles", state: "rebuilding", progress: 60, guest_os: "macOS 26.6.2", harnesses: ["claude-code"], created_at: STANDARD_AT },
];
const offer = (bases?: PoolMacOSBase[], over: Partial<Extract<MacOSOffer, { ok: true }>> = {}): MacOSOffer =>
  ({ ok: true, guestOS: "macOS 26.6.2 (25G83)", minCPUs: 4, minMemoryMiB: 8192, harnesses: ["none", "claude-code"], bases, createdAt: STANDARD_AT, profile: "Platform", ...over });

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

async function show(intent: RunIntent, o: MacOSOffer | undefined) {
  render(
    <MemoryRouter>
      <ModelAccessProvider status={STATUS} onRefresh={() => {}}>
        <MacOSOfferContext.Provider value={o}>
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
  await screen.findByRole("combobox", { name: NR_RUN.POOL_LABEL });
}
const trigger = () => screen.getByRole("button", { name: MAC_BASES.B01 });
const open = async () => {
  await userEvent.click(trigger());
  return screen.findByRole("radiogroup", { name: MAC_BASES.B01 });
};
const radio = (name: string) => screen.getByRole("radio", { name });

describe("picker · several bases (J6 closed)", () => {
  it("shows MAC-B01, Standard, and MAC-B02 under the OS row when the pool offers more than one base", async () => {
    await show(MAC, offer(BASES));
    expect(trigger()).toHaveTextContent(MAC_BASES.B04);
    expect(screen.getByText(MAC_BASES.B02)).toBeInTheDocument();
    expect(intentNow().macos_base).toBeUndefined();
  });
});

describe("picker · open, with reasons (J6 open)", () => {
  it("lists Standard then each base with its MAC-B03 line, and why a base cannot be picked", async () => {
    await show(MAC, offer(BASES));
    const group = await open();
    const std = MAC_BASES.B03("macOS 26.6.2 (25G83)", shortDate(STANDARD_AT), "Claude Code, Your own tools");
    expect(within(group).getByText(std)).toBeInTheDocument();
    expect(within(group).getByText(MAC_BASES.B03("macOS 26.6.2", shortDate(XCODE_AT), "Claude Code, Your own tools"))).toBeInTheDocument();
    expect(within(group).getByText(MAC_BASES.B06)).toBeInTheDocument();
    expect(within(group).getByText(MAC_BASES.B07(60))).toBeInTheDocument();
    expect(radio(MAC_BASES.B04)).toBeEnabled();
    expect(radio("xcode-16")).toBeEnabled();
    expect(radio("ios-sim")).toBeDisabled();
    expect(radio("qa-profiles")).toBeDisabled();
  });

  it("picks a ready base, sends it on the wire, and picking Standard again omits it", async () => {
    await show(MAC, offer(BASES));
    await open();
    await userEvent.click(radio("xcode-16"));
    expect(intentNow().macos_base).toBe("xcode-16");
    expect(trigger()).toHaveTextContent("xcode-16");
    expect(intentToWire(intentNow()).body).toMatchObject({ machine_kind: "macos_vm", macos_base: "xcode-16" });
    await open();
    await userEvent.click(radio(MAC_BASES.B04));
    expect(intentNow().macos_base).toBeUndefined();
    expect(intentToWire(intentNow()).body).not.toHaveProperty("macos_base");
  });

  it("a base that is not ready cannot be picked", async () => {
    await show(MAC, offer(BASES));
    await open();
    await userEvent.click(radio("qa-profiles"));
    expect(intentNow().macos_base).toBeUndefined();
  });
});

// Board J6 "denied": the caller's own answer per base, with the profile's name from the same list (MAC-B05).
describe("picker · not allowed by the profile (J6 open, denied)", () => {
  it("greys a ready base the profile does not allow, with MAC-B05 ahead of any other reason, and cannot pick it", async () => {
    await show(MAC, offer([...BASES, { name: "security-tools", state: "ready", not_allowed: true }, { name: "ios-sim-2", state: "rebuilding", progress: 10, not_allowed: true }]));
    const group = await open();
    expect(within(group).getAllByText(MAC_BASES.B05("Platform"))).toHaveLength(2);
    expect(within(group).queryByText(MAC_BASES.B07(10))).toBeNull();
    expect(radio("security-tools")).toBeDisabled();
    await userEvent.click(radio("security-tools"));
    expect(intentNow().macos_base).toBeUndefined();
  });

  it("disables Standard when the allow list leaves it off, and still lets the person pick an allowed base", async () => {
    await show(MAC, offer([{ name: "xcode-16", state: "ready" }], { standardNotAllowed: true }));
    await open();
    expect(radio(MAC_BASES.B04)).toBeDisabled();
    expect(within(screen.getByRole("radiogroup", { name: MAC_BASES.B01 })).getByText(MAC_BASES.B05("Platform"))).toBeInTheDocument();
    await userEvent.click(radio("xcode-16"));
    expect(intentNow().macos_base).toBe("xcode-16");
  });

  it("offers the picker for a pool with only Standard when the profile leaves Standard off", async () => {
    await show(MAC, offer([], { standardNotAllowed: true }));
    expect(trigger()).toBeInTheDocument();
  });
});

describe("picker · keyboard", () => {
  it("an arrow key moves focus and does not pick or close; Space picks and closes", async () => {
    await show(MAC, offer([{ name: "xcode-16", state: "ready" }, { name: "lint", state: "ready" }]));
    await open();
    act(() => radio(MAC_BASES.B04).focus());
    await userEvent.keyboard("{ArrowDown}");
    expect(screen.getByRole("radiogroup", { name: MAC_BASES.B01 })).toBeInTheDocument();
    await vi.waitFor(() => expect(radio("xcode-16")).toHaveFocus());
    await userEvent.keyboard("{ArrowDown}");
    expect(screen.getByRole("radiogroup", { name: MAC_BASES.B01 })).toBeInTheDocument();
    await vi.waitFor(() => expect(radio("lint")).toHaveFocus());
    await userEvent.keyboard(" ");
    expect(intentNow().macos_base).toBe("lint");
    await vi.waitFor(() => expect(screen.queryByRole("radiogroup", { name: MAC_BASES.B01 })).toBeNull());
  });

  it("Enter on a base picks it and closes the menu", async () => {
    await show(MAC, offer([{ name: "xcode-16", state: "ready" }]));
    await open();
    act(() => radio("xcode-16").focus());
    await userEvent.keyboard("{Enter}");
    expect(intentNow().macos_base).toBe("xcode-16");
    await vi.waitFor(() => expect(screen.queryByRole("radiogroup", { name: MAC_BASES.B01 })).toBeNull());
  });
});

describe("picker · only one base (J6 one)", () => {
  it("shows no picker for a pool with only Standard, and none for a pool that says nothing about bases", async () => {
    await show(MAC, offer([]));
    expect(screen.queryByRole("button", { name: MAC_BASES.B01 })).toBeNull();
    cleanup();
    await show(MAC, offer(undefined));
    expect(screen.queryByText(MAC_BASES.B02)).toBeNull();
  });

  it("shows no picker on a container or a Linux VM", async () => {
    await show({ ...MAC, machine_kind: "linux_vm" }, offer(BASES));
    expect(screen.queryByRole("button", { name: MAC_BASES.B01 })).toBeNull();
  });
});

describe("a base the pool does not list", () => {
  it("keeps the picker, names the draft's base as not on every Mac, and lets the person pick another", async () => {
    await show({ ...MAC, macos_base: "xcode-16" }, offer([]));
    expect(trigger()).toHaveTextContent("xcode-16");
    await open();
    expect(screen.getByText(MAC_BASES.B06)).toBeInTheDocument();
    await userEvent.click(radio(MAC_BASES.B04));
    expect(intentNow().macos_base).toBeUndefined();
  });

  it("drops the base with the machine kind", async () => {
    await show({ ...MAC, macos_base: "xcode-16" }, offer(BASES));
    await userEvent.click(screen.getByRole("button", { name: NR_MACHINE.OS_LINUX }));
    expect(intentNow()).toMatchObject({ machine_kind: "linux_vm" });
    expect(intentNow().macos_base).toBeUndefined();
  });
});
