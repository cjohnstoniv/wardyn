/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { RunsSegmentBar } from "./runs-segment-bar";

describe("RunsSegmentBar", () => {
  it("draws the keyboard focus outline inside its scrolling track", () => {
    render(<RunsSegmentBar value="all" counts={{ all: 4, agent_tasks: 1, environments: 1, apps: 1, services: 1 }} onChange={() => {}} />);
    for (const b of screen.getAllByRole("button")) {
      expect(b.className).toContain("focus-visible:outline-offset-[-2px]");
    }
  });
});
