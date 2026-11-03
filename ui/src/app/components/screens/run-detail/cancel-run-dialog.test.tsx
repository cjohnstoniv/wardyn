/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Cancel run on a run that waits: the toast names the run by its title, never by its id.
import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const toastApi = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn() }));
vi.mock("sonner", () => ({ toast: toastApi }));
const killRun = vi.hoisted(() => vi.fn());
vi.mock("../../../lib/api/runs", () => ({ runs: { killRun } }));

import CancelRunDialog from "./cancel-run-dialog";

const ID = "1697570d-aaaa-bbbb-cccc-0123456789ab";

beforeEach(() => vi.clearAllMocks());

describe("CancelRunDialog", () => {
  it("toasts Ending <title> when the kill is accepted", async () => {
    killRun.mockResolvedValue(undefined);
    const onClose = vi.fn();
    render(<CancelRunDialog runId={ID} title="Nightly dependency bump" onClose={onClose} />);
    await userEvent.click(screen.getByRole("button", { name: "Kill run" }));
    await waitFor(() => expect(toastApi.success).toHaveBeenCalledWith("Ending Nightly dependency bump"));
    expect(killRun).toHaveBeenCalledWith(ID);
    expect(onClose).toHaveBeenCalled();
  });

  it("toasts Couldn't end <title>, with the reason, when the kill fails", async () => {
    killRun.mockRejectedValue(new Error("run already ended"));
    render(<CancelRunDialog runId={ID} title="Nightly dependency bump" onClose={vi.fn()} />);
    await userEvent.click(screen.getByRole("button", { name: "Kill run" }));
    await waitFor(() => expect(toastApi.error).toHaveBeenCalledWith("Couldn't end Nightly dependency bump", { description: "run already ended" }));
  });

  it("says this run, never the id, when the run has no title", async () => {
    killRun.mockResolvedValue(undefined);
    render(<CancelRunDialog runId={ID} onClose={vi.fn()} />);
    await userEvent.click(screen.getByRole("button", { name: "Kill run" }));
    await waitFor(() => expect(toastApi.success).toHaveBeenCalledWith("Ending this run"));
  });
});
