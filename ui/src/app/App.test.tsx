/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// FirstRunLanding (Phase 5) now waits for BOTH the setup-status fetch AND the
// real role to resolve before it ever navigates — a member landed here under
// the role context's fail-open "admin" default would be routed by the wrong
// rule (App.tsx's own comment on the component explains why). This suite
// drives it directly with a stub RoleProvider rather than the whole App, since
// App's own auth/health polling has nothing to do with this decision.
import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { FirstRunLanding } from "./App";
import { RoleProvider, type Role } from "./components/wardyn/operator-context";
import { baseStatus } from "./lib/test-fixtures";
import type { SetupStatus } from "./lib/types";

function renderLanding(role: Role, roleResolved: boolean, status: SetupStatus | null) {
  return render(
    <MemoryRouter initialEntries={["/"]}>
      <RoleProvider role={role} roleResolved={roleResolved}>
        <Routes>
          <Route path="/" element={<FirstRunLanding status={status} />} />
          <Route path="/setup" element={<div>setup screen</div>} />
          <Route path="/runs" element={<div>runs screen</div>} />
        </Routes>
      </RoleProvider>
    </MemoryRouter>,
  );
}

describe("FirstRunLanding — waits for both status and role", () => {
  // Negative control: status is in, but the role hasn't resolved yet — must
  // not navigate at all (neither route's content renders).
  it("status resolved + role unresolved navigates nowhere", () => {
    renderLanding("member", false, baseStatus({ has_runs: false }));
    expect(screen.queryByText("setup screen")).toBeNull();
    expect(screen.queryByText("runs screen")).toBeNull();
  });

  it("navigates once role resolves — an unseen member lands on their own Getting Started", () => {
    renderLanding("member", true, baseStatus({ has_runs: false }));
    expect(screen.getByText("setup screen")).toBeInTheDocument();
  });

  it("admin is unaffected — the admin has_runs rule still applies once resolved", () => {
    renderLanding("admin", true, baseStatus({ has_runs: true }));
    expect(screen.getByText("runs screen")).toBeInTheDocument();
  });
});
