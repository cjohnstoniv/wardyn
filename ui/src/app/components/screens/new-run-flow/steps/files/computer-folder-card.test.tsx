/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import { ComputerFolderCard } from "./computer-folder";

const wfetchMock = vi.fn();
vi.mock("../../../../../lib/api/core", async (orig) => ({ ...(await orig<typeof import("../../../../../lib/api/core")>()), wfetch: (...a: unknown[]) => wfetchMock(...a) }));

const json = (body: unknown) => new Response(JSON.stringify(body), { status: 200, headers: { "content-type": "application/json" } });
const RUNNER = { id: "rn-laptop", name: "laptop-7f2", state: "claimed", folders: true, last_seen_at: new Date().toISOString() };
let broken: string | null;

beforeEach(() => {
  broken = null;
  wfetchMock.mockImplementation(async (path: string) => {
    if (path.startsWith("/me/runners/rn-laptop/folders")) {
      if (broken && path.includes(encodeURIComponent(broken))) throw new Error("runner went away");
      return json({ path: "", runner: "laptop-7f2", folders: [], survey: { files: 1284, bytes: 46 * 1024 * 1024, left_out: [], ignored: 0 } });
    }
    if (path.startsWith("/me/runners")) return json([{ ...RUNNER, online: true }]);
    throw new Error(`unexpected ${path}`);
  });
});
afterEach(() => {
  cleanup();
  wfetchMock.mockReset();
});

const card = (path: string) => (
  <MemoryRouter>
    <ComputerFolderCard folder={{ runner_id: "rn-laptop", path, name: "x", direction: "in" }} onDirection={() => {}} onChange={() => {}} onRemove={() => {}} />
  </MemoryRouter>
);

describe("ComputerFolderCard survey", () => {
  it("drops the old folder's counts when Change picks a folder whose survey fails", async () => {
    const { rerender } = render(card("/Users/alice/a"));
    const counts = NR_FILES.COMPUTER_COUNT("1,284", "46.0 MB");
    await waitFor(() => expect(screen.getByTestId("nr-computer-card")).toHaveTextContent(counts));
    broken = "/Users/alice/b";
    rerender(card("/Users/alice/b"));
    await waitFor(() => expect(screen.getByTestId("nr-computer-card")).not.toHaveTextContent(counts));
    expect(screen.getByText("—")).toBeInTheDocument();
  });
});
