/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { DefaultPolicyContext } from "../../../../../lib/api/policies";
import type { ElevationOffer, GovernanceRequest, GovernanceRequestList } from "../../../../../lib/types/governance-requests";
import { NR_RUN_IN } from "../../../../wardyn/copy/new-run/run-in";
import * as RL from "../../../../wardyn/copy/run-lifetime";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { weekdayClock } from "../../../run-detail/run-ends-row";
import type { DraftAction } from "../../draft/draft-reducer";
import type { RunIntent } from "../../draft/run-intent";
import { RunInField } from "./run-in-field";
import { runInChoices } from "./run-in-model";
import type { RunInRead } from "./use-run-in";

afterEach(cleanup);

const NOW = Date.now();
const ENDS = new Date(NOW + 190 * 60_000).toISOString();

const PLATFORM: DefaultPolicyContext = { unit_id: "u-plat", unit_name: "Platform", home: true };
const DATA: DefaultPolicyContext = { unit_id: "u-data", unit_name: "Data science", home: false };
const PAYMENTS: DefaultPolicyContext = { unit_id: "u-pay", unit_name: "Payments", home: false };
const BILLING: ElevationOffer = { unit_id: "u-bill", unit_name: "Billing", max_elevation: "4h" };

const elevation = {
  id: "c1", state: "applied", kind: "elevation", unit_id: "u-pay", unit_name: "Payments", duration: "4h", ends_at: ENDS,
  mine: true, decidable: false, deciders: [], expires_at: ENDS,
} as unknown as GovernanceRequest;
const LIST: GovernanceRequestList = { requests: [elevation], offered: [BILLING], image_offered: false };

function read(over: Partial<RunInRead> = {}): RunInRead {
  return { status: "ready", choices: runInChoices([PLATFORM, DATA, PAYMENTS], LIST, NOW), list: LIST, now: NOW, reload: vi.fn(), ...over };
}

function setup(props: { run?: RunInRead; runIn?: string; notice?: string; homeFallbackName?: string } = {}) {
  const dispatch = vi.fn<(a: DraftAction) => void>();
  const utils = render(<RunInField run={props.run ?? read()} runIn={props.runIn} notice={props.notice} homeFallbackName={props.homeFallbackName} dispatch={dispatch} />);
  /** The intent after every edit dispatched so far, from one that already holds `runIn`. */
  const edited = (from: RunIntent = props.runIn ? { run_in: props.runIn } : {}) => dispatch.mock.calls.reduce((i, [a]) => (a.type === "edit" ? a.apply(i) : i), from);
  return { dispatch, edited, ...utils };
}

const trigger = () => screen.getByRole("combobox", { name: NR_RUN_IN.LABEL });

describe("Run in field: states", () => {
  it("shows the home unit chosen with the hint", () => {
    setup();
    expect(screen.getByText(NR_RUN_IN.QUESTION)).toBeInTheDocument();
    expect(screen.getByText(NR_SHARED.OPTIONAL)).toBeInTheDocument();
    expect(trigger()).toHaveTextContent("Platform (home)");
    expect(screen.getByText(NR_RUN_IN.HINT)).toBeInTheDocument();
  });

  it("opens on your units, the elevation with its end and time left, and the units you can ask for", async () => {
    setup();
    await userEvent.click(trigger());
    // The Elevated group's heading and its chip read the same, so the headings are found by their own slot.
    const headings = [...document.querySelectorAll('[data-slot="select-label"]')].map((h) => h.textContent);
    expect(headings).toEqual([NR_RUN_IN.GROUP_YOURS, NR_RUN_IN.GROUP_ELEVATED, NR_RUN_IN.GROUP_ASK]);
    const home = screen.getByRole("option", { name: /^Platform/ });
    expect(within(home).getByText(NR_RUN_IN.CHIP_HOME)).toBeInTheDocument();
    const paid = screen.getByRole("option", { name: /^Payments/ });
    expect(within(paid).getByText(NR_RUN_IN.CHIP_ELEVATED)).toBeInTheDocument();
    expect(within(paid).getByText(NR_RUN_IN.ELEVATED_LINE(weekdayClock(ENDS), "3 h 10 min"))).toBeInTheDocument();
    expect(NR_RUN_IN.ELEVATED_LINE("Sun 18:40", "3 h 10 min")).toBe("Ends Sun 18:40 (in 3 h 10 min)");
    const bill = screen.getByRole("option", { name: /^Billing/ });
    expect(within(bill).getByText(NR_RUN_IN.CHIP_NEEDS)).toBeInTheDocument();
    expect(within(bill).getByText(NR_RUN_IN.ASK_LINE("4 hours"))).toBeInTheDocument();
    expect(within(screen.getByRole("option", { name: /^Data science/ })).queryByText(NR_RUN_IN.CHIP_HOME)).toBeNull();
  });

  it("shows an elevated pick with the Elevated chip and no hint", () => {
    setup({ runIn: "u-pay" });
    expect(trigger()).toHaveTextContent("Payments");
    expect(trigger()).not.toHaveTextContent("(home)");
    expect(screen.getByText(NR_RUN_IN.CHIP_ELEVATED)).toBeInTheDocument();
    expect(screen.queryByText(NR_RUN_IN.HINT)).toBeNull();
  });

  it("says the run ends when the elevation ends, under an elevated pick only", () => {
    setup({ runIn: "u-pay" });
    expect(screen.getByText(RL.endsElevation(weekdayClock(ENDS)))).toBeInTheDocument();
    cleanup();
    setup({ runIn: "u-data" });
    expect(screen.queryByText(/This run ends when/)).toBeNull();
    cleanup();
    setup();
    expect(screen.queryByText(/This run ends when/)).toBeNull();
  });

  it("hides the hint once any unit but the home unit is picked", () => {
    setup({ runIn: "u-data" });
    expect(trigger()).toHaveTextContent("Data science");
    expect(screen.queryByText(NR_RUN_IN.HINT)).toBeNull();
  });

  it("is disabled and says it is loading when the draft already carries a unit", () => {
    setup({ run: { status: "loading", now: NOW, reload: vi.fn() }, runIn: "u-pay" });
    expect(trigger()).toBeDisabled();
    expect(trigger()).toHaveTextContent(NR_RUN_IN.LOADING);
    expect(screen.getByText(NR_RUN_IN.HINT)).toBeInTheDocument();
    expect(NR_RUN_IN.LOADING).toBe("Loading your units…");
  });

  it("is not drawn while loading with no unit in the draft", () => {
    const { container } = setup({ run: { status: "loading", now: NOW, reload: vi.fn() } });
    expect(container).toBeEmptyDOMElement();
  });

  it("is not drawn for one unit and no offers", () => {
    const list: GovernanceRequestList = { requests: [], offered: [], image_offered: false };
    const { container } = setup({ run: read({ choices: runInChoices([PLATFORM], list, NOW), list }) });
    expect(container).toBeEmptyDOMElement();
  });

  it("is not drawn for a server that sends no contexts", () => {
    const { container } = setup({ run: read({ choices: undefined }) });
    expect(container).toBeEmptyDOMElement();
  });

  it("says the units could not be read, with the home unit still chosen", () => {
    setup({ run: { status: "failed", now: NOW, reload: vi.fn() }, homeFallbackName: "Platform" });
    expect(trigger()).toBeDisabled();
    expect(trigger()).toHaveTextContent("Platform (home)");
    expect(screen.getByText(NR_RUN_IN.HINT)).toBeInTheDocument();
    expect(screen.getByText(NR_RUN_IN.UNITS_UNREADABLE)).toBeInTheDocument();
  });

  it("is not drawn when the units could not be read and no home unit is known", () => {
    const { container } = setup({ run: { status: "failed", now: NOW, reload: vi.fn() } });
    expect(container).toBeEmptyDOMElement();
  });

  it("shows the notice that an elevation ended, with the home unit chosen", () => {
    const notice = NR_RUN_IN.ENDED("Payments", "Platform");
    setup({ notice });
    expect(screen.getByText(notice)).toBeInTheDocument();
    expect(trigger()).toHaveTextContent("Platform (home)");
    expect(screen.getByText(NR_RUN_IN.HINT)).toBeInTheDocument();
  });

  it("shows the notice that the person is no longer in a unit, even when no other unit is offered", () => {
    const notice = NR_RUN_IN.NO_LONGER_IN("Data science", "Platform");
    const list: GovernanceRequestList = { requests: [], offered: [], image_offered: false };
    setup({ notice, run: read({ choices: runInChoices([PLATFORM], list, NOW), list }) });
    expect(screen.getByText(notice)).toBeInTheDocument();
  });
});

describe("Run in field: choosing", () => {
  it("takes the choice out of the intent when the home unit is chosen", async () => {
    const { edited } = setup({ runIn: "u-pay" });
    await userEvent.click(trigger());
    await userEvent.click(screen.getByRole("option", { name: /^Platform/ }));
    const next = edited({ shape: "agent_task", run_in: "u-pay" });
    expect(next).toEqual({ shape: "agent_task" });
    expect("run_in" in next).toBe(false);
  });

  it("writes the unit's id when another unit is chosen", async () => {
    const { edited } = setup();
    await userEvent.click(trigger());
    await userEvent.click(screen.getByRole("option", { name: /^Payments/ }));
    expect(edited({ shape: "agent_task" }).run_in).toBe("u-pay");
  });

  it("lets the person pick a unit they can only ask for", async () => {
    const { edited } = setup();
    await userEvent.click(trigger());
    await userEvent.click(screen.getByRole("option", { name: /^Billing/ }));
    expect(edited({}).run_in).toBe("u-bill");
  });

  it("keeps the picked unit's name, so a notice can name it once the units stop listing it", async () => {
    const { dispatch } = setup();
    await userEvent.click(trigger());
    await userEvent.click(screen.getByRole("option", { name: /^Data science/ }));
    expect(dispatch).toHaveBeenCalledWith({ type: "guided", patch: { runInPicked: { unitId: "u-data", name: "Data science" } } });
  });

  it("ends the notice when the person picks a unit", async () => {
    const { dispatch } = setup({ notice: NR_RUN_IN.ENDED("Payments", "Platform") });
    await userEvent.click(trigger());
    await userEvent.click(screen.getByRole("option", { name: /^Data science/ }));
    expect(dispatch).toHaveBeenCalledWith({ type: "guided", patch: { runInCleared: undefined } });
  });
});
