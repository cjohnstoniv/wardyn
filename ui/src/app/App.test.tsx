/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { RequireSetupComplete } from "./App";

// ui-shellAuth-1: the first-run gate must not bounce a route it doesn't
// actually own back to /setup. /ssh-keys is a personal-credential screen
// (see ssh-keys.tsx's own header comment) that the account menu always links
// to, gate or no gate — so it needs the same exemption /demos and
// /integrations already have.
function renderGateAt(path: string, gated: boolean) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <Routes>
        <Route element={<RequireSetupComplete gated={gated} />}>
          <Route path="/setup" element={<div>Setup screen</div>} />
          <Route path="/demos" element={<div>Demos screen</div>} />
          <Route path="/ssh-keys" element={<div>SSH keys screen</div>} />
          <Route path="/runs" element={<div>Runs screen</div>} />
        </Route>
      </Routes>
    </MemoryRouter>,
  );
}

describe("RequireSetupComplete — first-run gate exemptions", () => {
  it("does not redirect /ssh-keys away while the gate is active", () => {
    renderGateAt("/ssh-keys", true);
    expect(screen.getByText("SSH keys screen")).toBeInTheDocument();
  });

  it("still redirects an ungated route (e.g. /runs) to /setup while gated", () => {
    renderGateAt("/runs", true);
    expect(screen.getByText("Setup screen")).toBeInTheDocument();
    expect(screen.queryByText("Runs screen")).toBeNull();
  });

  it("lets every route through once the gate is inactive", () => {
    renderGateAt("/runs", false);
    expect(screen.getByText("Runs screen")).toBeInTheDocument();
  });
});
