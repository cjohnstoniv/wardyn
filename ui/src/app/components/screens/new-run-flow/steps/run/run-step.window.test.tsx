/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Run step's window on the Mac (board WIN-2, Run step frames): hidden while the site switch is off, the option and
// its hints, the check it asks for, a refusal that clears it, and the kinds that have none.

import * as React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { HttpError } from "../../../../../lib/api/core";
import { runs } from "../../../../../lib/api/runs";
import { SCENARIO_POOLS } from "../../../../../lib/new-run-fixtures/scenarios/pools";
import type { SetupStatus } from "../../../../../lib/types/setup";
import { NR_MACHINE } from "../../../../wardyn/copy/new-run/machine";
import { NR_REVIEW } from "../../../../wardyn/copy/new-run/review";
import { NR_RUN } from "../../../../wardyn/copy/new-run/run";
import { NR_WINDOW } from "../../../../wardyn/copy/new-run/window";
import { ModelAccessProvider } from "../../../../wardyn/model-access-context";
import { OperatorProvider } from "../../../../wardyn/operator-context";
import type { RunIntent } from "../../draft/run-intent";
import { NewRunDraftProvider, useNewRunDispatch, useNewRunDraft } from "../../new-run-route";
import { MacOSOfferContext, type MacOSOffer } from "./macos-offer";
import { RunStep } from "./run-step";

const SITE_OFF = "Opening a window onto a macOS VM run is turned off for this install. An admin can turn it on in Settings, under macOS virtual machines, or set macos_vm.allow_attended_window in the site config.";
const PROFILE = "Your organisation's governance profile doesn't allow an interactive desktop on this run.";
const SIGN_INS = "Your admin doesn't allow sign-ins in a desktop while Claude Code runs, and a macOS desktop can't block them yet.";
const NOT_YOURS = "Wardyn opens a window only on a Mac whose runner you registered. This run is on another computer, so Wardyn can't tell who is at its keyboard. Watch its desktop from the run page instead.";

const WINDOW_ON = { features: { macos_vms: true, mac_window: true } } as SetupStatus;
const WINDOW_OFF = { features: { macos_vms: true } } as SetupStatus;
const ONE_MAC: MacOSOffer = { ok: true, guestOS: "macOS 26.1", minCPUs: 4, minMemoryMiB: 8192, harnesses: ["none"], runner: "Maya's MacBook" };
const SEVERAL: MacOSOffer = { ...ONE_MAC, runner: undefined };
const MAC: RunIntent = { shape: "agent_task", experience: "interactive", tools: [{ kind: "harness", id: "claude-code" }], no_repositories_or_drives: true, machine_kind: "macos_vm" };

const preview = vi.fn();
beforeEach(() => {
  preview.mockReset();
  preview.mockResolvedValue({ spec: {}, source: { kind: "inline" }, warnings: [], pending: [], repository_access: [] });
  vi.spyOn(runs, "previewRunPolicyWire").mockImplementation(preview);
});
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

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

async function show(intent: RunIntent, status: SetupStatus | null, offer?: MacOSOffer) {
  render(
    <MemoryRouter>
      <OperatorProvider operator={false} principal="alice">
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
      </OperatorProvider>
    </MemoryRouter>,
  );
  await screen.findByRole("combobox", { name: NR_RUN.POOL_LABEL });
}
const option = () => screen.getByRole("checkbox", { name: NR_WINDOW.OPTION });
const refusal = (message: string, reason: string, field = "desktop_window") => new HttpError(403, message, reason, "", "", "", undefined, "", "", field);

describe("Run step: the window option, site switch off (the default)", () => {
  it("draws no Window group, and the view-only line names no window", async () => {
    for (const status of [WINDOW_OFF, { features: { macos_vms: true, mac_window: false } } as SetupStatus]) {
      await show(MAC, status, ONE_MAC);
      expect(screen.queryByRole("group", { name: NR_WINDOW.GROUP })).toBeNull();
      expect(screen.queryByRole("checkbox", { name: NR_WINDOW.OPTION })).toBeNull();
      expect(screen.getByText(NR_WINDOW.VIEW_ONLY_WINDOW_OFF)).toBeInTheDocument();
      expect(screen.queryByText(NR_WINDOW.VIEW_ONLY_WINDOW_ON)).toBeNull();
      cleanup();
    }
    expect(preview).not.toHaveBeenCalled();
  });
});

describe("Run step: the window option, site switch on", () => {
  it("unticked by default on one Mac: the group, the label and W2-03, and the view-only line names the window", async () => {
    await show(MAC, WINDOW_ON, ONE_MAC);
    expect(screen.getByRole("group", { name: NR_WINDOW.GROUP })).toBeInTheDocument();
    expect(option()).not.toBeChecked();
    expect(screen.getByText(NR_WINDOW.ONE_MAC("Maya's MacBook"))).toBeInTheDocument();
    expect(screen.queryByText(NR_WINDOW.BE_THERE("Maya's MacBook"))).toBeNull();
    expect(screen.getByText(NR_WINDOW.VIEW_ONLY_WINDOW_ON)).toBeInTheDocument();
    expect(intentNow().desktop_window).toBeUndefined();
  });

  it("ticked: the draft carries desktop_window at_start, W2-04 follows, and the preview is asked with it", async () => {
    let answer: (v: unknown) => void = () => {};
    preview.mockImplementation(() => new Promise((ok) => { answer = ok; }));
    await show(MAC, WINDOW_ON, ONE_MAC);
    await userEvent.click(option());
    expect(intentNow().desktop_window).toBe("at_start");
    expect(option()).toBeChecked();
    expect(screen.getByText(NR_WINDOW.BE_THERE("Maya's MacBook"))).toBeInTheDocument();
    expect(screen.getByText(NR_REVIEW.CHECKING)).toBeInTheDocument();
    await waitFor(() => expect(preview).toHaveBeenCalled(), { timeout: 3000 });
    expect(preview.mock.calls[0][0]).toMatchObject({ machine_kind: "macos_vm", desktop_window: "at_start" });
    answer({ spec: {}, source: { kind: "inline" }, warnings: [], pending: [], repository_access: [] });
    await waitFor(() => expect(screen.queryByText(NR_REVIEW.CHECKING)).toBeNull());
    expect(intentNow().desktop_window).toBe("at_start");
  });

  it("a pool of several Macs: W2-05, and no line naming a Mac", async () => {
    await show(MAC, WINDOW_ON, SEVERAL);
    expect(screen.getByText(NR_WINDOW.SEVERAL_MACS)).toBeInTheDocument();
    await userEvent.click(option());
    expect(screen.queryByText(/Be at /)).toBeNull();
  });

  it("keyboard focus reaches the option, and Space ticks it", async () => {
    await show(MAC, WINDOW_ON, ONE_MAC);
    option().focus();
    expect(option()).toHaveFocus();
    await userEvent.keyboard(" ");
    expect(intentNow().desktop_window).toBe("at_start");
  });

  it.each([
    ["the site switch turned off since the status loaded", SITE_OFF, "mac_window_site_off"],
    ["governance", PROFILE, "governance_unit"],
    ["agent sign-ins not allowed", SIGN_INS, "desktop_sign_ins_off"],
    ["who asks", NOT_YOURS, "mac_window_not_your_mac"],
  ])("a refusal naming desktop_window (%s) clears the choice with W2-06 and the server's sentence", async (_name, message, reason) => {
    preview.mockRejectedValue(refusal(message, reason));
    await show(MAC, WINDOW_ON, ONE_MAC);
    await userEvent.click(option());
    await waitFor(() => expect(intentNow().desktop_window).toBeUndefined(), { timeout: 3000 });
    expect(option()).not.toBeChecked();
    expect(screen.getByText(NR_WINDOW.CLEARED)).toBeInTheDocument();
    expect(screen.getByText(message)).toBeInTheDocument();
    // Ticked again: the notice goes, the choice is back, and the server is asked again.
    preview.mockResolvedValue({ spec: {}, source: { kind: "inline" }, warnings: [], pending: [], repository_access: [] });
    await userEvent.click(option());
    expect(screen.queryByText(NR_WINDOW.CLEARED)).toBeNull();
    expect(intentNow().desktop_window).toBe("at_start");
  });

  it("a refusal that shares a reason but names no field never clears the window", async () => {
    preview.mockRejectedValue(refusal(PROFILE, "governance_unit", ""));
    await show(MAC, WINDOW_ON, ONE_MAC);
    await userEvent.click(option());
    await waitFor(() => expect(preview).toHaveBeenCalled(), { timeout: 3000 });
    await waitFor(() => expect(screen.queryByText(NR_REVIEW.CHECKING)).toBeNull());
    expect(intentNow().desktop_window).toBe("at_start");
    expect(screen.queryByText(NR_WINDOW.CLEARED)).toBeNull();
  });

  it("leaving macOS takes the window with it", async () => {
    await show(MAC, WINDOW_ON, ONE_MAC);
    await userEvent.click(option());
    await userEvent.click(screen.getByRole("button", { name: NR_MACHINE.OS_LINUX }));
    expect(intentNow().desktop_window).toBeUndefined();
    expect(screen.queryByRole("group", { name: NR_WINDOW.GROUP })).toBeNull();
  });

  it("a Linux VM and a container have no Window group", async () => {
    await show({ ...MAC, machine_kind: "linux_vm" }, WINDOW_ON, ONE_MAC);
    expect(screen.queryByRole("group", { name: NR_WINDOW.GROUP })).toBeNull();
    expect(screen.getByText(NR_WINDOW.OS_WINDOWS_LATER)).toBeInTheDocument();
    cleanup();
    await show({ ...MAC, machine_kind: undefined }, WINDOW_ON, ONE_MAC);
    expect(screen.queryByRole("group", { name: NR_WINDOW.GROUP })).toBeNull();
    expect(screen.queryByText(NR_WINDOW.OPTION)).toBeNull();
  });
});

describe("Run step: the Windows lines", () => {
  it("say Windows isn't available yet, and with macOS off for the organisation say that too", async () => {
    await show({ ...MAC, machine_kind: "linux_vm" }, WINDOW_ON, ONE_MAC);
    expect(screen.getByText(NR_WINDOW.OS_WINDOWS_LATER)).toBeInTheDocument();
    cleanup();
    await show({ ...MAC, machine_kind: "linux_vm" }, { features: { macos_vms: false } } as SetupStatus);
    expect(screen.getByText(NR_WINDOW.OS_LATER)).toBeInTheDocument();
  });
});
