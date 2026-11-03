/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// KM-3: the run page's Machine tile, one test per frame the board draws. The run read is the machine's row as it is now,
// shared by every run that used it, and names the run that holds it (or held it last): only that run's page says what a
// stop did to the machine.

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { shortDate } from "../../../../lib/format";
import { aheadByHours } from "../../../../lib/test-clock";
import type { RunDetail, RunMachine } from "../../../../lib/types";
import { KEEP_LIST, KEEP_RUN as T } from "../../../wardyn/copy/keep-machine";
import { MachineWidget } from "./machine-card";

afterEach(cleanup);

const MAC = "Maya's MacBook";
const A = "run-a";
const B = "run-b";
const EXPIRES = aheadByHours(24 * 28);
const machine = (over: Partial<RunMachine> = {}): RunMachine => ({
  id: "m-dev", name: "dev", mode: "new", keep_at_end: true, state: "in_use", scrubbed: false, runner_name: MAC, holder_run_id: A,
  base: "Standard", disk_bytes: 12 * 1024 ** 3, expires_at: EXPIRES, deleted: false, ...over,
});
type Facts = Pick<RunDetail, "id" | "machine" | "state" | "failure_hint">;
const show = (f: Partial<Facts> & Pick<Facts, "state">) => {
  render(
    <MemoryRouter>
      <MachineWidget run={{ id: A, ...f }} />
    </MemoryRouter>,
  );
  return screen.queryByTestId("run-machine-card");
};
const text = (card: HTMLElement | null) => card?.textContent ?? "";
const row = (card: HTMLElement, label: string) => within(card).getByText(label).nextSibling;

describe("Run page: the Machine tile", () => {
  it("draws nothing for a run that keeps no machine", () => {
    expect(show({ state: "RUNNING" })).toBeNull();
  });

  it("km3-running-new: Keeps, K-31 and K-32, with the name, Mac and base rows", () => {
    const card = show({ state: "RUNNING", machine: machine() })!;
    expect(within(card).getByText(T.TITLE)).toBeInTheDocument();
    expect(within(card).getByText(T.CHIP_KEEPS)).toBeInTheDocument();
    expect(within(card).getByText(T.WHILE_RUNNING)).toBeInTheDocument();
    expect(within(card).getByText(T.NEW(MAC))).toBeInTheDocument();
    expect(row(card, T.ROW_NAME)).toHaveTextContent("dev");
    expect(row(card, T.ROW_MAC)).toHaveTextContent(MAC);
    expect(row(card, T.ROW_BASE)).toHaveTextContent("Standard");
  });

  it("km3-running-picked-name: the server-picked name shows", () => {
    const card = show({ state: "RUNNING", machine: machine({ name: "mac-7f3a" }) })!;
    expect(row(card, T.ROW_NAME)).toHaveTextContent("mac-7f3a");
  });

  it("km3-running-resumed: K-33 without its date, which a claim overwrites", () => {
    const card = show({ state: "RUNNING", machine: machine({ mode: "resumed" }) })!;
    expect(within(card).getByText(T.WHILE_RUNNING)).toBeInTheDocument();
    expect(within(card).getByText(T.RESUMED)).toBeInTheDocument();
    expect(text(card)).not.toMatch(/Last used/);
  });

  it("km3-running-fresh: K-34", () => {
    const card = show({ state: "RUNNING", machine: machine({ mode: "fresh" }) })!;
    expect(within(card).getByText(T.FRESH(MAC))).toBeInTheDocument();
  });

  it("km3-stopping: this run holds the machine, has ended, and the Mac has not reported the stop: K-44", () => {
    const card = show({ state: "STOPPED", machine: machine() })!;
    expect(within(card).getByText(T.STOPPING)).toBeInTheDocument();
    expect(within(card).getByText(T.CHIP_KEEPS)).toBeInTheDocument();
  });

  it("km3-kept: this run held it and stopped: Kept, K-35, the Written and Expires rows and K-36", () => {
    const card = show({ state: "STOPPED", machine: machine({ state: "kept", scrubbed: true }) })!;
    expect(within(card).getByText(T.CHIP_KEPT)).toBeInTheDocument();
    expect(within(card).getByText(T.KEPT(MAC))).toBeInTheDocument();
    expect(row(card, T.ROW_WRITTEN)).toHaveTextContent("12 GB");
    expect(row(card, T.ROW_EXPIRES)).toHaveTextContent(shortDate(EXPIRES));
    expect(within(card).getByText(T.SCRUBBED)).toBeInTheDocument();
    expect(within(card).queryByText(T.AS_NOW)).toBeNull();
  });

  it("km3-kept-hard: this run held it and stopped abruptly: K-37 with the Kept chip", () => {
    const card = show({ state: "KILLED", machine: machine({ state: "kept", scrubbed: false }) })!;
    expect(within(card).getByText(T.KEPT(MAC))).toBeInTheDocument();
    expect(within(card).getByText(T.NOT_SCRUBBED)).toBeInTheDocument();
    expect(within(card).getByText(T.CHIP_KEPT)).toBeInTheDocument();
  });

  it("km3-grace: the run still RUNNING with its machine already kept: K-38 names the machine, and K-36 or K-37 is this run's own stop", () => {
    const card = show({ state: "RUNNING", machine: machine({ state: "kept", scrubbed: true }) })!;
    expect(within(card).getByText(T.HELD(MAC, "dev"))).toBeInTheDocument();
    expect(within(card).getByText(T.CHIP_KEPT)).toBeInTheDocument();
    expect(within(card).getByText(T.SCRUBBED)).toBeInTheDocument();
    cleanup();
    expect(within(show({ state: "RUNNING", machine: machine({ state: "kept", scrubbed: false }) })!).getByText(T.NOT_SCRUBBED)).toBeInTheDocument();
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

  it("a run whose machine's tools could not be updated, or that is locked, shows the runner's own sentence in the tile", () => {
    const tools = "This machine's tools couldn't be updated, so it didn't start. Start it fresh, or try again.";
    const locked = "This machine is already running on this Mac.";
    expect(within(show({ state: "FAILED", machine: machine({ state: "kept" }), failure_hint: tools })!).getByText(tools)).toBeInTheDocument();
    cleanup();
    expect(within(show({ state: "FAILED", machine: machine({ state: "kept" }), failure_hint: locked })!).getByText(locked)).toBeInTheDocument();
    cleanup();
    expect(text(show({ state: "FAILED", machine: machine({ state: "kept", scrubbed: true, holder_run_id: A }), failure_hint: "the image could not be pulled" }))).not.toContain("image could not");
  });

  it("K-42: the link goes to Your kept machines in settings", () => {
    const card = show({ state: "STOPPED", machine: machine({ state: "kept", scrubbed: true }) })!;
    expect(within(card).getByRole("link", { name: T.LINK })).toHaveAttribute("href", "/account#kept-machines");
  });
});

// Run A stopped cleanly and kept dev; run B then resumed it. The row is B's now: A's page says the machine as it is now.
describe("Run page: a run that no longer holds the machine", () => {
  it("while B holds it, A's page never says Stopping, nor anything of A's stop", () => {
    const card = show({ state: "STOPPED", machine: machine({ state: "in_use", scrubbed: true, holder_run_id: B }) })!;
    expect(within(card).getByText(T.BUSY)).toBeInTheDocument();
    expect(within(card).getByText(KEEP_LIST.CHIP_IN_USE)).toBeInTheDocument();
    expect(text(card)).not.toMatch(/Stopping|stop normally|settings/);
  });

  it("after B stopped abruptly, A's page flags the machine as needing cleanup, amber, without saying which run stopped badly", () => {
    const card = show({ state: "STOPPED", machine: machine({ state: "kept", scrubbed: false, holder_run_id: B }) })!;
    expect(within(card).getByText(T.NEEDS_CLEANUP)).toBeInTheDocument();
    expect(within(card).getByText(KEEP_LIST.CHIP_NOT_CLEANED)).toBeInTheDocument();
    expect(within(card).getByText(T.AS_NOW)).toBeInTheDocument();
    expect(within(card).queryByText(T.NOT_SCRUBBED)).toBeNull();
    expect(text(card)).not.toMatch(/didn't stop normally|this run/i);
  });

  it("after B stopped cleanly, A's page says Kept as the machine is now, with no stop result", () => {
    const card = show({ state: "STOPPED", machine: machine({ state: "kept", scrubbed: true, holder_run_id: B }) })!;
    expect(within(card).getByText(T.CHIP_KEPT)).toBeInTheDocument();
    expect(within(card).getByText(T.AS_NOW)).toBeInTheDocument();
    expect(within(card).queryByText(T.SCRUBBED)).toBeNull();
    expect(row(card, T.ROW_WRITTEN)).toHaveTextContent("12 GB");
  });

  it("a server that names no holder leaves only a live run sure of it: an ended run says the machine as it is now", () => {
    const card = show({ state: "STOPPED", machine: machine({ state: "kept", scrubbed: true, holder_run_id: undefined }) })!;
    expect(within(card).getByText(T.AS_NOW)).toBeInTheDocument();
    cleanup();
    expect(within(show({ state: "RUNNING", machine: machine({ holder_run_id: undefined }) })!).getByText(T.WHILE_RUNNING)).toBeInTheDocument();
  });
});
