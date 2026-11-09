/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The shell's scroll containment. <main> is the console's one scroller, so it must be a containing block
// (an absolutely positioned sr-only live region inside a long screen would otherwise land on the viewport,
// make the document taller than the shell and let a wheel over the sidebar slide the whole shell), and it
// must start every route at its top. The shell root clips so nothing can scroll the document from inside.
import { describe, it, expect, vi, afterEach } from "vitest";
import { act, cleanup, render, screen } from "@testing-library/react";
import { Link, MemoryRouter, Route, Routes, useSearchParams } from "react-router-dom";

import { AppShell } from "./app-shell";
import { ThemeProvider } from "../wardyn/theme-provider";

function Screen({ name }: { name: string }) {
  const [, setParams] = useSearchParams();
  return (
    <div>
      <h1>{name}</h1>
      <Link to="/b">go to b</Link>
      <button type="button" onClick={() => setParams({ q: "x" })}>
        filter
      </button>
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
          </Route>
        </Routes>
      </ThemeProvider>
    </MemoryRouter>,
  );
}

describe("AppShell scroll containment", () => {
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it("makes <main> a positioned scroller that does not chain, and clips at the shell root", async () => {
    renderShell();
    const main = await screen.findByRole("main");
    for (const cls of ["relative", "overflow-y-auto", "overscroll-contain"]) {
      expect(main.classList.contains(cls), `main has ${cls}`).toBe(true);
    }
    const root = main.parentElement!.parentElement!;
    expect(root.classList.contains("overflow-clip")).toBe(true);
    expect(root.classList.contains("relative")).toBe(true);
  });

  it("opens the next route at the top of <main>, but not on a search-param change", async () => {
    renderShell();
    const main = await screen.findByRole("main");
    await screen.findByRole("heading", { name: "Screen A" });

    main.scrollTop = 400;
    act(() => screen.getByRole("button", { name: "filter" }).click());
    expect(main.scrollTop).toBe(400);

    act(() => screen.getByRole("link", { name: "go to b" }).click());
    await screen.findByRole("heading", { name: "Screen B" });
    expect(main.scrollTop).toBe(0);
  });
});
