/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { runs } from "../../../../lib/api/runs";
import { SCENARIO_PREFLIGHT } from "../../../../lib/new-run-fixtures/scenarios/preflight";
import type { PreflightResult } from "../../../../lib/types/preflight";
import { NR_LAUNCH } from "../../../wardyn/copy/new-run/launch";
import { NR_REVIEW } from "../../../wardyn/copy/new-run/review";
import { OperatorProvider } from "../../../wardyn/operator-context";
import type { RunIntent } from "../draft/run-intent";
import { NEW_RUN_SCREENS, NewRunRoute, type Seed, type SeedLoaders } from "../new-run-route";
import { LaunchedScreen } from "./launched-screen";
import { ReviewWithLaunch } from "./review-with-launch";

const RUN_ID = "b2222222-2222-4222-8222-222222222222";
const INTENT: RunIntent = { shape: "environment", experience: "interactive", no_repositories_or_drives: true, ends_after_sec: 3600 };
const loaders: SeedLoaders = {
  template: async (): Promise<Seed> => ({ origin: { kind: "scratch" }, intent: INTENT }),
  again: async (): Promise<Seed> => ({ origin: { kind: "again", runId: RUN_ID, title: "before" }, intent: INTENT }),
};
function Where() {
  const l = useLocation();
  return <p data-testid="where">{l.pathname + l.search}</p>;
}

beforeEach(() => {
  vi.spyOn(runs, "preflightRunWire").mockResolvedValue(SCENARIO_PREFLIGHT.member as PreflightResult);
  vi.spyOn(runs, "previewRunPolicyWire").mockResolvedValue({ spec: {}, source: { kind: "inline" }, warnings: [], pending: [], repository_access: [] } as never);
  vi.spyOn(runs, "createRunWire").mockResolvedValue({ id: RUN_ID, state: "PENDING", title: "wired" } as never);
  vi.spyOn(runs, "getRun").mockResolvedValue({ id: RUN_ID, state: "PENDING", title: "wired" } as never);
});
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("Review launches through the Name dialog to Launched", () => {
  it("Launch opens the dialog; a create lands on ?launched=", async () => {
    render(
      <MemoryRouter initialEntries={[`/runs/new?again=${RUN_ID}`]}>
        <OperatorProvider operator={false} principal="alice">
          <Routes>
            <Route
              path="/runs/new"
              element={<><NewRunRoute loaders={loaders} screens={{ ...NEW_RUN_SCREENS, review: ReviewWithLaunch, launched: LaunchedScreen }} /><Where /></>}
            />
          </Routes>
        </OperatorProvider>
      </MemoryRouter>,
    );
    const launch = await screen.findByRole("button", { name: NR_REVIEW.LAUNCH_RUN });
    await waitFor(() => expect(launch).toBeEnabled());
    await userEvent.click(launch);
    const box = await screen.findByRole("dialog", { name: NR_LAUNCH.TITLE });
    await userEvent.type(within(box).getByLabelText(NR_LAUNCH.TITLE_LABEL), "wired");
    await within(box).findByText(NR_LAUNCH.CHECKED_JUST_NOW);
    await userEvent.click(within(box).getByRole("button", { name: new RegExp(`^${NR_LAUNCH.LAUNCH}$`) }));
    expect(await screen.findByText(NR_LAUNCH.BADGE_STARTING)).toBeInTheDocument();
    expect(screen.getByTestId("where")).toHaveTextContent(`/runs/new?launched=${RUN_ID}`);
    expect(runs.createRunWire).toHaveBeenCalledTimes(1);
  });

  describe("Cancel while the create is out (DR-0269)", () => {
    function mount() {
      render(
        <MemoryRouter initialEntries={[`/runs/new?again=${RUN_ID}`]}>
          <OperatorProvider operator={false} principal="alice">
            <Routes>
              <Route
                path="/runs/new"
                element={<><NewRunRoute loaders={loaders} screens={{ ...NEW_RUN_SCREENS, review: ReviewWithLaunch, launched: LaunchedScreen }} /><Where /></>}
              />
            </Routes>
          </OperatorProvider>
        </MemoryRouter>,
      );
    }
    const openDialog = async () => {
      const launch = await screen.findByRole("button", { name: NR_REVIEW.LAUNCH_RUN });
      await waitFor(() => expect(launch).toBeEnabled());
      await userEvent.click(launch);
      const box = await screen.findByRole("dialog", { name: NR_LAUNCH.TITLE });
      await within(box).findByText(NR_LAUNCH.CHECKED_JUST_NOW);
      return box;
    };
    const press = (box: HTMLElement) => userEvent.click(within(box).getByRole("button", { name: new RegExp(`^${NR_LAUNCH.LAUNCH}$`) }));

    it("a reopened dialog cannot make a second run while the first create is still out", async () => {
      let finish!: (r: unknown) => void;
      vi.mocked(runs.createRunWire).mockImplementation(() => new Promise((resolve) => { finish = resolve; }) as never);
      mount();
      let box = await openDialog();
      await userEvent.type(within(box).getByLabelText(NR_LAUNCH.TITLE_LABEL), "once");
      await press(box);
      expect(runs.createRunWire).toHaveBeenCalledTimes(1);
      await userEvent.click(within(box).getByRole("button", { name: NR_LAUNCH.CANCEL }));
      await waitFor(() => expect(screen.queryByRole("dialog", { name: NR_LAUNCH.TITLE })).toBeNull());

      box = await openDialog();
      expect(within(box).getByRole("button", { name: new RegExp(`^${NR_LAUNCH.LAUNCH}$`) })).toBeDisabled();
      await press(box);
      expect(runs.createRunWire).toHaveBeenCalledTimes(1);

      // The one run still lands where it always did.
      finish({ id: RUN_ID, state: "PENDING", title: "once" });
      expect(await screen.findByText(NR_LAUNCH.BADGE_STARTING)).toBeInTheDocument();
      expect(runs.createRunWire).toHaveBeenCalledTimes(1);
    });

    it("a later deliberate launch works once the first create has answered", async () => {
      let fail!: (e: unknown) => void;
      vi.mocked(runs.createRunWire).mockImplementationOnce(() => new Promise((_ok, no) => { fail = no; }) as never);
      mount();
      let box = await openDialog();
      await userEvent.type(within(box).getByLabelText(NR_LAUNCH.TITLE_LABEL), "twice");
      await press(box);
      await userEvent.click(within(box).getByRole("button", { name: NR_LAUNCH.CANCEL }));
      await waitFor(() => expect(screen.queryByRole("dialog", { name: NR_LAUNCH.TITLE })).toBeNull());
      fail(new Error("refused"));

      box = await openDialog();
      await waitFor(() => expect(within(box).getByRole("button", { name: new RegExp(`^${NR_LAUNCH.LAUNCH}$`) })).toBeEnabled());
      await press(box);
      expect(await screen.findByText(NR_LAUNCH.BADGE_STARTING)).toBeInTheDocument();
      expect(runs.createRunWire).toHaveBeenCalledTimes(2);
    });
  });
});
