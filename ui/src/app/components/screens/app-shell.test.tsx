/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { AppShell, MobileNav } from "./app-shell";
import { ThemeProvider } from "../wardyn/theme-provider";

// below md the desktop aside is hidden, so this Sheet-based hamburger is
// the ONLY navigation. These pins fail if the drawer stops opening, drops nav
// items, or loses its aria-expanded/Escape wiring.
function renderMobileNav() {
  return render(
    <MemoryRouter>
      <MobileNav
        pendingApprovals={2}
        attentionCount={0}
        readiness="ready"
        meta={{ trustDomain: "example.test", identityProvider: "spiffe", principal: "u@example.test", method: "sso" }}
      />
    </MemoryRouter>,
  );
}

// Every background poll in the console keeps its last-good data on failure, so
// this banner is the ONLY thing separating a quiet fleet from a dead daemon —
// and the readiness chip must not report the outage as an unfinished setup.
describe("AppShell (control plane unreachable)", () => {
  afterEach(() => vi.unstubAllGlobals());

  function renderShell(unreachable: boolean) {
    const fetchMock = vi.fn().mockRejectedValue(new Error("connection refused"));
    vi.stubGlobal("fetch", fetchMock);
    render(
      <MemoryRouter>
        <ThemeProvider>
          <AppShell
            pendingApprovals={0}
            attentionCount={0}
            onSignOut={() => {}}
            unreachable={unreachable}
            lastOkAt={null}
          />
        </ThemeProvider>
      </MemoryRouter>,
    );
    return fetchMock;
  }

  it("stays silent while the daemon answers", () => {
    renderShell(false);
    expect(screen.queryByText(/Control plane unreachable/)).toBeNull();
  });

  it("banners the outage and leaves the readiness chip alone", async () => {
    const fetchMock = renderShell(true);
    expect(screen.getByText(/Control plane unreachable/)).toBeInTheDocument();
    // Its own status probe resolves the synthetic unreachable payload; the chip
    // must NOT read that as "Needs setup".
    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith("/api/v1/setup/status", expect.anything()),
    );
    expect(screen.queryByText("Needs setup")).toBeNull();
    expect(screen.getByText(/Checking/)).toBeInTheDocument();
  });
});

describe("MobileNav (below-md nav fallback)", () => {
  it("starts collapsed: trigger present, aria-expanded=false, no nav links rendered", () => {
    renderMobileNav();
    const trigger = screen.getByRole("button", { name: /open navigation menu/i });
    expect(trigger).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByRole("link", { name: "Runs" })).toBeNull();
  });

  it("opening the drawer reveals every nav item and flips aria-expanded to true", async () => {
    const user = userEvent.setup();
    renderMobileNav();
    const trigger = screen.getByRole("button", { name: /open navigation menu/i });
    await user.click(trigger);

    expect(trigger).toHaveAttribute("aria-expanded", "true");
    for (const label of ["Runs", "Approvals", "Policies", "Secrets", "Workspaces", "Audit", "Recordings", "Getting started"]) {
      expect(screen.getByRole("link", { name: new RegExp(`^${label}`) })).toBeInTheDocument();
    }
  });

  it("Escape closes the drawer and returns aria-expanded to false", async () => {
    const user = userEvent.setup();
    renderMobileNav();
    const trigger = screen.getByRole("button", { name: /open navigation menu/i });
    await user.click(trigger);
    expect(trigger).toHaveAttribute("aria-expanded", "true");

    await user.keyboard("{Escape}");
    await waitFor(() => expect(trigger).toHaveAttribute("aria-expanded", "false"));
    expect(screen.queryByRole("link", { name: "Runs" })).toBeNull();
  });
});
