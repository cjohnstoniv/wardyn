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
import { ViewSwitch, UserViewDroppedNotice, UserViewEyebrow, type ViewUserType } from "./view-switch";
import { currentView, viewTarget, type ConsoleView, type ViewAccess } from "./console-view";
import { CONSOLE_VIEW, VIEW_DROPPED } from "./copy/console-view";

const STANDARD: ViewUserType = { id: "standard", name: "Standard user" };
const PM: ViewUserType = { id: "pm", name: "Portfolio manager" };
const DEV: ViewUserType = { id: "dev", name: "Developer" };

afterEach(() => {
  vi.restoreAllMocks();
});

function Where() {
  return <div data-testid="where">{useLocation().pathname}</div>;
}

function Dirty() {
  useUnsavedGuard("view-switch-test", true, () => "unsaved text");
  return null;
}

function mount(
  access: ViewAccess,
  view: ConsoleView,
  {
    dirty = false,
    currentUserType,
    preselectType,
    userTypes = [],
  }: {
    dirty?: boolean;
    currentUserType?: { id: string; name: string } | null;
    preselectType?: string;
    userTypes?: ViewUserType[];
  } = {},
) {
  const assign = vi.fn();
  vi.spyOn(window, "location", "get").mockReturnValue({ ...window.location, assign });
  render(
    <MemoryRouter initialEntries={[view === "admin" ? "/admin/runs" : "/runs"]}>
      <UnsavedGuardProvider>
        {dirty && <Dirty />}
        <ViewSwitch
          access={access}
          view={view}
          currentUserType={currentUserType}
          preselectType={preselectType}
          userTypes={userTypes}
        />
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
    // #912: the plain toggle (no type picker here — this mount has no types)
    // names no type, forwarded through as the explicit third argument.
    expect(setMode).toHaveBeenCalledWith(true, false, undefined);
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

describe("ViewSwitch — the type picker (#912)", () => {
  it("with two or more types, the User segment opens a menu; picking one enters as that type", async () => {
    const setMode = vi.spyOn(health, "setMemberMode").mockResolvedValue(undefined);
    const { assign } = mount("session-admin", "admin", { userTypes: [STANDARD, PM, DEV] });
    await userEvent.click(seg(CONSOLE_VIEW.USER));
    await userEvent.click(await screen.findByRole("menuitem", { name: DEV.name }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith("/runs"));
    expect(setMode).toHaveBeenCalledWith(true, false, "dev");
  });

  it("picking the preselected type from Admin view still enters (not a dead click)", async () => {
    // Pinning test: a controlled radio-style menu where the "checked" item
    // never fires on re-selection would make the preselected item a dead
    // click from here — the admin could never confirm the very choice the
    // dropdown shows them first.
    const setMode = vi.spyOn(health, "setMemberMode").mockResolvedValue(undefined);
    const { assign } = mount("session-admin", "admin", { userTypes: [STANDARD, PM], preselectType: "pm" });
    await userEvent.click(seg(CONSOLE_VIEW.USER));
    await userEvent.click(await screen.findByRole("menuitem", { name: PM.name }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith("/runs"));
    expect(setMode).toHaveBeenCalledWith(true, false, "pm");
  });

  it("reselecting the type already being viewed as is a no-op", async () => {
    const setMode = vi.spyOn(health, "setMemberMode").mockResolvedValue(undefined);
    mount("session-user", "user", { userTypes: [STANDARD, PM, DEV], currentUserType: { id: "pm", name: PM.name } });
    await userEvent.click(seg(CONSOLE_VIEW.USER));
    await userEvent.click(await screen.findByRole("menuitem", { name: PM.name }));
    expect(setMode).not.toHaveBeenCalled();
  });

  it("with only the built-in type, the User segment stays the plain toggle — no menu", () => {
    mount("session-user", "user", { userTypes: [STANDARD], currentUserType: { id: "standard", name: STANDARD.name } });
    expect(seg(CONSOLE_VIEW.USER)).not.toHaveAttribute("aria-haspopup");
  });
});

describe("UserViewEyebrow (#912)", () => {
  it("renders nothing with only the built-in type", () => {
    render(<UserViewEyebrow currentUserType={{ id: "standard", name: STANDARD.name }} userTypes={[STANDARD]} />);
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("names the current type and reopens the picker to change it", async () => {
    const setMode = vi.spyOn(health, "setMemberMode").mockResolvedValue(undefined);
    const assign = vi.fn();
    vi.spyOn(window, "location", "get").mockReturnValue({ ...window.location, assign });
    render(<UserViewEyebrow currentUserType={{ id: "pm", name: PM.name }} userTypes={[STANDARD, PM, DEV]} />);
    const trigger = screen.getByRole("button", { name: CONSOLE_VIEW.EYEBROW_USER(PM.name) });
    await userEvent.click(trigger);
    await userEvent.click(await screen.findByRole("menuitem", { name: DEV.name }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith("/runs"));
    expect(setMode).toHaveBeenCalledWith(true, false, "dev");
  });
});

describe("UserViewDroppedNotice (#912)", () => {
  it("renders nothing without a drop", () => {
    const { container } = render(<UserViewDroppedNotice access="session-admin" dropped={null} userTypes={[]} />);
    expect(container).toBeEmptyDOMElement();
  });

  it("shows the type's NAME, not its id, and offers a real way to choose another", async () => {
    const setMode = vi.spyOn(health, "setMemberMode").mockResolvedValue(undefined);
    const assign = vi.fn();
    vi.spyOn(window, "location", "get").mockReturnValue({ ...window.location, assign });
    render(
      <UserViewDroppedNotice
        access="session-admin"
        dropped={{ user_type: "contractor", user_type_name: "Contractor" }}
        userTypes={[STANDARD, PM]}
      />,
    );
    const status = screen.getByRole("status");
    expect(status).toHaveTextContent(VIEW_DROPPED.BODY("Contractor"));
    expect(status).not.toHaveTextContent("contractor.");
    await userEvent.click(screen.getByRole("button", { name: VIEW_DROPPED.CHOOSE_ANOTHER }));
    await userEvent.click(await screen.findByRole("menuitem", { name: PM.name }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith("/runs"));
    expect(setMode).toHaveBeenCalledWith(true, false, "pm");
  });

  it("falls back to the id when no cached name is available", () => {
    render(<UserViewDroppedNotice access="session-admin" dropped={{ user_type: "contractor" }} userTypes={[]} />);
    expect(screen.getByRole("status")).toHaveTextContent(VIEW_DROPPED.BODY("contractor"));
  });

  it("Stay in the Admin view dismisses it", async () => {
    render(<UserViewDroppedNotice access="session-admin" dropped={{ user_type: "contractor" }} userTypes={[]} />);
    await userEvent.click(screen.getByRole("button", { name: VIEW_DROPPED.STAY }));
    expect(screen.queryByRole("status")).toBeNull();
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
    expect(viewTarget("user", "/admin/runs/abc", "?tab=recording#t")).toBe("/runs/abc?tab=recording#t");
    expect(viewTarget("user", "/admin/policies", "?x=1")).toBe("/runs");
  });
});
