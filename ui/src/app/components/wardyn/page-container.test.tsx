/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { PageContainer } from "./page-container";

// #3058: two admin page columns and one top padding; the phone gutter is 16 px, 24 px from sm up.
describe("PageContainer", () => {
  it("gives a list page 1200 px and a form page 900 px, both with the same top padding and gutters", () => {
    render(
      <>
        <PageContainer width="list">list</PageContainer>
        <PageContainer width="form">form</PageContainer>
      </>,
    );
    const list = screen.getByText("list");
    const form = screen.getByText("form");
    expect(list).toHaveClass("max-w-[1200px]", "pt-8", "px-4", "sm:px-6");
    expect(form).toHaveClass("max-w-[900px]", "pt-8", "px-4", "sm:px-6");
    expect(list).toHaveAttribute("data-page-width", "list");
    expect(form).toHaveAttribute("data-page-width", "form");
  });

  it("lets a page add its own spacing without losing the column", () => {
    render(<PageContainer width="list" className="space-y-5 pb-24">x</PageContainer>);
    expect(screen.getByText("x")).toHaveClass("max-w-[1200px]", "pt-8", "space-y-5", "pb-24");
    expect(screen.getByText("x")).not.toHaveClass("pb-6");
  });
});
