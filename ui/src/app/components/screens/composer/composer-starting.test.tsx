/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { ASSISTANT as T } from "../../wardyn/copy/assistant";
import { ComposerStarting } from "./composer-starting";

afterEach(cleanup);

const show = (props: React.ComponentProps<typeof ComposerStarting>) =>
  render(
    <MemoryRouter>
      <ComposerStarting {...props} />
    </MemoryRouter>,
  );

describe("ComposerStarting", () => {
  it("says the assistant is starting, under what policy, and links the run once there is one", () => {
    show({ provider: "Org provider", runId: "r-1", fetching: false });
    expect(screen.getByText(T.STARTING)).toBeInTheDocument();
    expect(screen.getByText(T.RUN_POLICY("Org provider"))).toBeInTheDocument();
    expect(screen.getByRole("link", { name: T.OPEN_RUN })).toHaveAttribute("href", "/runs/r-1");
    expect(screen.queryByText(T.FETCHING)).toBeNull();
  });

  it("shows the build-file fetch first, and no run link before the run exists", () => {
    show({ provider: "Org provider", fetching: true });
    expect(screen.getByText(T.FETCHING)).toBeInTheDocument();
    expect(screen.getByText(T.FETCH_LINE)).toBeInTheDocument();
    expect(screen.queryByRole("link")).toBeNull();
  });
});
