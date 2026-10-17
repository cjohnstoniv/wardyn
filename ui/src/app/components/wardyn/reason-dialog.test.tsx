/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The reason box stops at the server's 512-character bound (FUG-L725, FUG-L699).
import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";

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
});
