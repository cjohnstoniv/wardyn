/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { health } from "../../lib/api/health";
import { UnsavedGuardProvider, useUnsavedGuard } from "../../lib/use-unsaved-guard";
import { UNSAVED_GUARD } from "./copy";
import { ViewSwitch } from "./view-switch";
import { currentView, viewTarget, type ConsoleView, type ViewAccess } from "./console-view";
import { CONSOLE_VIEW } from "./copy/console-view";

afterEach(() => {
  vi.restoreAllMocks();
});

function Where() {
  return <div data-testid="where">{useLocation().pathname}</div>;
}

function Dirty() {
  useUnsavedGuard(true);
  return null;
}

function mount(access: ViewAccess, view: ConsoleView, { dirty = false } = {}) {
  const assign = vi.fn();
  vi.spyOn(window, "location", "get").mockReturnValue({ ...window.location, assign });
  render(
    <MemoryRouter initialEntries={[view === "admin" ? "/admin/runs" : "/runs"]}>
      <UnsavedGuardProvider>
        {dirty && <Dirty />}
        <ViewSwitch access={access} view={view} />
        <Routes>
          <Route path="*" element={<Where />} />
        </Routes>
      </UnsavedGuardProvider>
    </MemoryRouter>,
  );
  return { assign };
}

const seg = (name: string) => within(screen.getByRole("group", { name: CONSOLE_VIEW.GROUP })).getByRole("button", { name });

describe("ViewSwitch", () => {
  it("is a labelled group of two pressed-or-not buttons; the pressed one is aria-disabled", () => {
    mount("session-admin", "admin");
    expect(seg(CONSOLE_VIEW.ADMIN)).toHaveAttribute("aria-pressed", "true");
    expect(seg(CONSOLE_VIEW.ADMIN)).toHaveAttribute("aria-disabled", "true");
    expect(seg(CONSOLE_VIEW.USER)).toHaveAttribute("aria-pressed", "false");
    expect(seg(CONSOLE_VIEW.USER)).not.toHaveAttribute("aria-disabled");
  });

  it("on SSO it POSTs, then reloads into the other view's home", async () => {
    const setMode = vi.spyOn(health, "setMemberMode").mockResolvedValue(undefined);
    const { assign } = mount("session-admin", "admin");
    await userEvent.click(seg(CONSOLE_VIEW.USER));
    await waitFor(() => expect(assign).toHaveBeenCalledWith("/runs"));
    expect(setMode).toHaveBeenCalledWith(true, false);
  });

  it("clicking the pressed segment does nothing", async () => {
    const setMode = vi.spyOn(health, "setMemberMode").mockResolvedValue(undefined);
    const { assign } = mount("session-user", "user");
    await userEvent.click(seg(CONSOLE_VIEW.USER));
    expect(setMode).not.toHaveBeenCalled();
    expect(assign).not.toHaveBeenCalled();
  });

  it("a failed POST says so inline, re-enables, and does not reload", async () => {
    vi.spyOn(health, "setMemberMode").mockRejectedValue(new Error("boom"));
    const { assign } = mount("session-user", "user");
    await userEvent.click(seg(CONSOLE_VIEW.ADMIN));
    expect(await screen.findByRole("alert")).toHaveTextContent(CONSOLE_VIEW.SWITCH_FAILED);
    expect(seg(CONSOLE_VIEW.ADMIN)).toBeEnabled();
    expect(assign).not.toHaveBeenCalled();
  });

  it("a single-operator install navigates only", async () => {
    const setMode = vi.spyOn(health, "setMemberMode");
    const { assign } = mount("url", "user");
    await userEvent.click(seg(CONSOLE_VIEW.ADMIN));
    expect(screen.getByTestId("where")).toHaveTextContent("/admin");
    expect(setMode).not.toHaveBeenCalled();
    expect(assign).not.toHaveBeenCalled();
  });

  it("asks the unsaved guard before the POST; Keep editing changes nothing", async () => {
    const setMode = vi.spyOn(health, "setMemberMode").mockResolvedValue(undefined);
    mount("session-admin", "admin", { dirty: true });
    await userEvent.click(seg(CONSOLE_VIEW.USER));
    expect(screen.getByRole("alertdialog")).toHaveTextContent(UNSAVED_GUARD.TITLE);
    expect(setMode).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole("button", { name: UNSAVED_GUARD.STAY }));
    expect(setMode).not.toHaveBeenCalled();
  });
});

describe("the view a page is in, and where a follower tab lands", () => {
  it("an SSO session's clamp decides, a single-operator install's URL does", () => {
    expect(currentView("session-admin", "user")).toBe("admin");
    expect(currentView("session-user", "admin")).toBe("user");
    expect(currentView("admin-only", "user")).toBe("admin");
    expect(currentView("user-only", "admin")).toBe("user");
    expect(currentView("url", "admin")).toBe("admin");
    expect(currentView("url", "user")).toBe("user");
  });

  it("the twin when there is one, else the view's home", () => {
    expect(viewTarget("user", "/admin/runs/abc")).toBe("/runs/abc");
    expect(viewTarget("user", "/admin/policies")).toBe("/runs");
    expect(viewTarget("admin", "/workspaces/w1")).toBe("/admin/workspaces/w1");
    expect(viewTarget("admin", "/runs/new")).toBe("/admin");
    expect(viewTarget("admin", "/account")).toBe("/admin");
  });
});
