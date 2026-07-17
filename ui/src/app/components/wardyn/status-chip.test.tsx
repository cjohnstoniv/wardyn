/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { StatusChip } from "./status-chip";

// U116: the `reason` used to ride ONLY in the native `title` attr — invisible to
// screen readers and unreachable without a mouse hover. It now rides in the
// chip's accessible NAME (aria-label) — announced to AT, but NOT a duplicate DOM
// text node (which double-matched getByText where a caller also renders the
// reason visibly, e.g. the confinement wizard).
describe("StatusChip AT-accessible reason (U116)", () => {
  it("announces the reason to assistive tech via aria-label, not just the title attr", () => {
    render(<StatusChip status="unavailable" reason="No KVM device on this host" />);
    // The reason rides in the chip's aria-label (announced by SRs)…
    expect(screen.getByText("Unavailable here")).toHaveAttribute(
      "aria-label",
      "Unavailable here: No KVM device on this host",
    );
    // …NOT as a separate DOM text node — so it can't double-match a reason a
    // caller also renders visibly, and can't leak into getByText/textContent.
    expect(screen.queryByText("No KVM device on this host")).not.toBeInTheDocument();
  });

  it("announces the reason on the 'optional' variant too", () => {
    render(<StatusChip status="optional" reason="Only needed for private repos" />);
    expect(screen.getByText("Optional")).toHaveAttribute(
      "aria-label",
      "Optional: Only needed for private repos",
    );
  });
});
