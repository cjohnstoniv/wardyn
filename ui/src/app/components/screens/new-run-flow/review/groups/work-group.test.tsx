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

// #2107: a typed ref says it is not checked until the run starts; a picked or default ref says nothing new.
describe("Review: a typed ref", () => {
  const a = { kind: "repository" as const, name: "app", target: "/home/agent/work/app", read_only: false, starts_here: false, ref: "feature/x-typo" };

  it("shows the muted not-checked line under the row", () => {
    render(<AttachmentRow a={a} refNotChecked />);
    expect(screen.getByTestId("attachment-ref-not-checked")).toHaveTextContent("Not checked until the run starts.");
  });

  it("shows nothing new for a ref that was picked or is the default", () => {
    render(<AttachmentRow a={a} />);
    expect(screen.queryByTestId("attachment-ref-not-checked")).not.toBeInTheDocument();
  });
});
