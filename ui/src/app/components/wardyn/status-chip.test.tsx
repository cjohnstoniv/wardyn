/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { StatusChip } from "./status-chip";

// U116: the `reason` used to ride ONLY in the native `title` attr — invisible to
// screen readers and unreachable without a mouse hover. tier-illustration.tsx's
// sr-only-twin pattern fixes this: the reason also renders as visually-hidden
// text inside the chip.
describe("StatusChip AT-accessible reason (U116)", () => {
  it("exposes the reason to assistive tech as sr-only text, not just the title attr", () => {
    render(<StatusChip status="unavailable" reason="No KVM device on this host" />);
    expect(screen.getByText("No KVM device on this host")).toBeInTheDocument();
  });

  it("exposes the reason on the 'optional' variant too", () => {
    render(<StatusChip status="optional" reason="Only needed for private repos" />);
    expect(screen.getByText("Only needed for private repos")).toBeInTheDocument();
  });
});
