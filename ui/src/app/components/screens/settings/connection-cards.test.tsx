/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Model provider card. Since 0.8 (#548) the deployment's own model
// credentials it held credential no run, so it says where model access is set
// up — model providers — and offers no field, lane or sign-in of its own.
import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";

import { ModelProviderCard, S } from "./connection-cards";

describe("ModelProviderCard", () => {
  it("points at model providers and offers no credential of its own", () => {
    render(<ModelProviderCard />);
    expect(screen.getByRole("heading", { name: S.MODEL_TITLE })).toBeInTheDocument();
    expect(screen.getByText(S.MODEL_MOVED)).toBeInTheDocument();
    expect(screen.queryByRole("radiogroup")).not.toBeInTheDocument();
    expect(screen.queryByRole("textbox")).not.toBeInTheDocument();
    expect(screen.queryByRole("button")).not.toBeInTheDocument();
  });
});
