/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A route change must be heard: the tab title names the page, and focus moves to the new page's heading
// (unless the screen already put it somewhere inside the page). A search-param change is not a new page.
import { describe, it, expect, vi, afterEach } from "vitest";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import { Link, MemoryRouter, Route, Routes, useSearchParams } from "react-router-dom";

import { AppShell } from "./app-shell";
import { ThemeProvider } from "../wardyn/theme-provider";
import { CONSOLE_VIEW } from "../wardyn/copy/console-view";

function Screen({ name, ownFocus }: { name: string; ownFocus?: boolean }) {
  const [, setParams] = useSearchParams();
  return (
    <div>
      <h1>{name}</h1>
      <Link to="/b">go to b</Link>
      <Link to="/c">go to c</Link>
      <button type="button" onClick={() => setParams({ q: "x" })}>
        filter
      </button>
      {ownFocus && <input aria-label="own field" ref={(el) => el?.focus()} />}
    </div>
  );
}

function renderShell() {
  vi.stubGlobal(
    "fetch",
    vi.fn((url: RequestInfo | URL) =>
      String(url).endsWith("/api/v1/me")
        ? Promise.resolve({ ok: true, json: async () => ({ principal: "root@wardyn.local", method: "token", role: "admin" }) })
        : Promise.resolve({ ok: true, json: async () => ({}) }),
    ) as unknown as typeof fetch,
  );
  return render(
    <MemoryRouter initialEntries={["/a"]}>
      <ThemeProvider>
        <Routes>
          <Route element={<AppShell pendingApprovals={0} attentionCount={0} onSignOut={() => {}} />}>
            <Route path="/a" element={<Screen name="Screen A" />} />
            <Route path="/b" element={<Screen name="Screen B" />} />
            <Route path="/c" element={<Screen name="Screen C" ownFocus />} />
          </Route>
        </Routes>
      </ThemeProvider>
    </MemoryRouter>,
  );
}

describe("AppShell route announcement", () => {
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it("puts the page heading in the tab title, and changes it with the route", async () => {
    renderShell();
    await screen.findByRole("heading", { name: "Screen A" });
    await waitFor(() => expect(document.title).toMatch(/^Screen A - /));
    expect([CONSOLE_VIEW.TITLE_ADMIN, CONSOLE_VIEW.TITLE_USER].some((t) => document.title.endsWith(t))).toBe(true);

    act(() => screen.getByRole("link", { name: "go to b" }).click());
    await screen.findByRole("heading", { name: "Screen B" });
    await waitFor(() => expect(document.title).toMatch(/^Screen B - /));
  });

  it("moves focus to the new page's heading after a navigation, not on first load", async () => {
    renderShell();
    const a = await screen.findByRole("heading", { name: "Screen A" });
    expect(a).not.toHaveFocus();

    act(() => screen.getByRole("link", { name: "go to b" }).click());
    const b = await screen.findByRole("heading", { name: "Screen B" });
    await waitFor(() => expect(b).toHaveFocus());
  });

  it("leaves focus alone on a search-param change", async () => {
    renderShell();
    await screen.findByRole("heading", { name: "Screen A" });
    const filter = screen.getByRole("button", { name: "filter" });
    filter.focus();
    act(() => filter.click());
    expect(filter).toHaveFocus();
  });

  it("keeps focus a screen placed inside the page itself", async () => {
    renderShell();
    await screen.findByRole("heading", { name: "Screen A" });
    act(() => screen.getByRole("link", { name: "go to c" }).click());
    await screen.findByRole("heading", { name: "Screen C" });
    await waitFor(() => expect(document.title).toMatch(/^Screen C - /));
    expect(screen.getByLabelText("own field")).toHaveFocus();
  });
});
