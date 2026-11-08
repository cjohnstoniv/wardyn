/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "Who may use it" naming a unit: the type Select gains Unit, the name box becomes a Select of profiles, and a
// saved unit row prints the profile's name. The new strings are written out, not read from the copy module, so a
// drift in copy/pool-unit.ts fails this suite. A read that is still pending is a promise the test settles.
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { RunnerPoolUsePolicy } from "../../../../lib/types/runner-pools";

const usePolicyMock = vi.fn();
const setUsePolicyMock = vi.fn();
vi.mock("../../../../lib/api/runner-pools", () => ({
  runnerPools: {
    usePolicy: (...a: unknown[]) => usePolicyMock(...a),
    setUsePolicy: (...a: unknown[]) => setUsePolicyMock(...a),
    clearUsePolicy: vi.fn(),
  },
}));
const getGovernanceMock = vi.fn();
vi.mock("../../../../lib/api/governance", async () => {
  const actual = await vi.importActual<typeof import("../../../../lib/api/governance")>("../../../../lib/api/governance");
  return { ...actual, governance: { ...actual.governance, getGovernance: (...a: unknown[]) => getGovernanceMock(...a) } };
});
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

import type { GovernanceSnapshot, GovernanceUnit } from "../../../../lib/api/governance";
import { DEFAULT_GOVERNANCE_UNIT_ID } from "../../../../lib/types/governance";
import { aheadByHours } from "../../../../lib/test-clock";
import { setField } from "../../../../../test/set-field";
import { PoolPolicy } from "./pool-policy";

const PLATFORM = "c3a8f1d2-4b6e-4a70-9d15-8e2b7f0c6a39";
const BUILD = "d4b9e2a3-5c7f-4b81-8e26-9f3c8a1d7b40";
const GONE = "e5a9c1d3-6f28-47b0-8d14-3b7a0e9c5f62";

const unit = (id: string, name: string): GovernanceUnit => ({
  id,
  name,
  ceiling: {},
  limits: {},
  created_at: aheadByHours(-1),
  updated_at: aheadByHours(-1),
});
const SNAPSHOT = {
  profiles: [unit(DEFAULT_GOVERNANCE_UNIT_ID, "Default"), unit(PLATFORM, "Platform A"), unit(BUILD, "Platform A / build")],
  assignments: [],
} as GovernanceSnapshot;

const policy = (subjects: RunnerPoolUsePolicy["subjects"]): RunnerPoolUsePolicy => ({
  pool_id: "p-ci",
  subjects,
  revision: 3,
  updated_at: "2026-10-11T10:00:00Z",
});

function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

const renderPolicy = () => render(<PoolPolicy poolId="p-ci" poolName="CI jobs" onChanged={vi.fn()} />);
const edit = async () => userEvent.click(await screen.findByRole("button", { name: "Edit who may use it" }));
const chooseUnitType = async () => {
  await userEvent.click(screen.getByRole("combobox", { name: "Add" }));
  await userEvent.click(await screen.findByRole("option", { name: "Unit" }));
};

beforeEach(() => {
  for (const m of [usePolicyMock, setUsePolicyMock, getGovernanceMock]) m.mockReset();
  usePolicyMock.mockResolvedValue(policy([{ subject_type: "user", subject: "lee@example.com" }]));
  getGovernanceMock.mockResolvedValue(SNAPSHOT);
  setUsePolicyMock.mockResolvedValue(policy([]));
});
afterEach(cleanup);

describe("the pool policy's unit picker", () => {
  it("offers Unit as a type, and swaps the name box for a Select of profiles without Default (frame E1)", async () => {
    renderPolicy();
    await edit();
    expect(getGovernanceMock).not.toHaveBeenCalled();
    await chooseUnitType();
    expect(screen.queryByLabelText("Name")).toBeNull();
    const picker = await screen.findByRole("combobox", { name: "Unit" });
    await waitFor(() => expect(picker).toBeEnabled());
    expect(screen.getByText("A profile is a governance unit.")).toBeInTheDocument();
    await userEvent.click(picker);
    const options = await screen.findAllByRole("option");
    expect(options.map((o) => o.textContent)).toEqual(["Platform A", "Platform A / build"]);
    expect(getGovernanceMock).toHaveBeenCalledTimes(1);
  });

  it("is loading, disabled, while the profiles are read (frame E3)", async () => {
    const read = deferred<GovernanceSnapshot>();
    getGovernanceMock.mockReturnValue(read.promise);
    renderPolicy();
    await edit();
    await chooseUnitType();
    expect(screen.getByText("Loading profiles…")).toBeInTheDocument();
    expect(screen.getByRole("combobox", { name: "Unit" })).toBeDisabled();
    await act(async () => read.resolve(SNAPSHOT));
    await waitFor(() => expect(screen.getByRole("combobox", { name: "Unit" })).toBeEnabled());
    expect(screen.queryByText("Loading profiles…")).toBeNull();
  });

  it("says the read failed, offers Try again in the Add button's slot, and reads again", async () => {
    const first = deferred<GovernanceSnapshot>();
    const second = deferred<GovernanceSnapshot>();
    getGovernanceMock.mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise);
    renderPolicy();
    await edit();
    await chooseUnitType();
    await act(async () => first.reject(new Error("offline")));
    expect(await screen.findByText("Couldn't load profiles.")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Add to list" })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(getGovernanceMock).toHaveBeenCalledTimes(2);
    await act(async () => second.resolve(SNAPSHOT));
    await waitFor(() => expect(screen.getByRole("combobox", { name: "Unit" })).toBeEnabled());
    expect(screen.queryByText("Couldn't load profiles.")).toBeNull();
    expect(screen.getByRole("button", { name: "Add to list" })).toBeInTheDocument();
  });

  it("adds the chosen profile as a unit subject by its uuid, shows its name, and saves it", async () => {
    renderPolicy();
    await edit();
    await chooseUnitType();
    const picker = await screen.findByRole("combobox", { name: "Unit" });
    await waitFor(() => expect(picker).toBeEnabled());
    await userEvent.click(picker);
    await userEvent.click(await screen.findByRole("option", { name: "Platform A / build" }));
    await userEvent.click(screen.getByRole("button", { name: "Add to list" }));
    const list = screen.getAllByRole("list")[0];
    const added = within(list).getAllByRole("listitem")[1];
    expect(added).toHaveTextContent("Unit");
    expect(added).toHaveTextContent("Platform A / build");
    expect(added).toHaveTextContent("and the units below it");
    expect(added).not.toHaveTextContent(BUILD);
    await userEvent.click(screen.getByRole("button", { name: "Save who may use it" }));
    await waitFor(() =>
      expect(setUsePolicyMock).toHaveBeenCalledWith("p-ci", 3, [
        { subject_type: "user", subject: "lee@example.com" },
        { subject_type: "unit", subject: BUILD },
      ]),
    );
  });

  it("refuses a unit listed twice, by its name, and an empty add with the new sentence", async () => {
    usePolicyMock.mockResolvedValue(policy([{ subject_type: "unit", subject: PLATFORM }]));
    renderPolicy();
    await edit();
    await chooseUnitType();
    await userEvent.click(screen.getByRole("button", { name: "Add to list" }));
    expect(screen.getByRole("alert")).toHaveTextContent("Name a person, group, role or unit to add.");
    const picker = screen.getByRole("combobox", { name: "Unit" });
    await waitFor(() => expect(picker).toBeEnabled());
    await userEvent.click(picker);
    await userEvent.click(await screen.findByRole("option", { name: "Platform A" }));
    await userEvent.click(screen.getByRole("button", { name: "Add to list" }));
    expect(screen.getByRole("alert")).toHaveTextContent('unit "Platform A" is listed twice');
  });

  it("the empty-name error names a unit for the free-text types too", async () => {
    renderPolicy();
    await edit();
    await userEvent.click(screen.getByRole("button", { name: "Add to list" }));
    expect(screen.getByRole("alert")).toHaveTextContent("Name a person, group, role or unit to add.");
    setField(screen.getByLabelText("Name"), "lee@example.com");
  });
});

describe("a saved unit row", () => {
  it("prints the profile's name and 'and the units below it' without editing (frame E2)", async () => {
    usePolicyMock.mockResolvedValue(policy([{ subject_type: "unit", subject: PLATFORM }]));
    renderPolicy();
    expect(await screen.findByText("Platform A")).toBeInTheDocument();
    expect(screen.getByText("and the units below it")).toBeInTheDocument();
    expect(screen.queryByText(PLATFORM)).toBeNull();
    expect(getGovernanceMock).toHaveBeenCalledTimes(1);
  });

  it("prints the uuid alone until the read answers", async () => {
    const read = deferred<GovernanceSnapshot>();
    getGovernanceMock.mockReturnValue(read.promise);
    usePolicyMock.mockResolvedValue(policy([{ subject_type: "unit", subject: PLATFORM }]));
    renderPolicy();
    expect(await screen.findByText(PLATFORM)).toBeInTheDocument();
    expect(screen.queryByText("and the units below it")).toBeNull();
    expect(screen.queryByText("No longer a profile. It admits no one.")).toBeNull();
    await act(async () => read.resolve(SNAPSHOT));
    expect(await screen.findByText("and the units below it")).toBeInTheDocument();
    expect(screen.queryByText(PLATFORM)).toBeNull();
  });

  it("prints the uuid in mono and says a deleted profile admits no one (frame E3)", async () => {
    usePolicyMock.mockResolvedValue(policy([{ subject_type: "unit", subject: GONE }]));
    renderPolicy();
    expect(await screen.findByText("No longer a profile. It admits no one.")).toBeInTheDocument();
    expect(screen.getByText(GONE)).toBeInTheDocument();
    expect(screen.queryByText("and the units below it")).toBeNull();
  });

  it("reads no profiles when the list holds no unit", async () => {
    renderPolicy();
    expect(await screen.findByText("lee@example.com")).toBeInTheDocument();
    expect(getGovernanceMock).not.toHaveBeenCalled();
  });
});
