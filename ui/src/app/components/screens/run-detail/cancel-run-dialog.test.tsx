/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Cancel run on a run that waits: the toast names the run by its title, never by its id.
import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HttpError } from "../../../lib/api/core";

const toastApi = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn(), info: vi.fn() }));
vi.mock("sonner", () => ({ toast: toastApi }));
const killRun = vi.hoisted(() => vi.fn());
vi.mock("../../../lib/api/runs", () => ({ runs: { killRun } }));

import CancelRunDialog from "./cancel-run-dialog";

const ID = "1697570d-aaaa-bbbb-cccc-0123456789ab";

beforeEach(() => vi.clearAllMocks());

describe("CancelRunDialog", () => {
  it("asks a plain cancel question: titled Cancel, no sandbox or credential talk, Keep run to back out", () => {
    render(<CancelRunDialog runId={ID} title="Nightly dependency bump" onClose={vi.fn()} />);
    const dialog = screen.getByRole("alertdialog", { name: "Cancel Nightly dependency bump?" });
    expect(dialog).toHaveTextContent("It will not start. Nothing was set up.");
    expect(dialog).not.toHaveTextContent(/sandbox|credentials|audit/i);
    expect(screen.getByRole("button", { name: "Keep run" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "End run" })).toBeNull();
  });

  it("toasts Cancelled <title> when the kill is accepted", async () => {
    killRun.mockResolvedValue(undefined);
    const onClose = vi.fn();
    render(<CancelRunDialog runId={ID} title="Nightly dependency bump" onClose={onClose} />);
    await userEvent.click(screen.getByRole("button", { name: "Cancel run" }));
    await waitFor(() => expect(toastApi.success).toHaveBeenCalledWith("Cancelled Nightly dependency bump"));
    expect(killRun).toHaveBeenCalledWith(ID, { notStarted: true });
    expect(onClose).toHaveBeenCalled();
  });

  // The run started after the view that opened this confirm last read it: the cancel asked the server to end it only
  // while it had not started, the server refused, and the toast says it started and was not cancelled.
  it("says the run started, not Cancelled or Couldn't cancel, when the server refuses because it has", async () => {
    killRun.mockRejectedValue(new HttpError(409, "run has started", "run_kill_started"));
    render(<CancelRunDialog runId={ID} title="Nightly dependency bump" onClose={vi.fn()} />);
    await userEvent.click(screen.getByRole("button", { name: "Cancel run" }));
    await waitFor(() =>
      expect(toastApi.info).toHaveBeenCalledWith("Not cancelled: Nightly dependency bump has started", { description: "To stop it, choose End run on its page." }),
    );
    expect(killRun).toHaveBeenCalledWith(ID, { notStarted: true });
    expect(toastApi.success).not.toHaveBeenCalled();
    expect(toastApi.error).not.toHaveBeenCalled();
  });

  it("toasts Couldn't cancel <title>, with the reason, when the kill fails", async () => {
    killRun.mockRejectedValue(new Error("run already ended"));
    render(<CancelRunDialog runId={ID} title="Nightly dependency bump" onClose={vi.fn()} />);
    await userEvent.click(screen.getByRole("button", { name: "Cancel run" }));
    await waitFor(() => expect(toastApi.error).toHaveBeenCalledWith("Couldn't cancel Nightly dependency bump", { description: "run already ended" }));
  });

  it("says this run, never the id, when the run has no title", async () => {
    killRun.mockResolvedValue(undefined);
    render(<CancelRunDialog runId={ID} onClose={vi.fn()} />);
    await userEvent.click(screen.getByRole("button", { name: "Cancel run" }));
    await waitFor(() => expect(toastApi.success).toHaveBeenCalledWith("Cancelled this run"));
  });
});

// The precondition's two words are the server's: the query flag the kill handler reads and the reason it refuses with.
describe("cancel before start, Go and TypeScript", () => {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8 && !existsSync(join(dir, "go.mod")); i++) dir = dirname(dir);
  const read = (p: string) => readFileSync(join(dir, p), "utf8");

  it("sends the flag the kill handler reads, and knows the reason it refuses with", () => {
    expect(read("ui/src/app/lib/api/runs.ts")).toContain('"?not_started=true"');
    expect(read("internal/api/runs_lifecycle.go")).toContain('r.URL.Query().Get("not_started") == "true"');
    expect(read("ui/src/app/lib/api/kill-started.ts")).toContain('RUN_KILL_STARTED_REASON = "run_kill_started"');
    expect(read("internal/api/reasons.go")).toMatch(/reasonRunKillStarted\s+=\s+"run_kill_started"/);
  });
});
