/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The reason box stops at the server's 512-character bound (FUG-L725, FUG-L699).
import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { unsavedSnapshot } from "../../lib/unsaved-registry";

import { ReasonDialog } from "./reason-dialog";
import { OperatorProvider } from "./operator-context";

describe("ReasonDialog", () => {
  it("bounds the reason at 512 characters, the length the server accepts", () => {
    render(
      <OperatorProvider operator securityOperator>
        <ReasonDialog prompt={{ id: "a1", action: "deny", kind: "egress_domain" }} onClose={vi.fn()} onSubmit={vi.fn()} />
      </OperatorProvider>,
    );
    expect(screen.getByLabelText(/^Reason/)).toHaveAttribute("maxlength", "512");
  });

  it("is an unsaved editor while a reason is typed, and not once it is cleared or the dialog closes (DR-0021)", async () => {
    const prompt = { id: "a1", action: "deny", kind: "egress_domain" } as const;
    const tree = (p: typeof prompt | null) => (
      <OperatorProvider operator securityOperator>
        <ReasonDialog prompt={p} onClose={vi.fn()} onSubmit={vi.fn()} />
      </OperatorProvider>
    );
    const { rerender } = render(tree(prompt));
    expect(unsavedSnapshot()).toBeNull();
    await userEvent.type(screen.getByLabelText(/^Reason/), "wrong host");
    expect(unsavedSnapshot()).toBe("wrong host");
    await userEvent.clear(screen.getByLabelText(/^Reason/));
    expect(unsavedSnapshot()).toBeNull();
    await userEvent.type(screen.getByLabelText(/^Reason/), "again");
    rerender(tree(null));
    expect(unsavedSnapshot()).toBeNull();
  });

  const egress = { id: "a1", action: "approve", kind: "egress_domain" } as const;
  const open = (securityOperator: boolean) =>
    render(
      <OperatorProvider operator securityOperator={securityOperator}>
        <ReasonDialog prompt={egress} hasWorkspace onClose={vi.fn()} onSubmit={vi.fn()} />
      </OperatorProvider>,
    );

  // The scope radios were four Tab stops with dead arrow keys.
  it("makes the decision scope one Tab stop that the arrow keys move, skipping a disabled Always", async () => {
    open(false);
    const radio = (name: RegExp) => screen.getByRole("radio", { name });
    expect(radio(/^This run/)).toHaveAttribute("tabindex", "0");
    expect(radio(/^Once/)).toHaveAttribute("tabindex", "-1");
    radio(/^This run/).focus();
    await userEvent.keyboard("{ArrowRight}");
    expect(radio(/^Until/)).toHaveFocus();
    expect(radio(/^Until/)).toHaveAttribute("aria-checked", "true");
    // Always is disabled for a non-security operator: the next arrow wraps past it to Once.
    await userEvent.keyboard("{ArrowRight}");
    expect(radio(/^Once/)).toHaveFocus();
    await userEvent.keyboard("{ArrowLeft}");
    expect(radio(/^Until/)).toHaveFocus();
  });

  it("does not let the Until time field's own arrow keys move the scope", async () => {
    open(true);
    await userEvent.click(screen.getByRole("radio", { name: /^Until/ }));
    const field = screen.getByLabelText("Pick a time");
    field.focus();
    await userEvent.keyboard("{ArrowRight}");
    expect(screen.getByRole("radio", { name: /^Until/ })).toHaveAttribute("aria-checked", "true");
  });

  // A preset showed no selected state, and the time field kept a stale typed value after one.
  it("marks the chosen Until preset and shows its time in the one date field", async () => {
    open(true);
    await userEvent.click(screen.getByRole("radio", { name: /^Until/ }));
    const field = screen.getByLabelText("Pick a time") as HTMLInputElement;
    expect(field.value).toBe("");
    const hour = screen.getByRole("button", { name: "1 hour" });
    expect(hour).toHaveAttribute("aria-pressed", "false");
    await userEvent.click(hour);
    expect(hour).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("button", { name: "15 minutes" })).toHaveAttribute("aria-pressed", "false");
    const shown = new Date(field.value).getTime();
    expect(Math.abs(shown - (Date.now() + 3_600_000))).toBeLessThan(2 * 60_000);

    await userEvent.click(screen.getByRole("button", { name: "24 hours" }));
    expect(hour).toHaveAttribute("aria-pressed", "false");
    expect(Math.abs(new Date(field.value).getTime() - (Date.now() + 86_400_000))).toBeLessThan(2 * 60_000);

    fireEvent.change(field, { target: { value: "2031-01-02T03:04" } });
    expect(field.value).toBe("2031-01-02T03:04");
    expect(screen.getByRole("button", { name: "24 hours" })).toHaveAttribute("aria-pressed", "false");
  });
});
