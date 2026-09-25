/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// D5, owner ruling 2026-09-25: an SSO admin's "Run it" lands them in the User
// view, where a demo their ceiling narrows is hidden entirely
// (member-getting-started.tsx) — so this grid drops the same card rather
// than link to one that wouldn't be there.
import { describe, it, expect } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import FirstRunDemoGrid from "./runs-first-run-demos";
import { ViewAccessProvider, type ViewAccess } from "../wardyn/console-view";

function renderGrid(access: ViewAccess) {
  return render(
    <MemoryRouter>
      <ViewAccessProvider value={access}>
        <FirstRunDemoGrid llmReady secretNames={[]} />
      </ViewAccessProvider>
    </MemoryRouter>,
  );
}

describe("FirstRunDemoGrid — ceiling-narrowed demos under a view", () => {
  it("session-admin: a ceiling-narrowed demo's card is absent, an untouched one still renders Run it", () => {
    renderGrid("session-admin");
    expect(screen.queryByTestId("runs-empty-demo-held-at-the-door")).not.toBeInTheDocument();
    const sealed = within(screen.getByTestId("runs-empty-demo-sealed-box"));
    expect(sealed.getByRole("link", { name: "Run it" })).toHaveAttribute("href", "/setup?step=sealed-box");
  });

  it("url (D1): the same demo's card still renders, with Run it", () => {
    renderGrid("url");
    const held = within(screen.getByTestId("runs-empty-demo-held-at-the-door"));
    expect(held.getByRole("link", { name: "Run it" })).toHaveAttribute("href", "/setup?step=held-at-the-door");
  });
});
