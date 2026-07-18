/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { MobileNav } from "./app-shell";

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
