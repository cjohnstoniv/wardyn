/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// M-7 (admin-member-modes-design.md §4.6, QM-7) — the Admin view's switch link
// on the admin's own run follows ViewSwitch's rule: a single-operator install
// navigates, an SSO session POSTs then reloads, and a failed POST says so.
import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { health } from "../../lib/api/health";
import { OpenInUserView, ViewAccessProvider, type ViewAccess } from "./console-view";
import { CONSOLE_VIEW, OPEN_IN_USER_VIEW } from "./copy/console-view";

afterEach(() => {
  vi.restoreAllMocks();
});

function Where() {
  return <div data-testid="where">{useLocation().pathname}</div>;
}

function mount(access: ViewAccess) {
  const assign = vi.fn();
  vi.spyOn(window, "location", "get").mockReturnValue({ ...window.location, assign });
  const rowClick = vi.fn();
  render(
    <ViewAccessProvider value={access}>
      <MemoryRouter initialEntries={["/admin/runs"]}>
        <div onClick={rowClick}>
          <OpenInUserView runId="r 1" />
        </div>
        <Routes>
          <Route path="*" element={<Where />} />
        </Routes>
      </MemoryRouter>
    </ViewAccessProvider>,
  );
  return { assign, rowClick };
}

const link = () => screen.getByRole("button", { name: OPEN_IN_USER_VIEW });

describe("OpenInUserView", () => {
  it("a single-operator install navigates to the run's User-view twin, and the row's own click never fires", async () => {
    const setMode = vi.spyOn(health, "setMemberMode");
    const { assign, rowClick } = mount("url");
    await userEvent.click(link());
    expect(screen.getByTestId("where")).toHaveTextContent("/runs/r%201");
    expect(setMode).not.toHaveBeenCalled();
    expect(assign).not.toHaveBeenCalled();
    expect(rowClick).not.toHaveBeenCalled();
  });

  it("on SSO it POSTs, then reloads into that run in the User view", async () => {
    const setMode = vi.spyOn(health, "setMemberMode").mockResolvedValue(undefined);
    const { assign } = mount("session-admin");
    await userEvent.click(link());
    await waitFor(() => expect(assign).toHaveBeenCalledWith("/runs/r%201"));
    expect(setMode).toHaveBeenCalledWith(true, false);
  });

  it("a failed POST says so inline, re-enables, and does not reload", async () => {
    vi.spyOn(health, "setMemberMode").mockRejectedValue(new Error("boom"));
    const { assign } = mount("session-admin");
    await userEvent.click(link());
    expect(await screen.findByRole("alert")).toHaveTextContent(CONSOLE_VIEW.SWITCH_FAILED);
    expect(link()).toBeEnabled();
    expect(assign).not.toHaveBeenCalled();
  });

  it("the admin token has no User view, so it gets no link", () => {
    mount("admin-only");
    expect(screen.queryByRole("button", { name: OPEN_IN_USER_VIEW })).not.toBeInTheDocument();
  });
});
