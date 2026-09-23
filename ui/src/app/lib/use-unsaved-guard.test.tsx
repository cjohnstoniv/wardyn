/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { act } from "react";
import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { BrowserRouter, MemoryRouter, NavLink, Route, Routes, useNavigate } from "react-router-dom";
import { UnsavedGuardProvider, useGuardedNavClick, useUnsavedGuard } from "./use-unsaved-guard";
import { UNSAVED } from "./unsaved-copy";
import { registerUnsaved } from "./unsaved-registry";

// Mirrors the real call site (app-shell.tsx#SidebarNav): a guarded click sits
// on an actual <NavLink>, since a clean click lets the LINK's own navigation
// through rather than calling `navigate` itself — only the intercepted
// (dirty, confirmed) path calls `navigate` explicitly, after `preventDefault`
// stopped the native one.
function Editor({ dirty }: { dirty: boolean }) {
  useUnsavedGuard("test-editor", dirty, () => "draft text");
  const navigate = useNavigate();
  const guardedClick = useGuardedNavClick(navigate);
  return (
    <NavLink to="/next" onClick={guardedClick("/next")}>
      go
    </NavLink>
  );
}

function renderEditor(dirty: boolean) {
  return render(
    <MemoryRouter initialEntries={["/"]}>
      <UnsavedGuardProvider>
        <Routes>
          <Route path="/" element={<Editor dirty={dirty} />} />
          <Route path="/next" element={<div>next page</div>} />
        </Routes>
      </UnsavedGuardProvider>
    </MemoryRouter>,
  );
}

describe("useUnsavedGuard + useGuardedNavClick", () => {
  it("a clean editor navigates straight through — no dialog", async () => {
    renderEditor(false);
    await userEvent.click(screen.getByRole("link", { name: "go" }));
    expect(screen.getByText("next page")).toBeInTheDocument();
    expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument();
  });

  it("a dirty editor blocks the click and opens the confirm dialog", async () => {
    renderEditor(true);
    await userEvent.click(screen.getByRole("link", { name: "go" }));
    expect(screen.queryByText("next page")).not.toBeInTheDocument();
    const dialog = screen.getByRole("alertdialog");
    expect(dialog).toBeInTheDocument();
    expect(screen.getByText(UNSAVED.TITLE)).toBeInTheDocument();
    expect(screen.getByText(UNSAVED.BODY)).toBeInTheDocument();
  });

  it("Keep editing closes the dialog and never navigates", async () => {
    renderEditor(true);
    await userEvent.click(screen.getByRole("link", { name: "go" }));
    await userEvent.click(screen.getByRole("button", { name: UNSAVED.STAY }));
    expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument();
    expect(screen.queryByText("next page")).not.toBeInTheDocument();
  });

  it("Discard changes proceeds with the navigation that was held", async () => {
    renderEditor(true);
    await userEvent.click(screen.getByRole("link", { name: "go" }));
    await userEvent.click(screen.getByRole("button", { name: UNSAVED.DISCARD }));
    expect(screen.getByText("next page")).toBeInTheDocument();
    expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument();
  });
});

describe("useUnsavedGuard — beforeunload", () => {
  afterEach(() => vi.restoreAllMocks());

  it("is registered only while dirty, and removed the moment it isn't", () => {
    const add = vi.spyOn(window, "addEventListener");
    const remove = vi.spyOn(window, "removeEventListener");

    function Host({ dirty }: { dirty: boolean }) {
      useUnsavedGuard("beforeunload-editor", dirty, () => "draft");
      return null;
    }
    const { rerender, unmount } = render(<Host dirty={false} />);
    expect(add).not.toHaveBeenCalledWith("beforeunload", expect.anything());

    rerender(<Host dirty={true} />);
    expect(add).toHaveBeenCalledWith("beforeunload", expect.any(Function));

    rerender(<Host dirty={false} />);
    expect(remove).toHaveBeenCalledWith("beforeunload", expect.any(Function));

    unmount();
  });
});

// #460 review — BrowserRouter has no blocker for Back/Forward, so
// UnsavedGuardProvider listens to native `popstate` directly. By the time it
// fires the browser has ALREADY moved (window.history.state reads as the NEW
// entry) — the fix restores it immediately (mocked here: `history.go` is
// spied so the test controls exactly what "restore"/"redo" asserts, without
// depending on jsdom's own async back()/go() timing), then asks; Discard
// replays the original move.
function PushButton() {
  const navigate = useNavigate();
  return (
    <button type="button" onClick={() => navigate("/next")}>
      go forward
    </button>
  );
}

async function setUpAtNextEntry(user: ReturnType<typeof userEvent.setup>) {
  render(
    <BrowserRouter>
      <UnsavedGuardProvider>
        <PushButton />
      </UnsavedGuardProvider>
    </BrowserRouter>,
  );
  // A REAL react-router push — exercises the pushState patch that keeps
  // UnsavedGuardProvider's own index tracking current between pops, the same
  // path a real <Link>/navigate() click takes.
  await user.click(screen.getByRole("button", { name: "go forward" }));
}

// Simulates what the BROWSER itself does on a real Back/Forward: it moves
// history.state to the target entry, THEN fires popstate — with no call to
// pushState/replaceState in between (that's a JS-visible API the browser's
// own navigation never goes through). Using window.history.replaceState()
// here instead would run through use-unsaved-guard.tsx's OWN patch — a test
// artifact that updates historyIndexRef before the guard ever sees the
// "old" value, masking the delta it's supposed to compute. The native
// History.prototype method sidesteps that patch, the same way the browser's
// C++ implementation does.
function simulateBrowserPop(idx: number): void {
  History.prototype.replaceState.call(window.history, { idx }, "", window.location.pathname);
  act(() => {
    window.dispatchEvent(new PopStateEvent("popstate"));
  });
}

describe("UnsavedGuardProvider — browser Back/Forward (popstate)", () => {
  beforeEach(() => {
    window.history.replaceState(null, "", "/start");
  });
  afterEach(() => vi.restoreAllMocks());

  it("a dirty registry intercepts Back: restores the entry immediately and opens the confirm", async () => {
    const user = userEvent.setup();
    await setUpAtNextEntry(user);
    const goSpy = vi.spyOn(window.history, "go").mockImplementation(() => {});
    const unregister = registerUnsaved("dirty-back-test", () => "unsaved text");

    // Simulate the browser having already popped back one entry.
    const poppedTo = (window.history.state as { idx: number }).idx - 1;
    simulateBrowserPop(poppedTo);

    expect(goSpy).toHaveBeenCalledWith(1); // restore = -delta = -(-1)
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText(UNSAVED.TITLE)).toBeInTheDocument();

    unregister();
  });

  it("Keep editing stays — no further history.go call", async () => {
    const user = userEvent.setup();
    await setUpAtNextEntry(user);
    const goSpy = vi.spyOn(window.history, "go").mockImplementation(() => {});
    const unregister = registerUnsaved("dirty-back-stay", () => "unsaved text");

    const poppedTo = (window.history.state as { idx: number }).idx - 1;
    simulateBrowserPop(poppedTo);
    await screen.findByRole("alertdialog");

    await user.click(screen.getByRole("button", { name: UNSAVED.STAY }));
    expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument();
    expect(goSpy).toHaveBeenCalledTimes(1); // only the restore — no redo

    unregister();
  });

  it("Discard changes replays the ORIGINAL move", async () => {
    const user = userEvent.setup();
    await setUpAtNextEntry(user);
    const goSpy = vi.spyOn(window.history, "go").mockImplementation(() => {});
    const unregister = registerUnsaved("dirty-back-discard", () => "unsaved text");

    const poppedTo = (window.history.state as { idx: number }).idx - 1;
    simulateBrowserPop(poppedTo);
    await screen.findByRole("alertdialog");

    await user.click(screen.getByRole("button", { name: UNSAVED.DISCARD }));
    expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument();
    expect(goSpy).toHaveBeenNthCalledWith(1, 1); // restore
    expect(goSpy).toHaveBeenNthCalledWith(2, -1); // redo the original Back

    unregister();
  });

  it("a clean registry lets Back through untouched — no restore, no dialog", async () => {
    const user = userEvent.setup();
    await setUpAtNextEntry(user);
    const goSpy = vi.spyOn(window.history, "go").mockImplementation(() => {});

    const poppedTo = (window.history.state as { idx: number }).idx - 1;
    simulateBrowserPop(poppedTo);

    expect(goSpy).not.toHaveBeenCalled();
    expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument();
  });
});
