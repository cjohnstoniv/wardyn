/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { KillRunDialog } from "./kill-run-dialog";

const ID = "1697570d-b4cb-41d8-8d92-6600c9731d3f";

describe("KillRunDialog", () => {
  it("names the run by its title, keeps the short id beside it, and kills the id", async () => {
    const onConfirm = vi.fn();
    render(<KillRunDialog runId={ID} title="e2e fixture 2" onOpenChange={() => {}} onConfirm={onConfirm} />);
    const dialog = screen.getByRole("alertdialog", { name: "End e2e fixture 2?" });
    expect(dialog).toHaveTextContent("1697570d");
    expect(dialog).not.toHaveTextContent(ID);
    // #3036: the short id is the run reference's, so it copies the whole id.
    expect(within(dialog).getByRole("button", { name: "Copy run id 1697570d" })).toHaveAttribute("title", `${ID}\nClick to copy`);
    expect(dialog).toHaveTextContent("Ending the run is recorded in the audit trail.");
    await userEvent.click(screen.getByRole("button", { name: "End run" }));
    expect(onConfirm).toHaveBeenCalledWith(ID);
  });

  it("says End this run, never an id, when the run has no title", () => {
    render(<KillRunDialog runId={ID} onOpenChange={() => {}} onConfirm={() => {}} />);
    const dialog = screen.getByRole("alertdialog", { name: "End this run?" });
    expect(dialog).not.toHaveTextContent(ID);
    expect(dialog).not.toHaveTextContent("1697570d?");
  });

  it("says a Both ways folder does not come back, only when the run has one and it has started", () => {
    const { rerender } = render(<KillRunDialog runId={ID} title="t" onOpenChange={() => {}} onConfirm={() => {}} bothWaysFolder />);
    const dialog = screen.getByRole("alertdialog");
    expect(dialog).toHaveTextContent("does not come back");
    expect(dialog).toHaveTextContent("wardyn folder bring-back <run> first.");
    expect(dialog).not.toHaveTextContent("`");
    rerender(<KillRunDialog runId={ID} title="t" onOpenChange={() => {}} onConfirm={() => {}} />);
    expect(screen.getByRole("alertdialog")).not.toHaveTextContent("does not come back");
    expect(screen.getByRole("alertdialog")).not.toHaveTextContent("wardyn folder bring-back");
    rerender(<KillRunDialog runId={ID} title="t" onOpenChange={() => {}} onConfirm={() => {}} bothWaysFolder notStarted />);
    expect(screen.getByRole("alertdialog")).not.toHaveTextContent("does not come back");
    expect(screen.getByRole("alertdialog")).not.toHaveTextContent("wardyn folder bring-back");
  });

  it("names a run by the first line of a long task, bounded", () => {
    const task = `${"a".repeat(120)}\nsecond line`;
    render(<KillRunDialog runId={ID} title={task} onOpenChange={() => {}} onConfirm={() => {}} />);
    const dialog = screen.getByRole("alertdialog", { name: `End ${"a".repeat(79)}…?` });
    expect(dialog).not.toHaveTextContent("second line");
  });
});
