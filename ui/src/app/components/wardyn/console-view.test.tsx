/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { health } from "../../lib/api/health";
import { RoleProvider } from "./operator-context";
import {
  ViewAccessProvider,
  ViewGate,
  screenPath,
  viewAccess,
  viewLanding,
  viewVerdict,
  type ViewAccess,
} from "./console-view";
import { CONSOLE_VIEW, VIEW_ADMIN_TOKEN, VIEW_REFUSAL, VIEW_TO_ADMIN, VIEW_TO_USER } from "./copy/console-view";

afterEach(() => {
  vi.restoreAllMocks();
});

describe("viewAccess — who may be in which view", () => {
  const me = { method: "sso", role: "admin", memberMode: false, sso: true };
  it("an SSO admin tier holds its view in the session", () => {
    expect(viewAccess(me)).toBe("session-admin");
    expect(viewAccess({ ...me, role: "security_admin" })).toBe("session-admin");
    // The clamp reports role user; the flag is what says it is an admin.
    expect(viewAccess({ ...me, role: "user", memberMode: true })).toBe("session-user");
  });
  it("a user has only the User view, however they signed in", () => {
    expect(viewAccess({ ...me, role: "user" })).toBe("user-only");
    expect(viewAccess({ ...me, method: "token", role: "user" })).toBe("user-only");
  });
  it("the admin token is not a person on an SSO install, and is both views without one", () => {
    expect(viewAccess({ ...me, method: "token" })).toBe("admin-only");
    expect(viewAccess({ ...me, method: "token", sso: false })).toBe("url");
    expect(viewAccess({ ...me, method: "local" })).toBe("url");
  });
});

describe("viewVerdict — the §2.3 table", () => {
  it("a user on any /admin path is refused", () => {
    expect(viewVerdict("/admin/audit", "user-only")).toEqual({ kind: "refuse" });
    expect(viewVerdict("/admin", "user-only")).toEqual({ kind: "refuse" });
  });
  it("an admin in the User view is asked, never redirected, into the Admin view", () => {
    expect(viewVerdict("/admin/audit", "session-user")).toEqual({ kind: "to-admin" });
    expect(viewVerdict("/runs", "session-user")).toEqual({ kind: "pass" });
  });
  it("an admin in the Admin view is redirected to a page's twin", () => {
    expect(viewVerdict("/runs/abc-123", "session-admin")).toEqual({ kind: "twin", to: "/admin/runs/abc-123" });
    expect(viewVerdict("/runs", "session-admin")).toEqual({ kind: "twin", to: "/admin/runs" });
    expect(viewVerdict("/approvals", "session-admin")).toEqual({ kind: "twin", to: "/admin/approvals" });
    expect(viewVerdict("/workspaces/w1/", "session-admin")).toEqual({ kind: "twin", to: "/admin/workspaces/w1" });
    expect(viewVerdict("/secrets", "session-admin")).toEqual({ kind: "twin", to: "/admin/secrets" });
  });
  it("an admin in the Admin view is asked into the User view for pages with no twin", () => {
    for (const p of ["/runs/new", "/account", "/setup"]) {
      expect(viewVerdict(p, "session-admin")).toEqual({ kind: "to-user" });
    }
  });
  it("the admin token gets its own page on every User-view page", () => {
    for (const p of ["/runs", "/runs/new", "/runs/x", "/account", "/setup"]) {
      expect(viewVerdict(p, "admin-only")).toEqual({ kind: "admin-token" });
    }
    expect(viewVerdict("/admin/audit", "admin-only")).toEqual({ kind: "pass" });
  });
  it("neg: a single-operator install is never stopped — the URL is the view", () => {
    for (const p of ["/admin/audit", "/runs", "/runs/new", "/setup", "/admin/runs/x"]) {
      expect(viewVerdict(p, "url")).toEqual({ kind: "pass" });
    }
  });
  it("neg: pre-split paths M-1b deletes are left alone", () => {
    for (const a of ["session-admin", "admin-only", "user-only"] as ViewAccess[]) {
      expect(viewVerdict("/policies", a)).toEqual({ kind: "pass" });
      expect(viewVerdict("/ssh-keys", a)).toEqual({ kind: "pass" });
    }
  });
});

describe("viewLanding — where / lands", () => {
  it("an SSO admin lands in the Admin view; a user lands as today", () => {
    expect(viewLanding("/runs", "session-admin")).toBe("/admin/runs");
    expect(viewLanding("/setup", "session-admin")).toBe("/admin/setup");
    expect(viewLanding("/setup", "admin-only")).toBe("/admin/setup");
    expect(viewLanding("/setup", "user-only")).toBe("/setup");
    expect(viewLanding("/setup", "session-user")).toBe("/setup");
  });
  it("D1: a single-operator install is in the Admin view until setup is done, then the User view", () => {
    expect(viewLanding("/setup", "url")).toBe("/admin/setup");
    expect(viewLanding("/runs", "url")).toBe("/runs");
  });
});

it("screenPath strips the view prefix and nothing else", () => {
  expect(screenPath("/admin/runs")).toBe("/runs");
  expect(screenPath("/admin")).toBe("/");
  expect(screenPath("/administrators")).toBe("/administrators");
  expect(screenPath("/runs")).toBe("/runs");
});

function Where() {
  const l = useLocation();
  return <div data-testid="where">{l.pathname + l.search}</div>;
}

function mount(path: string, access: ViewAccess, roleResolved = true) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <RoleProvider role="admin" roleResolved={roleResolved}>
        <ViewAccessProvider value={access}>
          <Routes>
            <Route element={<ViewGate fallback={<div>loading</div>} />}>
              <Route path="*" element={<div>screen</div>} />
            </Route>
          </Routes>
          <Where />
        </ViewAccessProvider>
      </RoleProvider>
    </MemoryRouter>,
  );
}

describe("ViewGate", () => {
  it("waits for the real role before deciding anything", () => {
    mount("/admin/audit", "user-only", false);
    expect(screen.getByText("loading")).toBeInTheDocument();
    expect(screen.queryByText(VIEW_REFUSAL.BODY)).toBeNull();
  });

  it("refuses a user on /admin without mounting the screen, and sends them to their runs", async () => {
    mount("/admin/audit", "user-only");
    expect(screen.getByRole("heading", { name: VIEW_REFUSAL.TITLE })).toBeInTheDocument();
    expect(screen.getByText(VIEW_REFUSAL.BODY)).toBeInTheDocument();
    expect(screen.queryByText("screen")).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: VIEW_REFUSAL.CTA }));
    expect(screen.getByTestId("where")).toHaveTextContent("/runs");
  });

  it("redirects to the twin, keeping the query", () => {
    mount("/runs/abc?tab=log", "session-admin");
    expect(screen.getByTestId("where")).toHaveTextContent("/admin/runs/abc?tab=log");
    expect(screen.getByText("screen")).toBeInTheDocument();
  });

  it("switching to the Admin view clears the clamp, then reloads the same page", async () => {
    const setMode = vi.spyOn(health, "setMemberMode").mockResolvedValue(undefined);
    const assign = vi.fn();
    vi.spyOn(window, "location", "get").mockReturnValue({ ...window.location, assign });
    mount("/admin/audit?x=1", "session-user");
    expect(screen.getByRole("heading", { name: VIEW_TO_ADMIN.TITLE })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: VIEW_TO_ADMIN.GO }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith("/admin/audit?x=1"));
    expect(setMode).toHaveBeenCalledWith(false);
  });

  it("a failed switch says so and stays put", async () => {
    vi.spyOn(health, "setMemberMode").mockRejectedValue(new Error("403"));
    mount("/runs/new", "session-admin");
    expect(screen.getByRole("heading", { name: VIEW_TO_USER.TITLE })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: VIEW_TO_USER.GO }));
    expect(await screen.findByRole("alert")).toHaveTextContent(CONSOLE_VIEW.SWITCH_FAILED);
    expect(screen.getByRole("button", { name: VIEW_TO_USER.GO })).toBeEnabled();
  });

  it("staying in the Admin view goes to the Admin view's runs", async () => {
    mount("/setup", "session-admin");
    await userEvent.click(screen.getByRole("button", { name: VIEW_TO_USER.STAY }));
    expect(screen.getByTestId("where")).toHaveTextContent("/admin/runs");
  });

  it("the admin token is told a user page belongs to a person", async () => {
    mount("/runs", "admin-only");
    expect(screen.getByText(VIEW_ADMIN_TOKEN.BODY)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: VIEW_ADMIN_TOKEN.CTA }));
    expect(screen.getByTestId("where")).toHaveTextContent("/admin");
  });
});
