/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { KillRunDialog } from "./kill-run-dialog";

const ID = "1697570d-b4cb-41d8-8d92-6600c9731d3f";

describe("KillRunDialog", () => {
  it("names the run by its title, keeps the short id beside it, and kills the id", async () => {
    const onConfirm = vi.fn();
    render(<KillRunDialog runId={ID} title="e2e fixture 2" onOpenChange={() => {}} onConfirm={onConfirm} />);
    const dialog = screen.getByRole("alertdialog", { name: 'Kill "e2e fixture 2"?' });
    expect(dialog).toHaveTextContent("1697570d");
    expect(dialog).not.toHaveTextContent(ID);
    expect(dialog).toHaveTextContent("The kill is recorded in the audit trail.");
    await userEvent.click(screen.getByRole("button", { name: "Kill run" }));
    expect(onConfirm).toHaveBeenCalledWith(ID);
  });

  it("falls back to the id when the run has no title", () => {
    render(<KillRunDialog runId={ID} onOpenChange={() => {}} onConfirm={() => {}} />);
    expect(screen.getByRole("alertdialog", { name: `Kill ${ID}?` })).toBeInTheDocument();
  });

  it("names a run by the first line of a long task, bounded", () => {
    const task = `${"a".repeat(120)}\nsecond line`;
    render(<KillRunDialog runId={ID} title={task} onOpenChange={() => {}} onConfirm={() => {}} />);
    const dialog = screen.getByRole("alertdialog", { name: `Kill "${"a".repeat(79)}…"?` });
    expect(dialog).not.toHaveTextContent("second line");
  });
});
