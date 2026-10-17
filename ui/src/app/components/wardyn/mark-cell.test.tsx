/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A caveat mark names its residual risk as text, not only as a hover title (FR-0123).
import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";

import { CC_META } from "./cc-meta";
import { RESIDUAL_PREFIX } from "./copy";
import { MarkCell } from "./mark-cell";

describe("MarkCell", () => {
  it("a caveat carries its residual risk as text a screen reader reads", () => {
    render(<MarkCell mark="caveat" cc="CC1" />);
    expect(screen.getByText(`${RESIDUAL_PREFIX} ${CC_META.CC1.doesntProtect}`)).toHaveClass("sr-only");
  });
});
