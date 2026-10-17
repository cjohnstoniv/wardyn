/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The reason box stops at the server's 512-character bound (FUG-L725, FUG-L699).
import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
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
});
