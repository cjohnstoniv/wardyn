/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The pool form registers with the shared unsaved guard while it holds edits.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

const getMock = vi.fn();
const membersMock = vi.fn();
vi.mock("../../../../lib/api/runner-pools", () => ({
  runnerPools: {
    get: (...a: unknown[]) => getMock(...a),
    members: (...a: unknown[]) => membersMock(...a),
    createMine: vi.fn(),
    updateMine: vi.fn(),
    update: vi.fn(),
  },
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

import { unsavedSnapshot } from "../../../../lib/unsaved-registry";
import { setField } from "../../../../../test/set-field";
import { pool } from "./pool-test-fixtures";
import { PoolFormScreen } from "./pool-form";

const MINE = pool({ id: "p-lap", name: "Laptop", scope: "user", owner: "dana@example.com", limits: { barriers: ["CC1", "CC2"] } });

function renderAt(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <Routes>
        <Route path="/account/pools/new" element={<PoolFormScreen scope="mine" />} />
        <Route path="/account/pools/:id/edit" element={<PoolFormScreen scope="mine" />} />
      </Routes>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  getMock.mockReset().mockResolvedValue(MINE);
  membersMock.mockReset().mockResolvedValue([]);
});
afterEach(() => cleanup());

describe("PoolFormScreen unsaved guard", () => {
  it("a new pool: clean, dirty once named, clean again when the name is cleared", async () => {
    renderAt("/account/pools/new");
    const name = await screen.findByLabelText(/^Name/);
    expect(unsavedSnapshot()).toBeNull();
    setField(name, "Build box");
    expect(unsavedSnapshot()).toContain("Build box");
    setField(name, "");
    expect(unsavedSnapshot()).toBeNull();
  });

  it("an existing pool: the async load that resets the values is not an edit; editing then restoring is", async () => {
    renderAt("/account/pools/p-lap/edit");
    const name = await screen.findByLabelText(/^Name/);
    expect(name).toHaveValue("Laptop");
    expect(unsavedSnapshot()).toBeNull();
    setField(name, "Laptop 2");
    expect(unsavedSnapshot()).not.toBeNull();
    setField(name, "Laptop");
    expect(unsavedSnapshot()).toBeNull();
  });

  it("unregisters on unmount", async () => {
    const { unmount } = renderAt("/account/pools/new");
    setField(await screen.findByLabelText(/^Name/), "Build box");
    expect(unsavedSnapshot()).not.toBeNull();
    unmount();
    expect(unsavedSnapshot()).toBeNull();
  });
});
