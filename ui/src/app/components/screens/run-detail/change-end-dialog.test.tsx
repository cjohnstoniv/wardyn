/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #1322: the Change dialog's capped toast says the admin loosened the limit
// when the server says so, and the plain capped sentence otherwise.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { makeRun } from "../../../../test/factories";
import type { RunDetail } from "../../../lib/types";
import { ChangeEndDialog } from "./change-end-dialog";

const setRunEndAndWaitMock = vi.fn();
vi.mock("../../../lib/api/runs", () => ({
  runs: { setRunEndAndWait: (...a: unknown[]) => setRunEndAndWaitMock(...a) },
}));
const toastWarningMock = vi.fn();
vi.mock("sonner", () => ({
  toast: { error: vi.fn(), success: vi.fn(), warning: (...a: unknown[]) => toastWarningMock(...a) },
}));

const LOOSENED = "Your admin loosened this after the run started. Start a new run to get the new limit.";

async function saveCapped(extra: Record<string, unknown>) {
  const user = userEvent.setup({ pointerEventsCheck: 0 });
  const latestEnd = new Date(Date.now() + 2 * 24 * 3600_000).toISOString();
  setRunEndAndWaitMock.mockResolvedValue({
    id: "run-1",
    ends_at: latestEnd,
    wait_budget_sec: 0,
    capped: ["ends_at"],
    latest_end: latestEnd,
    ...extra,
  });
  render(
    <ChangeEndDialog open onOpenChange={() => {}} run={makeRun() as RunDetail} onChanged={() => {}} allowNoEnd={false} />,
  );
  await user.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => expect(toastWarningMock).toHaveBeenCalledTimes(1));
}

beforeEach(() => vi.clearAllMocks());

describe("ChangeEndDialog — a capped save", () => {
  it("the admin loosened the limit: says so, not the capped sentence", async () => {
    await saveCapped({ ends_cap_loosened: true });
    expect(toastWarningMock).toHaveBeenCalledWith(LOOSENED);
  });

  it("no flag: the capped sentence", async () => {
    await saveCapped({});
    expect(toastWarningMock).toHaveBeenCalledWith(expect.stringMatching(/as far as your admin allows/));
    expect(toastWarningMock).not.toHaveBeenCalledWith(LOOSENED);
  });
});
