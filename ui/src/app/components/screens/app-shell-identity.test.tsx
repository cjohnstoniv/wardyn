/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { AppShell } from "./app-shell";
import { usePrincipal } from "../wardyn/operator-context";
import { ThemeProvider } from "../wardyn/theme-provider";

// 0.7.1 — the header shows the PERSON, not the IdP's object id. For an SSO
// user /me carries `principal` (the raw OIDC sub — an Entra object id, or
// `gsv-member-0001` on the kind demo) beside `email` and `name`; the account
// chip used to render the sub. It now reads name → email → principal, and the
// sub survives as a secondary mono line in the menu, because that is the
// string OPERATIONS.md tells an admin to paste. The principal the console
// COMPARES against (PrincipalContext) must stay the sub — pinned last.
describe("AppShell — the account chip shows who you are (0.7.1)", () => {
  afterEach(() => vi.unstubAllGlobals());

  function renderShellWithMe(me: Record<string, unknown>, child?: React.ReactNode) {
    vi.stubGlobal(
      "fetch",
      vi.fn((url: RequestInfo | URL) => {
        const u = String(url);
        if (u.endsWith("/healthz")) {
          return Promise.resolve({
            ok: true,
            json: async () => ({ trust_domain: "wardyn.local", identity_provider: "dex" }),
          });
        }
        if (u.endsWith("/api/v1/me")) return Promise.resolve({ ok: true, json: async () => me });
        return Promise.resolve({ ok: true, json: async () => ({}) });
      }) as unknown as typeof fetch,
    );
    return render(
      <MemoryRouter>
        <ThemeProvider>
          <Routes>
            <Route
              element={<AppShell pendingApprovals={0} attentionCount={0} onSignOut={() => {}} />}
            >
              <Route index element={child ?? <span data-testid="page" />} />
            </Route>
          </Routes>
        </ThemeProvider>
      </MemoryRouter>,
    );
  }

  // The account-menu trigger is the last button in the header (after "New
  // run" and "Toggle theme" — see TopBar), the same locator the sibling file uses.
  function trigger() {
    const header = screen.getByRole("banner");
    const buttons = within(header).getAllByRole("button");
    return buttons[buttons.length - 1];
  }

  async function openMenu() {
    await userEvent.setup().click(trigger());
    return screen.getByRole("menu");
  }

  it("SSO with a name: the chip reads the display name, never the subject; the menu keeps the subject once", async () => {
    renderShellWithMe({
      principal: "gsv-member-0001",
      method: "sso",
      email: "alice.smith@corp.example",
      name: "Alice Smith",
      role: "member",
      operator: false,
      security_operator: false,
    });
    await waitFor(() => expect(trigger()).toHaveTextContent("Alice Smith"));
    expect(trigger()).toHaveTextContent("AS"); // initials from the name, not the sub
    // Negative control — without it this test passes on the 0.7.0 shell too.
    expect(trigger().textContent).not.toContain("gsv-member-0001");

    const menu = await openMenu();
    expect(within(menu).getByText("Alice Smith")).toBeInTheDocument();
    expect(within(menu).getAllByText("gsv-member-0001")).toHaveLength(1);
  });

  it("SSO without a name: the chip reads the email's local part and the menu shows the email over the subject", async () => {
    renderShellWithMe({
      principal: "gsv-member-0001",
      method: "sso",
      email: "alice.smith@corp.example",
      role: "member",
      operator: false,
      security_operator: false,
    });
    await waitFor(() => expect(trigger()).toHaveTextContent("alice.smith"));
    expect(trigger()).toHaveTextContent("AS");
    expect(trigger().textContent).not.toContain("gsv-member-0001");

    const menu = await openMenu();
    expect(within(menu).getByText("alice.smith@corp.example")).toBeInTheDocument();
    expect(within(menu).getAllByText("gsv-member-0001")).toHaveLength(1);
  });

  it("admin token (no email, no name — also the pre-0.7.1 daemon shape): unchanged, and the subject is not printed twice", async () => {
    renderShellWithMe({ principal: "admin-token", method: "token" });
    await waitFor(() => expect(trigger()).toHaveTextContent("admin-token"));

    const menu = await openMenu();
    // e2e/auth.spec.ts finds this button by its "admin" text — the fallback
    // lane must keep rendering the principal itself.
    expect(within(menu).getAllByText("admin-token")).toHaveLength(1);
  });

  it("local mode: unchanged", async () => {
    renderShellWithMe({ principal: "local:operator", method: "local" });
    await waitFor(() => expect(trigger()).toHaveTextContent("local:operator"));
    const menu = await openMenu();
    expect(within(menu).getAllByText("local:operator")).toHaveLength(1);
  });

  it("the principal the console compares ownership against is still the subject", async () => {
    function Probe() {
      return <span data-testid="principal-probe">{usePrincipal()}</span>;
    }
    renderShellWithMe(
      {
        principal: "gsv-member-0001",
        method: "sso",
        email: "alice.smith@corp.example",
        name: "Alice Smith",
        role: "member",
        operator: false,
        security_operator: false,
      },
      <Probe />,
    );
    // Wait for /me to settle (the chip shows the name) before reading the probe,
    // so this reads the RESOLVED value and not the seed.
    await waitFor(() => expect(trigger()).toHaveTextContent("Alice Smith"));
    expect(screen.getByTestId("principal-probe")).toHaveTextContent("gsv-member-0001");
    expect(screen.getByTestId("principal-probe").textContent).not.toContain("Alice");
  });
});
