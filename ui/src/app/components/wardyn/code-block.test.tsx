/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { CodeBlock } from "./code-block";

describe("CodeBlock copyLabel", () => {
  it("names the Copy button, and defaults to Copy without it", () => {
    render(
      <>
        <CodeBlock text="a" copyLabel="Copy ssh config" />
        <CodeBlock text="b" />
      </>,
    );
    expect(screen.getByRole("button", { name: "Copy ssh config", hidden: true })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Copy", hidden: true })).toBeInTheDocument();
  });
});
