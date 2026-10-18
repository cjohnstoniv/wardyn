/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { CopyPill } from "./record-pane-chips";

// #2843: a long command wraps inside its pill, left-aligned, and never pushes
// the pill past the container's edge.
describe("CopyPill", () => {
  it("is capped at the container, left-aligned, and breaks long commands", () => {
    const cmd = `curl -sS --noproxy '*' "$WARDYN_PROXY_URL/wardyn/v1/secrets/wardyn-demo-key"`;
    render(<CopyPill text={cmd} />);
    const pill = screen.getByRole("button", { name: "Copy command" });
    expect(pill.className).toContain("max-w-full");
    expect(pill.className).toContain("text-left");
    expect(screen.getByText(cmd).className).toContain("break-all");
  });
});
