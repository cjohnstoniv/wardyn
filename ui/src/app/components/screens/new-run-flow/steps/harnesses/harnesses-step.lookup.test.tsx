/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The status client answers a failed /setup/status read with its unreachable fallback, which has no harnesses.
// That is a failed read: the step says it couldn't read them, with Try again, instead of an empty roster that
// leaves Add a harness disabled with no reason.
import * as React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { NR_HARNESSES, NR_HARNESSES_LOOKUP } from "../../../../wardyn/copy/new-run/harnesses";
import { SCENARIO_HARNESSES, SCENARIO_HARNESS_SETS } from "../../../../../lib/new-run-fixtures/scenarios/harnesses";
import { NewRunDraftProvider, useNewRunDispatch } from "../../new-run-route";
import type { RunIntent } from "../../draft/run-intent";
import { HarnessesStep } from "./harnesses-step";
import { loadRoster, type Roster } from "./roster";

const ROSTER: Roster = { harnesses: SCENARIO_HARNESSES.full, sets: SCENARIO_HARNESS_SETS.full };
const ENVIRONMENT: RunIntent = { shape: "environment", experience: "interactive" };
const reply = (status: number, body: unknown = {}) =>
  new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });

function Seeded({ intent, children }: { intent: RunIntent; children: React.ReactNode }) {
  const dispatch = useNewRunDispatch();
  const [ready, setReady] = React.useState(false);
  React.useEffect(() => {
    dispatch({ type: "edit", apply: () => intent });
    setReady(true);
  }, [dispatch, intent]);
  return ready ? children : null;
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("loadRoster against the status client's fallback", () => {
  it("rejects with the couldn't-read note when the status read fails", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(reply(500)));
    await expect(loadRoster()).rejects.toThrow(NR_HARNESSES_LOOKUP.UNREACHABLE);
  });

  it("resolves the roster when the status read answers", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(reply(200, { runner: { driver: "docker", confinement_classes: [] }, harnesses: ROSTER.harnesses, harness_sets: ROSTER.sets })));
    await expect(loadRoster()).resolves.toMatchObject({ harnesses: ROSTER.harnesses, sets: ROSTER.sets });
  });
});

describe("Harnesses step: a roster that could not be read", () => {
  it("says so with Try again, and reads the roster again on it", async () => {
    const load = vi.fn<() => Promise<Roster>>().mockRejectedValueOnce(new Error(NR_HARNESSES_LOOKUP.UNREACHABLE)).mockResolvedValue(ROSTER);
    render(
      <MemoryRouter>
        <NewRunDraftProvider owner="alice">
          <Seeded intent={ENVIRONMENT}>
            <HarnessesStep location={{ screen: "step", step: "harnesses" }} go={vi.fn()} startFromScratch={() => {}} deps={{ load }} />
          </Seeded>
        </NewRunDraftProvider>
      </MemoryRouter>,
    );
    expect(await screen.findByText(NR_HARNESSES_LOOKUP.UNREACHABLE)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: NR_HARNESSES.ADD_HARNESS })).toBeDisabled();
    await userEvent.click(screen.getByRole("button", { name: NR_HARNESSES_LOOKUP.RETRY }));
    await waitFor(() => expect(screen.getByRole("button", { name: NR_HARNESSES.ADD_HARNESS })).toBeEnabled());
    expect(screen.queryByText(NR_HARNESSES_LOOKUP.UNREACHABLE)).toBeNull();
    expect(load).toHaveBeenCalledTimes(2);
  });
});
