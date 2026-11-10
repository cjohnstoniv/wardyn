/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A route change must be heard: the tab title names the page, and focus moves to the new page's heading
// (unless the screen already put it somewhere inside the page). A search-param change is not a new page.
import * as React from "react";
import { describe, it, expect, vi, afterEach } from "vitest";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import { Link, MemoryRouter, Route, Routes, useSearchParams } from "react-router-dom";

// Its 15 s poll is a legitimate app timer that restarts with each route; leaving it out lets the shell's own
// timer count be asserted as exactly zero.
vi.mock("../wardyn/needs-you-notices", () => ({ NeedsYouNotices: () => null }));

import { AppShell } from "./app-shell";
import { ThemeProvider } from "../wardyn/theme-provider";
import { CONSOLE_VIEW } from "../wardyn/copy/console-view";

function Screen({ name, ownFocus }: { name: string; ownFocus?: boolean }) {
  const [, setParams] = useSearchParams();
  const [version, setVersion] = React.useState(0);
  return (
    <div>
      <h1 key={version}>{version ? `${name} v${version}` : name}</h1>
      <button type="button" onClick={() => setVersion((v) => v + 1)}>
        remount heading
      </button>
      <Link to="/b">go to b</Link>
      <Link to="/c">go to c</Link>
      <button type="button" onClick={() => setParams({ q: "x" })}>
        filter
      </button>
      {ownFocus && <input aria-label="own field" ref={(el) => el?.focus()} />}
      <Link to="/b" replace>redirect to b</Link>
    </div>
  );
}

const fromShell = (stack: string | undefined) => /screens\/app-shell\.tsx/.test(stack ?? "");

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

  // A sign-out switches to the sign-in gate without a reload, so the shell unmounting must reset the title.
  it("hands the tab its plain title back when the shell unmounts", async () => {
    const view = renderShell();
    await screen.findByRole("heading", { name: "Screen A" });
    await waitFor(() => expect(document.title).toMatch(/^Screen A - /));
    view.unmount();
    expect(document.title).toBe(CONSOLE_VIEW.TITLE_USER);
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

  it("does not take focus for a replace (a redirect, not a page the person chose), but still titles it", async () => {
    renderShell();
    await screen.findByRole("heading", { name: "Screen A" });
    act(() => screen.getByRole("link", { name: "redirect to b" }).click());
    const b = await screen.findByRole("heading", { name: "Screen B" });
    await waitFor(() => expect(document.title).toMatch(/^Screen B - /));
    expect(b).not.toHaveFocus();
  });

  // The heading can be remounted on the same route (the cockpit's focus mode hides and redraws its header).
  it("follows a remounted heading on the same route", async () => {
    renderShell();
    await screen.findByRole("heading", { name: "Screen A" });
    await waitFor(() => expect(document.title).toMatch(/^Screen A - /));
    act(() => screen.getByRole("button", { name: "remount heading" }).click());
    await screen.findByRole("heading", { name: "Screen A v1" });
    await waitFor(() => expect(document.title).toMatch(/^Screen A v1 - /));
    act(() => screen.getByRole("button", { name: "remount heading" }).click());
    await waitFor(() => expect(document.title).toMatch(/^Screen A v2 - /));
  });

  // The cold-load probe asserts no timer is pending once the console is entered. The title and focus work uses
  // observers only. jsdom queues its own setImmediate on every element.focus() (its selection task), which is
  // not the shell's, so only setTimeout and setInterval are faked here.
  it("schedules no timer from the shell on a navigation", async () => {
    renderShell();
    await screen.findByRole("heading", { name: "Screen A" });
    vi.useFakeTimers({ toFake: ["setTimeout", "setInterval"] });
    const scheduled: string[] = [];
    for (const name of ["setTimeout", "setInterval"] as const) {
      const real = globalThis[name];
      vi.spyOn(globalThis, name).mockImplementation(((...args: Parameters<typeof real>) => {
        if (fromShell(new Error().stack)) scheduled.push(name);
        return (real as (...a: unknown[]) => unknown)(...args);
      }) as never);
    }
    const settle = () =>
      act(async () => {
        await vi.advanceTimersByTimeAsync(100);
      });
    try {
      act(() => screen.getByRole("link", { name: "go to b" }).click());
      await settle();
      expect(screen.getByRole("heading", { name: "Screen B" })).toHaveFocus();
      expect(vi.getTimerCount()).toBe(0);
      act(() => screen.getByRole("link", { name: "redirect to b" }).click());
      await settle();
      expect(vi.getTimerCount()).toBe(0);
      expect(scheduled).toEqual([]);
    } finally {
      vi.restoreAllMocks();
      vi.useRealTimers();
    }
  });

  // Positive controls, so the two assertions above cannot pass vacuously.
  it("the timer probes do see a timer and a shell stack", () => {
    vi.useFakeTimers({ toFake: ["setTimeout", "setInterval"] });
    try {
      setTimeout(() => {}, 1000);
      expect(vi.getTimerCount()).toBe(1);
    } finally {
      vi.useRealTimers();
    }
    expect(fromShell("Error\n    at f (/repo/ui/src/app/components/screens/app-shell.tsx:530:12)")).toBe(true);
    expect(fromShell("Error\n    at f (/repo/ui/src/app/components/screens/top-bar.tsx:5:1)")).toBe(false);
  });
});
