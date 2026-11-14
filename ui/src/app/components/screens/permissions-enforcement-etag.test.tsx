/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Permissions screen sends the loaded map's ETag with every enforcement save. These pins cover what happens
// around it: a refused save, a reload that fails, a double click, and the ETag a save hands to the next one.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

const toastError = vi.fn();
vi.mock("sonner", () => ({
  toast: { error: (...a: unknown[]) => toastError(...a), success: vi.fn(), warning: vi.fn() },
}));

const getPermissionsMock = vi.fn();
const putEnforcementMock = vi.fn();
vi.mock("../../lib/api/permissions", () => ({
  permissions: {
    getPermissions: () => getPermissionsMock(),
    putEnforcement: (...a: unknown[]) => putEnforcementMock(...a),
  },
}));
vi.mock("../../lib/api/people", () => ({ people: { list: () => Promise.resolve({ people: [] }) } }));
vi.mock("../../lib/api/user-types", () => ({ userTypes: { listUserTypes: () => Promise.resolve([]) } }));

import { CAPABILITY_KINDS, KIND, PERM } from "../../lib/permissions-copy";
import { HttpError } from "../../lib/api/core";
import { PermissionsScreen } from "./permissions";

const STALE = new HttpError(412, "If-Match does not match", "capability_enforcement_stale");
const onlyOn = (...on: string[]) => Object.fromEntries(CAPABILITY_KINDS.map((k) => [k, on.includes(k)]));

async function openConfirm(user: ReturnType<typeof userEvent.setup>, kind: "feature" | "egress_host") {
  await user.click(screen.getByRole("switch", { name: `${PERM.ENFORCEMENT_TITLE} ${KIND[kind].label}` }));
  return screen.findByRole("alertdialog");
}
const confirmButton = (dialog: HTMLElement) => within(dialog).getByRole("button", { name: PERM.ENFORCE_CONFIRM });

beforeEach(() => {
  cleanup();
  toastError.mockClear();
  getPermissionsMock.mockReset().mockResolvedValue({ grants: [], enforcement: {}, etag: '"e1"' });
  putEnforcementMock.mockReset();
});

describe("PermissionsScreen — enforcement save and the loaded map", () => {
  it("a refused save says what to do, and a reload that fails leaves Confirm unable to write", async () => {
    putEnforcementMock.mockRejectedValueOnce(STALE);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(<MemoryRouter><PermissionsScreen /></MemoryRouter>);
    await screen.findByText(KIND.feature.unenforced);

    getPermissionsMock.mockRejectedValue(new HttpError(500, "boom"));
    await user.click(confirmButton(await openConfirm(user, "feature")));
    await waitFor(() => expect(toastError).toHaveBeenCalledWith("Failed to save enforcement", { description: PERM.ENFORCE_STALE }));
    await waitFor(() => expect(getPermissionsMock).toHaveBeenCalledTimes(2));

    await user.click(confirmButton(screen.getByRole("alertdialog")));
    expect(putEnforcementMock).toHaveBeenCalledTimes(1);
  });

  it("the other 412 on this save, a changed person, keeps its own sentence and does not reload", async () => {
    putEnforcementMock.mockRejectedValueOnce(new HttpError(412, "different person", "expected_principal_mismatch"));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(<MemoryRouter><PermissionsScreen /></MemoryRouter>);
    await screen.findByText(KIND.feature.unenforced);

    await user.click(confirmButton(await openConfirm(user, "feature")));
    await waitFor(() => expect(toastError).toHaveBeenCalledWith("Failed to save enforcement", { description: "different person" }));
    expect(getPermissionsMock).toHaveBeenCalledTimes(1);
  });

  it("a second Confirm while the save is in flight sends nothing", async () => {
    let finish: (v: unknown) => void = () => {};
    putEnforcementMock.mockReturnValueOnce(new Promise((r) => (finish = r)));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(<MemoryRouter><PermissionsScreen /></MemoryRouter>);
    await screen.findByText(KIND.feature.unenforced);

    const dialog = await openConfirm(user, "feature");
    await user.click(confirmButton(dialog));
    await user.click(confirmButton(dialog));
    expect(putEnforcementMock).toHaveBeenCalledTimes(1);
    finish({ enforcement: { feature: true }, etag: '"e2"' });
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
  });

  it("the next save sends the ETag the last save returned, not the one the page loaded", async () => {
    putEnforcementMock.mockResolvedValueOnce({ enforcement: { feature: true }, etag: '"e2"' });
    putEnforcementMock.mockResolvedValueOnce({ enforcement: { feature: true, egress_host: true }, etag: '"e3"' });
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(<MemoryRouter><PermissionsScreen /></MemoryRouter>);
    await screen.findByText(KIND.feature.unenforced);

    await user.click(confirmButton(await openConfirm(user, "feature")));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    await user.click(confirmButton(await openConfirm(user, "egress_host")));

    await waitFor(() => expect(putEnforcementMock).toHaveBeenCalledTimes(2));
    expect(putEnforcementMock).toHaveBeenNthCalledWith(1, onlyOn("feature"), '"e1"');
    expect(putEnforcementMock).toHaveBeenNthCalledWith(2, onlyOn("feature", "egress_host"), '"e2"');
  });

  it("Confirm while the reload after a refused save is still in flight sends nothing", async () => {
    putEnforcementMock.mockRejectedValueOnce(STALE);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(<MemoryRouter><PermissionsScreen /></MemoryRouter>);
    await screen.findByText(KIND.feature.unenforced);

    getPermissionsMock.mockReturnValue(new Promise(() => {}));
    await user.click(confirmButton(await openConfirm(user, "feature")));
    await waitFor(() => expect(getPermissionsMock).toHaveBeenCalledTimes(2));

    await user.click(confirmButton(screen.getByRole("alertdialog")));
    expect(putEnforcementMock).toHaveBeenCalledTimes(1);
  });
});
