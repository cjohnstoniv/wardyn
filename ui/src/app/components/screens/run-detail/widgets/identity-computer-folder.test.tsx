/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { shortTime } from "../../../../lib/format";
import type { RunComputerFolder, RunDetail } from "../../../../lib/types";
import { RUN_COMPUTER_FOLDER as C } from "../../../wardyn/copy/runners";
import { ComputerFolderRows } from "./identity-computer-folder";

const AT = "2026-10-19T10:12:00Z";
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

  it("once a Both ways run ends, says what came back and that the sandbox copy is gone", () => {
    show("COMPLETED", {
      ...FOLDER, state: "synced",
      seed: { at: AT, files: 1284, bytes: 1, refused: [] }, back: { at: AT, files: 3, bytes: 10, refused: [] },
    });
    expect(screen.getByText(C.ENDED)).toBeInTheDocument();
    expect(screen.getByText(C.FILES("3"))).toBeInTheDocument();
    expect(screen.getByText(C.DELETED)).toBeInTheDocument();
    expect(screen.getByText(C.BACK_AT(shortTime(AT), "3"))).toBeInTheDocument();
  });

  it("says when the changes could not come back", () => {
    show("COMPLETED", { ...FOLDER, state: "synced", seed: { at: AT, files: 1, bytes: 1, refused: [] }, back: { at: AT, files: 0, bytes: 0, refused: [], error: "laptop-7f2 is offline" } });
    expect(screen.getByRole("alert")).toHaveTextContent(C.BACK_FAILED("laptop-7f2 is offline"));
  });
});
