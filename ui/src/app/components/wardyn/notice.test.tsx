/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { Notice } from "./notice";

describe("Notice", () => {
  it("shows its words with a glyph for every tone, and the glyph is decorative", () => {
    for (const tone of ["info", "warning", "danger", "success"] as const) {
      const { container, unmount } = render(<Notice tone={tone} title="Heads up">Body {tone}</Notice>);
      expect(screen.getByText("Heads up")).toBeVisible();
      expect(screen.getByText(`Body ${tone}`)).toBeVisible();
      expect(container.querySelector("svg")).toHaveAttribute("aria-hidden", "true");
      expect(container.firstElementChild).toHaveClass(`bg-${tone}-subtle`);
      unmount();
    }
  });

  it("is a status region only when live", () => {
    const { rerender } = render(<Notice tone="info">Quiet</Notice>);
    expect(screen.queryByRole("status")).toBeNull();
    rerender(<Notice tone="info" live>Quiet</Notice>);
    expect(screen.getByRole("status")).toHaveTextContent("Quiet");
  });
});
