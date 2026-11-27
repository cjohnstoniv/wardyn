/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { assistant } from "../../../../lib/api/assistant";
import { ASSISTANT as T } from "../../../wardyn/copy/assistant";
import { DroppedHosts } from "./dropped-hosts";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

// The server's entry carries a machine code too; it is never shown.
const ENTRY = { host: "artifactory.example.internal", unit: "Payments", code: "not_in_ceiling", admin_change_approvals: 2 };

describe("DroppedHosts", () => {
  it("says a left-out host in plain words and never shows the server's code", () => {
    render(<DroppedHosts items={[ENTRY]} />);
    expect(screen.getByText(T.LEFT_OUT("artifactory.example.internal", "Payments"))).toBeInTheDocument();
    expect(screen.queryByText(/not_in_ceiling/)).toBeNull();
  });

  it("opens the admin-only note with the count that admin's change would wait for, and sends nothing", async () => {
    const fetchSpy = vi.fn();
    vi.stubGlobal("fetch", fetchSpy);
    const spies = (["postEdits", "postChecks", "postMessage", "confirmBuildFiles"] as const).map((k) => vi.spyOn(assistant, k));
    render(<DroppedHosts items={[ENTRY]} />);
    expect(screen.queryByTestId("composer-admin-note")).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: T.ASK_ADMIN }));
    expect(screen.getByTestId("composer-admin-note")).toHaveTextContent(T.ADMIN_ONLY("Payments", 2));
    expect(fetchSpy).not.toHaveBeenCalled();
    for (const s of spies) expect(s).not.toHaveBeenCalled();
  });

  it("offers no note for a left-out field, which no admin can add to a ceiling", () => {
    render(<DroppedHosts items={[{ field: "image", unit: "Payments", admin_change_approvals: 2 }]} />);
    expect(screen.getByText(T.LEFT_OUT("image", "Payments"))).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: T.ASK_ADMIN })).toBeNull();
  });

  it("offers no note when the count is not known, rather than guess one", () => {
    render(<DroppedHosts items={[{ host: "x.example", unit: "Payments" }]} />);
    expect(screen.queryByRole("button", { name: T.ASK_ADMIN })).toBeNull();
  });
});
