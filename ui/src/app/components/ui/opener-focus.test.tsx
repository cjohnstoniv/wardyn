/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { AlertDialog, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogTitle } from "./alert-dialog";
import { Dialog, DialogContent, DialogDescription, DialogTitle } from "./dialog";
import { Sheet, SheetContent, SheetDescription, SheetTitle } from "./sheet";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "./dropdown-menu";
import { holdExitAnimation } from "../../../test/exit-animation";

afterEach(cleanup);

// A dialog opened from state has no DialogTrigger, so Radix had nowhere to return focus: it fell to <body>.
function Harness({ kind, onCloseAutoFocus, removeOpener }: { kind: "dialog" | "alert" | "sheet"; onCloseAutoFocus?: (e: Event) => void; removeOpener?: boolean }) {
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
      ) : kind === "sheet" ? (
        <Sheet open={open} onOpenChange={setOpen}>
          <SheetContent onCloseAutoFocus={onCloseAutoFocus}>
            <SheetTitle>Title</SheetTitle>
            <SheetDescription>Body</SheetDescription>
            {removeOpener && body}
          </SheetContent>
        </Sheet>
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

describe.each(["dialog", "alert", "sheet"] as const)("a state-opened %s", (kind) => {
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

// A dialog opened from a dropdown item: the item is the focused element when the dialog mounts, and it
// unmounts with its menu, so focus went to the page body instead of the menu's trigger.
function MenuHarness() {
  const [open, setOpen] = React.useState(false);
  return (
    <>
      <DropdownMenu>
        <DropdownMenuTrigger>Actions</DropdownMenuTrigger>
        <DropdownMenuContent>
          <DropdownMenuItem onSelect={() => setOpen(true)}>Details</DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent>
          <DialogTitle>Title</DialogTitle>
          <DialogDescription>Body</DialogDescription>
        </DialogContent>
      </Dialog>
    </>
  );
}

describe("a dialog opened from a dropdown item", () => {
  it("returns focus to the menu trigger on close", async () => {
    const user = userEvent.setup();
    render(<MenuHarness />);
    await user.click(screen.getByRole("button", { name: "Actions" }));
    await user.click(await screen.findByRole("menuitem", { name: "Details" }));
    await screen.findByText("Title");
    await user.keyboard("{Escape}");
    await waitFor(() => expect(screen.getByRole("button", { name: "Actions" })).toHaveFocus());
  });
});

// A dialog that confirms the removal of the very row that opened it: the opener is gone when the dialog closes,
// so focus would fall to the body and a keyboard user would restart from the top of the page.
function ListHarness({ initial, rowsAs = "tr", refetchMs = 0 }: { initial: string[]; rowsAs?: "tr" | "card"; refetchMs?: number }) {
  const [names, setNames] = React.useState(initial);
  const [target, setTarget] = React.useState<string | null>(null);
  const rows = names.map((n) =>
    rowsAs === "tr" ? (
      <tr key={n}>
        <td>{n}</td>
        <td>
          <button type="button" onClick={() => setTarget(n)}>Delete {n}</button>
        </td>
      </tr>
    ) : (
      <div key={n} data-focus-row>
        <span>{n}</span>
        <button type="button" onClick={() => setTarget(n)}>Delete {n}</button>
      </div>
    ),
  );
  return (
    <main id="main-content" tabIndex={-1}>
      <h1>Things</h1>
      <button type="button">Elsewhere</button>
      {rowsAs === "tr" ? (
        <table>
          <tbody>{rows}</tbody>
        </table>
      ) : (
        <div>{rows}</div>
      )}
      <AlertDialog open={!!target} onOpenChange={(o) => !o && setTarget(null)}>
        <AlertDialogContent>
          <AlertDialogTitle>Delete {target}?</AlertDialogTitle>
          <AlertDialogDescription>Body</AlertDialogDescription>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <button
            type="button"
            onClick={() => {
              const gone = target;
              // A list refetches after the dialog has closed, so the row can outlive it.
              if (refetchMs) setTimeout(() => setNames((all) => all.filter((n) => n !== gone)), refetchMs);
              else setNames((all) => all.filter((n) => n !== gone));
              setTarget(null);
            }}
          >
            Confirm
          </button>
        </AlertDialogContent>
      </AlertDialog>
    </main>
  );
}

describe.each(["tr", "card"] as const)("a dialog confirming the removal of its own %s", (rowsAs) => {
  async function confirmDelete(user: ReturnType<typeof userEvent.setup>, name: string) {
    await user.click(screen.getByRole("button", { name: `Delete ${name}` }));
    await user.click(await screen.findByRole("button", { name: "Confirm" }));
  }

  it("moves focus to the same control in the next row", async () => {
    const user = userEvent.setup();
    render(<ListHarness initial={["a", "b", "c"]} rowsAs={rowsAs} />);
    await confirmDelete(user, "b");
    await waitFor(() => expect(screen.getByRole("button", { name: "Delete c" })).toHaveFocus());
  });

  it("moves focus to the next row when the list refetches after the dialog has closed", async () => {
    const user = userEvent.setup();
    render(<ListHarness initial={["a", "b", "c"]} rowsAs={rowsAs} refetchMs={150} />);
    await confirmDelete(user, "b");
    await waitFor(() => expect(screen.getByRole("button", { name: "Delete c" })).toHaveFocus(), { timeout: 2000 });
  });

  it("moves focus to the previous row when the last row was removed", async () => {
    const user = userEvent.setup();
    render(<ListHarness initial={["a", "b"]} rowsAs={rowsAs} />);
    await confirmDelete(user, "b");
    await waitFor(() => expect(screen.getByRole("button", { name: "Delete a" })).toHaveFocus());
  });

  it("moves focus to the page heading when no row is left", async () => {
    const user = userEvent.setup();
    render(<ListHarness initial={["a"]} rowsAs={rowsAs} />);
    await confirmDelete(user, "a");
    await waitFor(() => expect(screen.getByRole("heading", { name: "Things" })).toHaveFocus());
  });

  it("returns focus to the opener, and does not move it later, when the row stays", async () => {
    const user = userEvent.setup();
    render(<ListHarness initial={["a", "b"]} rowsAs={rowsAs} />);
    await user.click(screen.getByRole("button", { name: "Delete a" }));
    await user.click(await screen.findByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Delete a" })).toHaveFocus());
    await new Promise((r) => setTimeout(r, 250));
    expect(screen.getByRole("button", { name: "Delete a" })).toHaveFocus();
  });

  it("leaves focus alone when the user moved it before the row went", async () => {
    const user = userEvent.setup();
    render(<ListHarness initial={["a", "b"]} rowsAs={rowsAs} />);
    await confirmDelete(user, "a");
    screen.getByRole("button", { name: "Elsewhere" }).focus();
    await new Promise((r) => setTimeout(r, 250));
    expect(screen.getByRole("button", { name: "Elsewhere" })).toHaveFocus();
  });
});

// The focus watch must not leave a timer behind: tests (and the cold-load probe) assert none is pending.
describe("the opener focus watch leaves no timer behind", () => {
  afterEach(() => vi.useRealTimers());

  it("holds none while the page is quiet, and none once a vanished opener has been handled", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const user = userEvent.setup({ advanceTimers: (ms) => vi.advanceTimersByTime(ms) });
    render(<ListHarness initial={["a", "b", "c"]} />);
    await user.click(screen.getByRole("button", { name: "Delete b" }));
    await user.click(await screen.findByRole("button", { name: "Confirm" }));
    await vi.advanceTimersByTimeAsync(500);
    expect(screen.getByRole("button", { name: "Delete c" })).toHaveFocus();
    expect(vi.getTimerCount()).toBe(0);
  });

  it("holds none after a cancel that kept the opener, or after the page is torn down", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const user = userEvent.setup({ advanceTimers: (ms) => vi.advanceTimersByTime(ms) });
    const { unmount } = render(<ListHarness initial={["a", "b"]} />);
    await user.click(screen.getByRole("button", { name: "Delete a" }));
    await user.click(await screen.findByRole("button", { name: "Cancel" }));
    await vi.advanceTimersByTimeAsync(500);
    expect(screen.getByRole("button", { name: "Delete a" })).toHaveFocus();
    expect(vi.getTimerCount()).toBe(0);
    unmount();
    await vi.advanceTimersByTimeAsync(500);
    expect(vi.getTimerCount()).toBe(0);
  });
});

// Connected is not focusable. A remove button is disabled while its request is in flight, so a dialog that closed
// at once (its confirm sent the request) could hand focus to a disabled opener, which takes none, and leave it on
// the body for good, including when the request then fails and the row stays.
function BusyHarness({ failMs = 150 }: { failMs?: number }) {
  const [busy, setBusy] = React.useState(false);
  const [target, setTarget] = React.useState<string | null>(null);
  const names = ["a", "b"];
  return (
    <main id="main-content" tabIndex={-1}>
      <h1>Hosts</h1>
      <button type="button" onClick={() => setBusy(true)}>Start a request</button>
      <ul>
        {names.map((n) => (
          <li key={n}>
            {n} <button type="button" disabled={busy} onClick={() => setTarget(n)}>Remove {n}</button>
          </li>
        ))}
      </ul>
      <AlertDialog open={!!target} onOpenChange={(o) => !o && setTarget(null)}>
        <AlertDialogContent>
          <AlertDialogTitle>Remove {target}?</AlertDialogTitle>
          <AlertDialogDescription>Body</AlertDialogDescription>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <button
            type="button"
            onClick={() => {
              setBusy(true);
              setTarget(null);
              // The request fails: every button comes back, no row is removed.
              setTimeout(() => setBusy(false), failMs);
            }}
          >
            Confirm
          </button>
        </AlertDialogContent>
      </AlertDialog>
    </main>
  );
}

describe("a dialog whose opener is disabled while its request is in flight", () => {
  it("leaves focus on a usable element, never the body, while the request runs and after it fails", async () => {
    const user = userEvent.setup();
    render(<BusyHarness />);
    await user.click(screen.getByRole("button", { name: "Remove a" }));
    await user.click(await screen.findByRole("button", { name: "Confirm" }));
    await waitFor(() => expect(screen.getByRole("heading", { name: "Hosts" })).toHaveFocus());
    expect(screen.getByRole("button", { name: "Remove a" })).toBeDisabled();
    // The failure re-enables the buttons; focus is still on something, not the body.
    await waitFor(() => expect(screen.getByRole("button", { name: "Remove a" })).toBeEnabled(), { timeout: 2000 });
    await new Promise((r) => setTimeout(r, 250));
    expect(document.activeElement).not.toBe(document.body);
    expect(screen.getByRole("heading", { name: "Hosts" })).toHaveFocus();
  });

  // jsdom keeps activeElement on a button that becomes disabled, which a browser does not (focus falls to the
  // body). So the browser's state is set up by hand: the focused opener is blurred, then disabled, inside the
  // watch window that restoreOpenerFocus opened.
  it("recovers focus when the opener is disabled after the dialog closed and focus is on the body", async () => {
    const user = userEvent.setup();
    render(<BusyHarness />);
    await user.click(screen.getByRole("button", { name: "Remove a" }));
    await user.click(await screen.findByRole("button", { name: "Cancel" }));
    const opener = screen.getByRole("button", { name: "Remove a" });
    await waitFor(() => expect(opener).toHaveFocus());
    opener.blur();
    expect(document.activeElement).toBe(document.body);
    // A bare click: unlike user.click it does not move focus, which would end the watch.
    act(() => screen.getByRole("button", { name: "Start a request" }).click());
    expect(opener).toBeDisabled();
    await waitFor(() => expect(screen.getByRole("heading", { name: "Hosts" })).toHaveFocus(), { timeout: 2000 });
  });
});
