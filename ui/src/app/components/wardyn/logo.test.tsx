/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import type { Branding } from "../../lib/branding";
import Branded from "./branded";
import { WardynWordmark } from "./logo";

afterEach(cleanup);

// Below the sm breakpoint the wordmark is the only name the header's home link has: it
// must stay in the accessibility tree (visually hidden), never display:none.
describe("the compact wordmark keeps its name at narrow widths", () => {
  it("the product wordmark hides its text visually, not from assistive technology", () => {
    render(<WardynWordmark compact="sm" />);
    const name = screen.getByText("Wardyn");
    expect(name.className).toContain("sr-only");
    expect(name.className).toContain("sm:not-sr-only");
    expect(name.className).not.toMatch(/(^|\s)hidden(\s|$)/);
  });

  it("a branded bar does the same with the brand's name", () => {
    render(<Branded part="bar" brand={{ org_name: "Example Corp", name_format: "prefix" } as Branding} />);
    const name = screen.getByText(/Example Corp/);
    expect(name.className).toContain("sr-only");
    expect(name.className).not.toMatch(/(^|\s)hidden(\s|$)/);
  });
});
