/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import type { RunClones } from "../../../lib/api/run-clones";

const getRunClonesMock = vi.fn();
vi.mock("../../../lib/api/run-clones", () => ({
  getRunClones: (...a: unknown[]) => getRunClonesMock(...a),
}));

import { CloneStatusNote, CLONE_POLL_MS } from "./clone-status-note";
import { CLONE_NOTE } from "./clone-status-note-copy";

const clones = (state: string, repos: RunClones["repos"] = []): RunClones => ({ run_id: "run-1", state, repos });

function mount() {
  return render(
    <MemoryRouter initialEntries={["/runs/run-1"]}>
      <Routes>
        <Route path="/runs/:id" element={<CloneStatusNote />} />
      </Routes>
    </MemoryRouter>,
  );
}

// Lets the awaited read settle without moving the clock.
const settle = () => act(async () => {});

beforeEach(() => {
  cleanup();
  getRunClonesMock.mockReset();
  vi.useFakeTimers();
});
afterEach(() => {
  vi.useRealTimers();
});

describe("CloneStatusNote", () => {
  it("the container has role=status while pending", async () => {
    getRunClonesMock.mockResolvedValue(clones("pending"));
    mount();
    expect(screen.getByRole("status")).toBeInTheDocument();
    await settle();
    expect(screen.getByRole("status")).toBeEmptyDOMElement();
    expect(getRunClonesMock).toHaveBeenCalledWith("run-1");
  });

  it("renders the note after a pending to not_reported poll, and stops polling", async () => {
    getRunClonesMock
      .mockResolvedValueOnce(clones("pending"))
      .mockResolvedValueOnce(clones("not_reported", [{ repo: "example/docs", dest: "/workspace/docs", outcome: "not_reported" }]));
    mount();
    await settle();
    expect(getRunClonesMock).toHaveBeenCalledTimes(1);
    const status = screen.getByRole("status");
    expect(status).toBeEmptyDOMElement();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(CLONE_POLL_MS);
    });
    expect(getRunClonesMock).toHaveBeenCalledTimes(2);
    expect(screen.getByRole("status")).toBe(status);
    expect(status).toHaveTextContent(CLONE_NOTE.titleNotReported);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(CLONE_POLL_MS * 3);
    });
    expect(getRunClonesMock).toHaveBeenCalledTimes(2);
  });

  it("draws nothing for none, and for rows that all cloned", async () => {
    getRunClonesMock.mockResolvedValue(clones("none"));
    mount();
    await settle();
    expect(screen.getByRole("status")).toBeEmptyDOMElement();
    cleanup();
    getRunClonesMock.mockResolvedValue(clones("reported", [{ dest: "/workspace/a", outcome: "ok" }, { dest: "/workspace/b", outcome: "present" }]));
    mount();
    await settle();
    expect(screen.getByRole("status")).toBeEmptyDOMElement();
  });

  it("a skipped or failed row gets the other title, the board's sentence and the footer", async () => {
    getRunClonesMock.mockResolvedValue(
      clones("reported", [
        { repo: "example/docs", dest: "/workspace/docs", outcome: "skipped", cause: "not_a_checkout" },
        { repo: "example/api", dest: "/workspace/api", outcome: "failed", cause: "ref_failed" },
        { dest: "/workspace/site", outcome: "not_reported" },
        { dest: "/workspace/ok", outcome: "ok" },
      ]),
    );
    mount();
    await settle();
    const status = screen.getByRole("status");
    expect(status).toHaveTextContent(CLONE_NOTE.titleSomeNotCloned);
    expect(status).not.toHaveTextContent(CLONE_NOTE.titleNotReported);
    expect(status).toHaveTextContent("Skipped. The folder already holds files that are not a checkout of this repository, and they were left as they are.");
    expect(status).toHaveTextContent("Not cloned. The ref may not exist, or the run's allowed hosts or Wardyn's credential broker may have blocked the clone.");
    expect(status).toHaveTextContent(CLONE_NOTE.footer);
    expect(status).not.toHaveTextContent("/workspace/ok");
    expect(status).toHaveTextContent("Not reported");
  });

  it("the not-reported sentence never names the sandbox", async () => {
    getRunClonesMock.mockResolvedValue(
      clones("not_reported", [
        { repo: "example/docs", dest: "/workspace/docs", outcome: "not_reported" },
        { dest: "/workspace/tools", outcome: "not_reported" },
      ]),
    );
    mount();
    await settle();
    const status = screen.getByRole("status");
    expect(status).toHaveTextContent("Wardyn got no outcome for this repository, so it may or may not have been cloned.");
    expect(CLONE_NOTE.notReportedRow).not.toMatch(/sandbox/i);
    expect(status).toHaveTextContent(CLONE_NOTE.titleNotReported);
    expect(status).not.toHaveTextContent(CLONE_NOTE.footer);
    expect(status).not.toHaveTextContent(/failed/i);
  });

  it("a failed read draws nothing", async () => {
    getRunClonesMock.mockRejectedValue(new Error("boom"));
    mount();
    await settle();
    expect(screen.getByRole("status")).toBeEmptyDOMElement();
  });
});
