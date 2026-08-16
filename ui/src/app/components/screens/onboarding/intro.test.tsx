/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The readiness-derivation suites that used to live here moved with their
// subject to lib/readiness.test.ts. What remains is the one component test.
import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { HowItWorksStrip } from "./intro";

describe("HowItWorksStrip — node 5 qualifier is design-law verbatim", () => {
  it("renders the exact append-only audit qualifier string", () => {
    render(<HowItWorksStrip />);
    expect(
      screen.getByText("Append-only audit; session replay where the runner supports it"),
    ).toBeInTheDocument();
  });
});
