/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { AttachmentRow } from "./work-group";

afterEach(cleanup);

// The row's three columns need about 250 px; a 320 px phone has about 170, so the path and the
// chips were clipped by the group card. Under the console's narrow breakpoint it stacks.
describe("Review: an attachment row on a narrow screen", () => {
  it("stacks its name, mount path and chips under the narrow breakpoint", () => {
    render(<AttachmentRow a={{ kind: "repository", name: "web", target: "/home/agent/work/web", read_only: false, starts_here: true, ref: "main" }} />);
    const row = screen.getByText("web").parentElement!;
    expect(row.className).toContain("max-[520px]:grid-cols-1");
    expect(screen.getByText("/home/agent/work/web").nextElementSibling!.className).toContain("max-[520px]:justify-start");
  });
});
