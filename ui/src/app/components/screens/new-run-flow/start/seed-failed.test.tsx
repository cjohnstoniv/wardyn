/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HttpError } from "../../../../lib/api/core";
import { NR_TEMPLATES } from "../../../wardyn/copy/new-run/templates";
import { SeedFailed } from "./seed-failed";

describe("SeedFailed", () => {
  it("shows the server's sentence and starts an empty draft from its one action", async () => {
    const startFromScratch = vi.fn();
    render(<SeedFailed location={{ screen: "start" }} go={vi.fn()} seedError={new HttpError(404, "That template is gone.")} startFromScratch={startFromScratch} />);
    expect(screen.getByRole("alert")).toHaveTextContent("That template is gone.");
    const button = screen.getByRole("button", { name: NR_TEMPLATES.NEW_FROM_SCRATCH });
    expect(button).toHaveFocus();
    await userEvent.click(button);
    expect(startFromScratch).toHaveBeenCalledTimes(1);
  });
});
