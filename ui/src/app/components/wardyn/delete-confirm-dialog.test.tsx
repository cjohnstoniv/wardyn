/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const toastSuccess = vi.fn();
const toastError = vi.fn();
vi.mock("sonner", () => ({ toast: { success: (...a: unknown[]) => toastSuccess(...a), error: (...a: unknown[]) => toastError(...a) } }));

import { DeleteConfirmDialog } from "./delete-confirm-dialog";
import { OperatorProvider } from "./operator-context";
import { OPERATOR_ONLY_REASON } from "./copy";

// This dialog is the confirm step for four callers: Secrets, Policies,
// Workspaces and the workspace detail page. Other screens keep bespoke
// dialogs, so this file pins this dialog only.
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
    expect(screen.getByText(/requires the admin role/i)).toBeInTheDocument();
    const confirm = screen.getByRole("button", { name: /delete secret/i });
    expect(confirm).toBeDisabled();
    await user.click(confirm);
    expect(onDelete).not.toHaveBeenCalled();
  });

  // F5-F1/X3-F2 — `allowed` lets a caller gate on ownership (useCanMutate),
  // not just the operator tier, WITHOUT this dialog knowing anything about
  // workspaces/policies/secrets. Default (omitted) is unchanged: `operator`,
  // exactly the two tests above.
  describe("allowed — ownership-aware override", () => {
    it("allowed=true under a viewer context: the confirm button still works (a member deleting their own row)", async () => {
      const user = userEvent.setup();
      const onDelete = vi.fn().mockResolvedValue(undefined);
      render(
        <OperatorProvider operator={false}>
          <DeleteConfirmDialog
            name="my-workspace"
            entity="workspace"
            description="test"
            allowed={true}
            onOpenChange={() => {}}
            onDelete={onDelete}
            onDeleted={() => {}}
          />
        </OperatorProvider>,
      );
      const confirm = screen.getByRole("button", { name: /delete workspace/i });
      expect(confirm).not.toBeDisabled();
      // R-02: canConfirm (allowed ?? operator), not the raw operator flag,
      // must gate the "admin only" reason paragraph and its aria-describedby
      // link — a member deleting a row they own (allowed=true, operator=false)
      // must not see/announce a reason that contradicts the enabled button.
      expect(screen.queryByText(OPERATOR_ONLY_REASON)).toBeNull();
      expect(confirm).not.toHaveAttribute("aria-describedby");
      await user.click(confirm);
      expect(onDelete).toHaveBeenCalled();
    });

    it("allowed=false under an operator context: the confirm button is disabled (never a flip to more permissive)", async () => {
      const user = userEvent.setup({ pointerEventsCheck: 0 });
      const onDelete = vi.fn().mockResolvedValue(undefined);
      render(
        <DeleteConfirmDialog
          name="someone-elses-workspace"
          entity="workspace"
          description="test"
          allowed={false}
          onOpenChange={() => {}}
          onDelete={onDelete}
          onDeleted={() => {}}
        />,
      );
      const confirm = screen.getByRole("button", { name: /delete workspace/i });
      expect(confirm).toBeDisabled();
      await user.click(confirm);
      expect(onDelete).not.toHaveBeenCalled();
    });
  });
});

// #1484: a second click while the first delete is pending used to call
// onDelete again (the button never disabled and nothing guarded re-entry).
describe("DeleteConfirmDialog — one delete per confirm", () => {
  beforeEach(() => {
    toastSuccess.mockReset();
    toastError.mockReset();
  });

  function setup(onDelete: () => Promise<void>, extra: Partial<React.ComponentProps<typeof DeleteConfirmDialog>> = {}) {
    const onOpenChange = vi.fn();
    const onDeleted = vi.fn();
    render(
      <DeleteConfirmDialog
        name="example"
        entity="workspace"
        description="Fixture"
        onOpenChange={onOpenChange}
        onDelete={onDelete}
        onDeleted={onDeleted}
        {...extra}
      />,
    );
    return { onOpenChange, onDeleted };
  }

  it("two clicks fired before any await make one call, and the button is disabled while it is pending", async () => {
    const user = userEvent.setup();
    let settle!: () => void;
    const onDelete = vi.fn(() => new Promise<void>((r) => (settle = r)));
    const { onDeleted } = setup(onDelete);
    const button = screen.getByRole("button", { name: "Delete workspace" });
    // Both clicks land in the same tick, before the first call's promise or
    // any re-render could disable the button.
    await act(async () => {
      button.click();
      button.click();
    });
    expect(onDelete).toHaveBeenCalledTimes(1);
    expect(button).toBeDisabled();
    await user.click(button);
    expect(onDelete).toHaveBeenCalledTimes(1);
    await act(async () => settle());
    expect(onDelete).toHaveBeenCalledTimes(1);
    expect(onDeleted).toHaveBeenCalledTimes(1);
    expect(toastSuccess).toHaveBeenCalledTimes(1);
  });

  it("Cancel, Escape and an outside close are ignored while the delete is pending", async () => {
    const user = userEvent.setup();
    let settle!: () => void;
    const onDelete = vi.fn(() => new Promise<void>((r) => (settle = r)));
    const { onOpenChange } = setup(onDelete);
    await user.click(screen.getByRole("button", { name: "Delete workspace" }));
    expect(screen.getByRole("button", { name: "Cancel" })).toBeDisabled();
    await user.keyboard("{Escape}");
    expect(onOpenChange).not.toHaveBeenCalled();
    await act(async () => settle());
  });

  it("after a failure the button re-enables, and a retry makes exactly one more call", async () => {
    const user = userEvent.setup();
    const onDelete = vi.fn().mockRejectedValueOnce(new Error("boom")).mockResolvedValue(undefined);
    const { onDeleted } = setup(onDelete);
    const button = screen.getByRole("button", { name: "Delete workspace" });
    await user.click(button);
    expect(onDelete).toHaveBeenCalledTimes(1);
    expect(toastError).toHaveBeenCalledTimes(1);
    expect(button).not.toBeDisabled();
    expect(onDeleted).not.toHaveBeenCalled();
    await user.click(button);
    expect(onDelete).toHaveBeenCalledTimes(2);
    expect(onDeleted).toHaveBeenCalledTimes(1);
  });

  it("allowed=false stays disabled after a failure (both conditions hold, neither swaps the other)", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    const onDelete = vi.fn().mockRejectedValue(new Error("boom"));
    setup(onDelete, { allowed: false });
    const button = screen.getByRole("button", { name: "Delete workspace" });
    await user.click(button);
    expect(onDelete).not.toHaveBeenCalled();
    expect(button).toBeDisabled();
  });
});

// A name made only of letters, dots and underscores has no line-break opportunity, so a 96-character secret
// name widened the dialog's auto column and pushed Cancel and Delete out of sight. jsdom has no layout, so
// this pins the two classes that keep the column at the dialog's width and let the title wrap.
describe("DeleteConfirmDialog — a very long name", () => {
  it("keeps the content to one width-bound column and lets the title break inside the name", () => {
    const name = "prod.payments.eu_west_1.stripe_api_key.service_account.rotating.primary.2026_q4_backup_copy_final";
    render(
      <DeleteConfirmDialog name={name} entity="secret" description="test" onOpenChange={() => {}} onDelete={vi.fn()} onDeleted={() => {}} />,
    );
    expect(screen.getByRole("alertdialog").className).toContain("grid-cols-[minmax(0,1fr)]");
    expect(screen.getByRole("heading", { name: new RegExp(name.replace(/\./g, "\\.")) }).className).toContain("break-words");
  });

  it("wording: a Disconnect names its verb and subject in the title, the button and the toast, not the stored name", async () => {
    const user = userEvent.setup();
    toastSuccess.mockClear();
    render(
      <DeleteConfirmDialog
        name="git-pat-github-com"
        entity="credential"
        wording={{ verb: "Disconnect", past: "disconnected", subject: "Access token for github.com" }}
        description="test"
        onOpenChange={() => {}}
        onDelete={vi.fn().mockResolvedValue(undefined)}
        onDeleted={() => {}}
      />,
    );
    expect(screen.getByRole("heading", { name: "Disconnect Access token for github.com?" })).toBeInTheDocument();
    expect(screen.queryByText(/git-pat-github-com/)).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Disconnect" }));
    expect(toastSuccess).toHaveBeenCalledWith("Access token for github.com disconnected");
  });
});
