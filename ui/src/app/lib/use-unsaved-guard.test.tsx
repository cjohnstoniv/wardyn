/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, NavLink, Route, Routes, useNavigate } from "react-router-dom";
import { UnsavedGuardProvider, useGuardedNavClick, useUnsavedGuard } from "./use-unsaved-guard";
import { UNSAVED } from "./unsaved-copy";

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
