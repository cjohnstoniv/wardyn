/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { shortTime } from "../../../../lib/format";
import { aheadByHours } from "../../../../lib/test-clock";
import type { RunComputerFolder, RunDetail } from "../../../../lib/types";
import { FORCED_KEEP_PREFIX, RUN_COMPUTER_FOLDER as C } from "../../../wardyn/copy/runners";
import { ComputerFolderRows } from "./identity-computer-folder";

const AT = aheadByHours(-1);
const FOLDER: RunComputerFolder = { name: "payments-service", runner: "laptop-7f2", remote_dir: "/home/agent/work", direction: "both", state: "waiting" };
const show = (state: string, folder?: RunComputerFolder) =>
  render(<dl>{<ComputerFolderRows run={{ id: "r1", state, computer_folder: folder } as unknown as RunDetail} />}</dl>);

afterEach(cleanup);

describe("Folders from your computer", () => {
  it("is absent on a run with no computer folder", () => {
    show("RUNNING");
    expect(screen.queryByText(C.TITLE)).toBeNull();
  });

  it("waits for the first sync, and says nothing has run yet", () => {
    show("RUNNING", FOLDER);
    expect(screen.getByText(C.TITLE)).toBeInTheDocument();
    expect(screen.getByText(C.WAITING)).toBeInTheDocument();
    expect(screen.getByText(C.WAITING_BODY)).toBeInTheDocument();
    expect(screen.getByText("/home/agent/work")).toBeInTheDocument();
    expect(screen.getByText(C.BOTH)).toBeInTheDocument();
  });

  it("shows a synced folder's files at start, and paths it refused", () => {
    show("RUNNING", { ...FOLDER, state: "synced", seed: { at: AT, files: 1284, bytes: 48234496, refused: [{ path: "link", reason: "symlink" }] } });
    expect(screen.getByText(C.SYNCED)).toBeInTheDocument();
    expect(screen.getByText(C.AT_START("1,284", "46.0 MB"))).toBeInTheDocument();
    expect(screen.getByText(C.REFUSED("1"))).toBeInTheDocument();
  });

  it("names why the first sync stopped", () => {
    show("FAILED", { ...FOLDER, state: "failed", error: "laptop-7f2 is offline" });
    expect(screen.getByText(C.STOPPED)).toBeInTheDocument();
    expect(screen.getByRole("alert")).toHaveTextContent(`laptop-7f2 is offline ${C.NOTHING_RAN}`);
  });

  it("once a Both ways run ends, says what came back and never claims the sandbox copy was deleted", () => {
    show("COMPLETED", {
      ...FOLDER, state: "synced",
      seed: { at: AT, files: 1284, bytes: 1, refused: [] }, back: { at: AT, files: 3, bytes: 10, refused: [] },
    });
    expect(screen.getByText(C.ENDED)).toBeInTheDocument();
    expect(screen.getByText(C.FILES("3"))).toBeInTheDocument();
    expect(screen.getByText("The run has ended. Its sandbox copy may no longer be available.")).toBeInTheDocument();
    expect(screen.queryByText(/sandbox copy was deleted/)).toBeNull();
    expect(screen.getByText(C.BACK_AT(shortTime(AT), "3"))).toBeInTheDocument();
  });

  it("says when the changes could not come back", () => {
    show("COMPLETED", { ...FOLDER, state: "synced", seed: { at: AT, files: 1, bytes: 1, refused: [] }, back: { at: AT, files: 0, bytes: 0, refused: [], error: "laptop-7f2 is offline" } });
    expect(screen.getByRole("alert")).toHaveTextContent(C.BACK_FAILED("laptop-7f2 is offline"));
  });

  it("a forced end that kept the sandbox for its copy back is not called a failure", () => {
    show("FAILED", {
      ...FOLDER, state: "synced", fate: "retained", sandbox_copy: "kept",
      seed: { at: AT, files: 1, bytes: 1, refused: [] },
      back: { at: AT, files: 0, bytes: 0, refused: [], error: `${FORCED_KEEP_PREFIX}: the run was ended past its end while another operation held it or too many runs were ending at once` },
    });
    expect(screen.getByText(C.BACK_KEPT_FORCED)).toBeInTheDocument();
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.queryByText(/could not come back/)).toBeNull();
    expect(screen.getByText(C.NOT_BACK)).toBeInTheDocument();
  });

  describe("what became of the sandbox copy", () => {
    const SYNCED: RunComputerFolder = { ...FOLDER, state: "synced", seed: { at: AT, files: 9, bytes: 1, refused: [] } };
    const GIVE_UP_AT = "2026-11-12T14:30:00Z";
    const NAMES = ["README.md", "src/app/main.ts"];
    const REPORT = { unsynced: 4, paths: NAMES, at: AT };

    it("a terminal run with a pending pass is kept until the deadline, with count, list and time", () => {
      show("COMPLETED", { ...SYNCED, fate: "retained", sandbox_copy: "kept", last_report: { ...REPORT, paths_more: 2 }, give_up_at: GIVE_UP_AT });
      expect(screen.getByText(C.NOT_BACK)).toBeInTheDocument();
      expect(screen.getByText(C.FILES("4"))).toBeInTheDocument();
      expect(screen.getByText(C.KEPT_LIST)).toBeInTheDocument();
      const list = screen.getByRole("list", { name: C.PATHS_LABEL });
      expect(list).toHaveTextContent("README.md");
      expect(list).toHaveTextContent("src/app/main.ts");
      expect(screen.getByText(C.MORE("2"))).toBeInTheDocument();
      // The instant 2026-11-12T14:30Z shown in the viewer's own zone, with that zone's short name.
      const wanted: Record<string, RegExp> = {
        UTC: /02:30:00 PM UTC\b/,
        "America/New_York": /09:30:00 AM EST\b/,
        "Asia/Tokyo": /11:30:00 PM (GMT\+9|JST)\b/,
      };
      const para = screen.getByText(/^Wardyn may give up these changes and stop the sandbox after /);
      expect(para.textContent).toMatch(/ or sooner if needed to cut its network access\.$/);
      const zone = wanted[process.env.TZ ?? ""];
      if (zone) expect(para.textContent).toMatch(zone);
      else expect(para.textContent).toMatch(/\d{2}:30:00 [AP]M \S+, or/);
      expect(screen.getByText(C.KEEP_RUNNER_ONLINE("laptop-7f2"))).toBeInTheDocument();
      expect(screen.queryByText(C.MAY_BE_GONE)).toBeNull();
    });

    it("a give-up shows the count and its list", () => {
      show("COMPLETED", { ...SYNCED, fate: "given_up", sandbox_copy: "deleted", last_report: { unsynced: 2, paths: NAMES, at: AT } });
      expect(screen.getByText(C.GIVEN_UP)).toBeInTheDocument();
      expect(screen.getByText(C.FILES("2"))).toBeInTheDocument();
      expect(screen.getByText(C.GAVE_UP_LIST)).toBeInTheDocument();
      expect(screen.getByRole("list", { name: C.PATHS_LABEL })).toHaveTextContent("src/app/main.ts");
    });

    it("a give-up whose stop is not confirmed says so and claims no deletion", () => {
      show("COMPLETED", { ...SYNCED, fate: "given_up", sandbox_copy: "kept", last_report: REPORT });
      expect(screen.getByText(C.STOP_UNCONFIRMED)).toBeInTheDocument();
      expect(screen.queryByText(C.DELETED)).toBeNull();
      expect(screen.queryByText(C.GAVE_UP_LIST)).toBeNull();
    });

    it("says deleted only for sandbox_copy deleted, never for an ended run alone", () => {
      show("COMPLETED", { ...SYNCED, fate: "complete", sandbox_copy: "deleted" });
      expect(screen.getByText(C.DELETED)).toBeInTheDocument();
      cleanup();
      for (const copy of [undefined, "kept"] as const) {
        show("COMPLETED", { ...SYNCED, fate: "complete", sandbox_copy: copy });
        expect(screen.queryByText(C.DELETED)).toBeNull();
        cleanup();
      }
      show("COMPLETED", { ...SYNCED, fate: "given_up", last_report: REPORT });
      expect(screen.queryByText(C.DELETED)).toBeNull();
    });

    it("a give-up with sandbox_copy omitted or unknown leads with the unconfirmed stop, keeping heading, count and list", () => {
      for (const copy of [undefined, "future_value"]) {
        show("COMPLETED", { ...SYNCED, fate: "given_up", sandbox_copy: copy as "kept", last_report: REPORT });
        expect(screen.getByText(C.GIVEN_UP)).toBeInTheDocument();
        expect(screen.getByText(C.FILES("4"))).toBeInTheDocument();
        expect(screen.getByText(C.STOP_UNCONFIRMED)).toBeInTheDocument();
        expect(screen.getByRole("list", { name: C.PATHS_LABEL })).toHaveTextContent("README.md");
        expect(screen.queryByText(C.GAVE_UP_LIST)).toBeNull();
        expect(screen.queryByText(C.GAVE_UP_NO_LIST)).toBeNull();
        expect(screen.queryByText(C.DELETED)).toBeNull();
        cleanup();
      }
    });

    it("an unknown fate falls back to the neutral line and draws no fate rows", () => {
      show("COMPLETED", { ...SYNCED, fate: "future_value" as "retained", last_report: REPORT });
      expect(screen.getByText(C.MAY_BE_GONE)).toBeInTheDocument();
      expect(screen.queryByText(C.NOT_BACK)).toBeNull();
      expect(screen.queryByText(C.GIVEN_UP)).toBeNull();
    });

    it("a count with no names shows the count and no list", () => {
      show("COMPLETED", { ...SYNCED, fate: "retained", last_report: { unsynced: 7, at: AT }, give_up_at: GIVE_UP_AT });
      expect(screen.getByText(C.FILES("7"))).toBeInTheDocument();
      expect(screen.getByText(C.KEPT_NO_LIST)).toBeInTheDocument();
      expect(screen.queryByRole("list", { name: C.PATHS_LABEL })).toBeNull();
    });

    it("a count the runner never made reads Not counted, never 0", () => {
      show("COMPLETED", { ...SYNCED, fate: "retained", last_report: { unsynced: null, at: AT }, give_up_at: GIVE_UP_AT });
      expect(screen.getByText(C.NOT_COUNTED)).toBeInTheDocument();
      expect(screen.queryByText(C.FILES("0"))).toBeNull();
      cleanup();
      show("COMPLETED", { ...SYNCED, fate: "given_up", sandbox_copy: "deleted" });
      expect(screen.getByText(C.NOT_COUNTED)).toBeInTheDocument();
      expect(screen.getByText(C.GAVE_UP_NO_LIST)).toBeInTheDocument();
      expect(screen.queryByText(C.FILES("0"))).toBeNull();
    });

    it("with no fate the neutral line stays", () => {
      show("COMPLETED", SYNCED);
      expect(screen.getByText(C.MAY_BE_GONE)).toBeInTheDocument();
      expect(screen.queryByText(C.NOT_BACK)).toBeNull();
    });

    it("long paths wrap inside the scroll box instead of being cut off", () => {
      const long = "src/features/billing/invoices/components/line-items/adjustments/regional-tax/rounding-rules/fixtures/generated/en-GB/invoice.snapshot.test.ts";
      show("COMPLETED", { ...SYNCED, fate: "retained", last_report: { unsynced: 1, paths: [long], at: AT }, give_up_at: GIVE_UP_AT });
      const box = screen.getByTestId("run-computer-folder-paths");
      expect(box).toHaveTextContent(long);
      expect(box.className).toMatch(/break-all/);
      expect(box.className).not.toMatch(/truncate|whitespace-nowrap|text-ellipsis/);
      expect(box.className).toMatch(/overflow-y-auto/);
      expect(box).toHaveAttribute("tabindex", "0");
    });
  });
});
