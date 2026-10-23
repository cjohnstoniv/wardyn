/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The erase dialog offers every scope the server erases (internal/erasure), so a scope added there
// fails here until the console can ask for it.
import { describe, it, expect, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";

const erasePersonMock = vi.fn();
vi.mock("../../lib/api/people", () => ({ people: { list: () => Promise.resolve({ people: [] }) } }));
vi.mock("../../lib/api/credentials", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../lib/api/credentials")>()),
  credentials: { erasePerson: (...a: unknown[]) => erasePersonMock(...a) },
}));

import { EraseDataDialog } from "./erase-data-dialog";
import { ERASE } from "../wardyn/copy/credentials";

function repoRoot(): string {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8; i++) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    dir = dirname(dir);
  }
  throw new Error("go.mod not found walking up from " + process.cwd());
}

// Every `Name Scope = "value"` the Go erasure package declares.
const goScopes = [...readFileSync(join(repoRoot(), "internal/erasure/erasure.go"), "utf8").matchAll(/\bScope = "([a-z_]+)"/g)].map(
  (m) => m[1],
);

describe("Erase someone's data: the scopes", () => {
  it("sends every server scope once each is ticked, and leaves the three newest unticked on open", async () => {
    expect(goScopes).toHaveLength(10);
    erasePersonMock.mockResolvedValue({ person: "sub-ana", scopes: [], outcome: {} });
    render(<EraseDataDialog open onOpenChange={() => {}} onErased={() => {}} />);
    const dialog = screen.getByRole("dialog");
    for (const label of ["Run templates", "Runner pools", "Drives"]) {
      const box = within(dialog).getByRole("checkbox", { name: new RegExp(`^${label}`) });
      expect(box).not.toBeChecked();
      await userEvent.click(box);
    }
    await userEvent.click(within(dialog).getByRole("checkbox", { name: /^Recordings/ }));
    await userEvent.type(within(dialog).getByLabelText(ERASE.FIELD), "ana@example.com");
    await userEvent.click(within(dialog).getByRole("button", { name: "Erase data" }));
    expect([...erasePersonMock.mock.calls[0][1]].sort()).toEqual([...goScopes].sort());
  });
});
