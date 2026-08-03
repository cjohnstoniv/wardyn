/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { DeleteConfirmDialog } from "./delete-confirm-dialog";
import { OperatorProvider } from "./operator-context";

// This one dialog is the confirm step for every delete in the console
// (secret/policy/workspace/SCM host/credential) — gating it here is the
// chokepoint every screen's "Delete" trigger inherits.
describe("DeleteConfirmDialog — role-aware confirm", () => {
  it("operator (default, no provider needed): the confirm button works and deletes", async () => {
    const user = userEvent.setup();
    const onDelete = vi.fn().mockResolvedValue(undefined);
    render(
      <DeleteConfirmDialog
        name="anthropic-api-key"
        entity="secret"
        description="test"
        onOpenChange={() => {}}
        onDelete={onDelete}
        onDeleted={() => {}}
      />,
    );
    const confirm = screen.getByRole("button", { name: /delete secret/i });
    expect(confirm).not.toBeDisabled();
    await user.click(confirm);
    expect(onDelete).toHaveBeenCalled();
  });

  it("viewer: the confirm button is disabled, names the reason, and never calls onDelete", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    const onDelete = vi.fn().mockResolvedValue(undefined);
    render(
      <OperatorProvider operator={false}>
        <DeleteConfirmDialog
          name="anthropic-api-key"
          entity="secret"
          description="test"
          onOpenChange={() => {}}
          onDelete={onDelete}
          onDeleted={() => {}}
        />
      </OperatorProvider>,
    );
    expect(screen.getByText(/requires the operator role/i)).toBeInTheDocument();
    const confirm = screen.getByRole("button", { name: /delete secret/i });
    expect(confirm).toBeDisabled();
    await user.click(confirm);
    expect(onDelete).not.toHaveBeenCalled();
  });
});
