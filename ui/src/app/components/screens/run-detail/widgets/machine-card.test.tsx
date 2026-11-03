/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// KM-3: the run page's Machine card, one test per frame the board draws. The run read carries the name, the Mac and the
// stop's result only, so those are the rows; each frame's test names it.

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { RunDetail, RunMachine } from "../../../../lib/types";
import { KEEP_LIST, KEEP_RUN as T } from "../../../wardyn/copy/keep-machine";
import { IdentityWidget } from "./identity";
import { MachineCard } from "./machine-card";

afterEach(cleanup);

const MAC = "Maya's MacBook";
const machine = (over: Partial<RunMachine> = {}): RunMachine => ({ id: "m-dev", name: "dev", mode: "new", keep_at_end: true, state: "in_use", scrubbed: false, runner_name: MAC, deleted: false, ...over });
type Facts = Pick<RunDetail, "machine" | "state" | "failure_hint">;
const show = (f: Facts) => {
  render(
    <MemoryRouter>
      <MachineCard run={f} />
    </MemoryRouter>,
  );
  return screen.queryByTestId("run-machine-card");
};
const text = (card: HTMLElement | null) => card?.textContent ?? "";

describe("Run page: the Machine card", () => {
  it("draws nothing for a run that keeps no machine", () => {
    expect(show({ state: "RUNNING" })).toBeNull();
  });

  it("km3-running-new: Keeps, K-31 and K-32, the name and the Mac", () => {
    const card = show({ state: "RUNNING", machine: machine() })!;
    expect(within(card).getByRole("heading", { name: T.TITLE })).toBeInTheDocument();
    expect(within(card).getByText(T.CHIP_KEEPS)).toBeInTheDocument();
    expect(within(card).getByText(T.WHILE_RUNNING)).toBeInTheDocument();
    expect(within(card).getByText(T.NEW(MAC))).toBeInTheDocument();
    expect(within(card).getByText(T.ROW_NAME).nextSibling).toHaveTextContent("dev");
    expect(within(card).getByText(T.ROW_MAC).nextSibling).toHaveTextContent(MAC);
  });

  it("km3-running-picked-name: the server-picked name shows", () => {
    const card = show({ state: "RUNNING", machine: machine({ name: "mac-7f3a" }) })!;
    expect(within(card).getByText(T.ROW_NAME).nextSibling).toHaveTextContent("mac-7f3a");
  });

  it("km3-running-resumed: K-31 with no 'last used' date, which the run read doesn't carry", () => {
    const card = show({ state: "RUNNING", machine: machine({ mode: "resumed" }) })!;
    expect(within(card).getByText(T.WHILE_RUNNING)).toBeInTheDocument();
    expect(text(card)).not.toMatch(/Last used/);
  });

  it("km3-running-fresh: K-34", () => {
    const card = show({ state: "RUNNING", machine: machine({ mode: "fresh" }) })!;
    expect(within(card).getByText(T.FRESH(MAC))).toBeInTheDocument();
  });

  it("km3-stopping: the run is over and the Mac has not said how the stop went: K-44", () => {
    const card = show({ state: "STOPPED", machine: machine() })!;
    expect(within(card).getByText(T.STOPPING)).toBeInTheDocument();
    expect(within(card).getByText(T.CHIP_KEEPS)).toBeInTheDocument();
  });

  it("km3-kept: Kept, K-35 and K-36 after a graceful stop", () => {
    const card = show({ state: "STOPPED", machine: machine({ state: "kept", scrubbed: true }) })!;
    expect(within(card).getByText(T.CHIP_KEPT)).toBeInTheDocument();
    expect(within(card).getByText(T.KEPT(MAC))).toBeInTheDocument();
    expect(within(card).getByText(T.SCRUBBED)).toBeInTheDocument();
  });

  it("km3-kept-hard: K-37 after a hard stop", () => {
    const card = show({ state: "KILLED", machine: machine({ state: "kept", scrubbed: false }) })!;
    expect(within(card).getByText(T.KEPT(MAC))).toBeInTheDocument();
    expect(within(card).getByText(T.NOT_SCRUBBED)).toBeInTheDocument();
    expect(within(card).queryByText(T.SCRUBBED)).toBeNull();
  });

  it("km3-grace: the run still RUNNING with its machine already kept: K-38 names the machine", () => {
    const card = show({ state: "RUNNING", machine: machine({ state: "kept", scrubbed: true }) })!;
    expect(within(card).getByText(T.HELD(MAC, "dev"))).toBeInTheDocument();
    expect(within(card).getByText(T.CHIP_KEPT)).toBeInTheDocument();
  });

  it("km3-deleted-admin: Deleted and K-39, claiming only the deletion", () => {
    const card = show({ state: "KILLED", machine: { id: "m-dev", deleted: true, scrubbed: false } })!;
    expect(within(card).getByText(T.CHIP_DELETED)).toBeInTheDocument();
    expect(within(card).getByText(T.DELETED)).toBeInTheDocument();
    expect(within(card).queryByText(T.KEPT(MAC))).toBeNull();
  });

  it("km3-deleting: Deleting with the neutral sentence, never claiming the Mac is offline", () => {
    const card = show({ state: "KILLED", machine: machine({ state: "deleting" }) })!;
    expect(within(card).getByText(T.CHIP_DELETING)).toBeInTheDocument();
    expect(within(card).getByText(KEEP_LIST.DELETE_UNCONFIRMED)).toBeInTheDocument();
    expect(text(card)).not.toMatch(/offline/);
  });

  it("a run whose machine's tools could not be updated, or that is locked, says so in the card", () => {
    expect(within(show({ state: "FAILED", machine: machine({ state: "kept" }), failure_hint: "kept_machine_tools_update_failed" })!).getByText(T.TOOLS_FAILED)).toBeInTheDocument();
    cleanup();
    expect(within(show({ state: "FAILED", machine: machine({ state: "kept" }), failure_hint: "kept_machine_locked" })!).getByText(T.LOCKED(MAC))).toBeInTheDocument();
  });

  it("K-42: the link goes to Your kept machines in settings", () => {
    const card = show({ state: "STOPPED", machine: machine({ state: "kept", scrubbed: true }) })!;
    expect(within(card).getByRole("link", { name: T.LINK })).toHaveAttribute("href", "/account#kept-machines");
  });

  it("km3-page: the card sits in the Identity tile of a run that keeps a machine, and not in one that keeps none", () => {
    const run = { id: "run-1", created_at: "", updated_at: "", created_by: "a@b.c", agent: "claude-code", repo: "r", task: "t", confinement_class: "CC2", state: "RUNNING", spiffe_id: "spiffe://x", runner_target: "docker", placement: "local" } as RunDetail;
    const view = (r: RunDetail) => render(<MemoryRouter><IdentityWidget run={r} onGoPolicy={() => {}} /></MemoryRouter>);
    view({ ...run, machine: machine() });
    expect(screen.getByTestId("run-machine-card")).toBeInTheDocument();
    cleanup();
    view(run);
    expect(screen.queryByTestId("run-machine-card")).toBeNull();
  });
});
