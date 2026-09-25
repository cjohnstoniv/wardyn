/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// D5 watch-only, the grid's half: an SSO admin's "Run it" lands them in the
// User view, where a demo their ceiling rewrites offers no Start — so the card
// says "See it" for those, and keeps "Run it" for the rest.
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

describe("FirstRunDemoGrid — watch-only demos under a ceiling", () => {
  it("session-admin: a ceiling-narrowed demo reads See it, an untouched one Run it", () => {
    renderGrid("session-admin");
    const held = within(screen.getByTestId("runs-empty-demo-held-at-the-door"));
    expect(held.queryByRole("link", { name: "Run it" })).toBeNull();
    expect(held.getByRole("link", { name: "See it" })).toHaveAttribute("href", "/setup?step=held-at-the-door");
    const sealed = within(screen.getByTestId("runs-empty-demo-sealed-box"));
    expect(sealed.getByRole("link", { name: "Run it" })).toHaveAttribute("href", "/setup?step=sealed-box");
  });

  it("url (D1): the same demo keeps Run it", () => {
    renderGrid("url");
    const held = within(screen.getByTestId("runs-empty-demo-held-at-the-door"));
    expect(held.getByRole("link", { name: "Run it" })).toHaveAttribute("href", "/setup?step=held-at-the-door");
  });
});
