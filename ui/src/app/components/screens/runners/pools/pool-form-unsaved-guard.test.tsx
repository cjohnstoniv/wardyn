/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The pool form registers with the shared unsaved guard while it holds edits.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";

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
import { UnsavedGuardProvider } from "../../../../lib/use-unsaved-guard";
import { UNSAVED } from "../../../../lib/unsaved-copy";
import { POOL_FORM } from "../../../wardyn/copy/runner-pools";
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

// The screen's own Back navigated with no question: the shared guard sees link clicks and the browser's Back only.
describe("PoolFormScreen Back with unsaved edits", () => {
  function Where() {
    return <span data-testid="where">{useLocation().pathname}</span>;
  }

  it("asks first, stays on Keep editing, and leaves on Discard", async () => {
    const user = userEvent.setup();
    render(
      <UnsavedGuardProvider>
        <MemoryRouter initialEntries={["/account/pools/new"]}>
          <Routes>
            <Route path="/account/pools/new" element={<PoolFormScreen scope="mine" />} />
            <Route path="/account" element={null} />
          </Routes>
          <Where />
        </MemoryRouter>
      </UnsavedGuardProvider>,
    );
    setField(await screen.findByLabelText(/^Name/), "Build box");
    await user.click(screen.getByRole("button", { name: POOL_FORM.BACK_MINE }));
    const ask = await screen.findByRole("alertdialog", { name: UNSAVED.TITLE });
    expect(screen.getByTestId("where")).toHaveTextContent("/account/pools/new");
    await user.click(within(ask).getByRole("button", { name: UNSAVED.STAY }));
    expect(screen.getByTestId("where")).toHaveTextContent("/account/pools/new");

    await user.click(screen.getByRole("button", { name: POOL_FORM.BACK_MINE }));
    await user.click(within(await screen.findByRole("alertdialog", { name: UNSAVED.TITLE })).getByRole("button", { name: UNSAVED.DISCARD }));
    expect(screen.getByTestId("where")).toHaveTextContent(/^\/account$/);
  });
});
