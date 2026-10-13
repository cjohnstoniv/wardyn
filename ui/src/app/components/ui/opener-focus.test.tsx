/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { AlertDialog, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogTitle } from "./alert-dialog";
import { Dialog, DialogContent, DialogDescription, DialogTitle } from "./dialog";
import { holdExitAnimation } from "../../../test/exit-animation";

afterEach(cleanup);

// A dialog opened from state has no DialogTrigger, so Radix had nowhere to return focus: it fell to <body>.
function Harness({ kind, onCloseAutoFocus, removeOpener }: { kind: "dialog" | "alert"; onCloseAutoFocus?: (e: Event) => void; removeOpener?: boolean }) {
  const [open, setOpen] = React.useState(false);
  const [gone, setGone] = React.useState(false);
  const body = (
    <>
      <button type="button" onClick={() => setGone(true)}>Remove opener</button>
    </>
  );
  return (
    <>
      <button type="button">Elsewhere</button>
      {!gone && <button type="button" onClick={() => setOpen(true)}>Open</button>}
      {kind === "dialog" ? (
        <Dialog open={open} onOpenChange={setOpen}>
          <DialogContent onCloseAutoFocus={onCloseAutoFocus}>
            <DialogTitle>Title</DialogTitle>
            <DialogDescription>Body</DialogDescription>
            {removeOpener && body}
          </DialogContent>
        </Dialog>
      ) : (
        <AlertDialog open={open} onOpenChange={setOpen}>
          <AlertDialogContent onCloseAutoFocus={onCloseAutoFocus}>
            <AlertDialogTitle>Title</AlertDialogTitle>
            <AlertDialogDescription>Body</AlertDialogDescription>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            {removeOpener && body}
          </AlertDialogContent>
        </AlertDialog>
      )}
    </>
  );
}

describe.each(["dialog", "alert"] as const)("a state-opened %s", (kind) => {
  it("returns focus to the element that opened it on Escape", async () => {
    const user = userEvent.setup();
    render(<Harness kind={kind} />);
    await user.click(screen.getByRole("button", { name: "Open" }));
    expect(await screen.findByText("Title")).toBeTruthy();
    await user.keyboard("{Escape}");
    await waitFor(() => expect(screen.getByRole("button", { name: "Open" })).toHaveFocus());
  });

  it("returns focus again on a second open and close", async () => {
    const user = userEvent.setup();
    render(<Harness kind={kind} />);
    for (let i = 0; i < 2; i++) {
      await user.click(screen.getByRole("button", { name: "Open" }));
      await screen.findByText("Title");
      await user.keyboard("{Escape}");
      await waitFor(() => expect(screen.getByRole("button", { name: "Open" })).toHaveFocus());
    }
  });

  it("does not force focus when the opener is gone, and does not throw", async () => {
    const user = userEvent.setup();
    render(<Harness kind={kind} removeOpener />);
    await user.click(screen.getByRole("button", { name: "Open" }));
    await user.click(await screen.findByRole("button", { name: "Remove opener" }));
    await user.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByText("Title")).toBeNull());
    expect(screen.queryByRole("button", { name: "Open" })).toBeNull();
    expect(document.activeElement).toBe(document.body);
  });

  it("leaves focus alone when the consumer's own handler prevents the default", async () => {
    const user = userEvent.setup();
    render(<Harness kind={kind} onCloseAutoFocus={(e) => e.preventDefault()} />);
    await user.click(screen.getByRole("button", { name: "Open" }));
    await screen.findByText("Title");
    await user.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByText("Title")).toBeNull());
    await new Promise((r) => setTimeout(r, 30));
    expect(screen.getByRole("button", { name: "Open" })).not.toHaveFocus();
  });
});

// Radix runs the close handler only after the exit animation. A caller that moved focus on close (a heading, the
// next field) did it before then, and the handler must not pull focus back to the opener.
describe.each(["dialog", "alert"] as const)("a state-opened %s with an exit animation", (kind) => {
  it("keeps focus where the caller put it while the dialog was leaving", async () => {
    const hold = holdExitAnimation();
    try {
      const user = userEvent.setup();
      render(<Harness kind={kind} />);
      await user.click(screen.getByRole("button", { name: "Open" }));
      await screen.findByText("Title");
      await user.keyboard("{Escape}");
      // Still hidden from the accessibility tree while the dialog is leaving.
      screen.getByRole("button", { name: "Elsewhere", hidden: true }).focus();
      expect(screen.getByText("Title")).toBeTruthy();
      await hold.finish();
      await waitFor(() => expect(screen.queryByText("Title")).toBeNull());
      expect(screen.getByRole("button", { name: "Elsewhere" })).toHaveFocus();
    } finally {
      hold.release();
    }
  });

  it("still returns focus to the opener when nobody moved it", async () => {
    const hold = holdExitAnimation();
    try {
      const user = userEvent.setup();
      render(<Harness kind={kind} />);
      await user.click(screen.getByRole("button", { name: "Open" }));
      await screen.findByText("Title");
      await user.keyboard("{Escape}");
      await hold.finish();
      await waitFor(() => expect(screen.getByRole("button", { name: "Open" })).toHaveFocus());
    } finally {
      hold.release();
    }
  });
});
