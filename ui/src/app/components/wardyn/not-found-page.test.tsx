/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { NotFoundPage } from "./not-found-page";

afterEach(cleanup);

function Where() {
  return <div data-testid="where">{useLocation().pathname}</div>;
}

function renderAt(path: string, admin: boolean) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <Routes>
        <Route path="*" element={<><NotFoundPage admin={admin} /><Where /></>} />
      </Routes>
    </MemoryRouter>,
  );
}

// #2884: an address the console does not know says so and stays where it was typed.
describe("NotFoundPage", () => {
  it("names the address in the body, keeps the URL, and offers one way back", () => {
    renderAt("/definitely/not/a/page", false);
    expect(screen.getByRole("region", { name: "Page not found" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Page not found" })).toBeInTheDocument();
    expect(screen.getByText("Not found")).toBeInTheDocument();
    expect(screen.getByText("There's no page at", { exact: false })).toHaveTextContent(
      "There's no page at /definitely/not/a/page.",
    );
    expect(screen.getByText("None found in Wardyn")).toBeInTheDocument();
    expect(screen.getByText("Check the link, or go back to your runs.")).toBeInTheDocument();
    expect(screen.getByTestId("where")).toHaveTextContent("/definitely/not/a/page");
    expect(screen.getAllByRole("button")).toHaveLength(1);
  });

  it("renders the path as text, never as markup", () => {
    renderAt("/%3Cb%3Ex%3C%2Fb%3E", false);
    expect(document.querySelector("b")).toBeNull();
  });

  it("Back to Runs goes to /runs in the User view and /admin/runs in the Admin view", async () => {
    const user = userEvent.setup();
    renderAt("/nope", false);
    await user.click(screen.getByRole("button", { name: "Back to Runs" }));
    expect(screen.getByTestId("where")).toHaveTextContent("/runs");
    cleanup();
    renderAt("/admin/nope", true);
    await user.click(screen.getByRole("button", { name: "Back to Runs" }));
    expect(screen.getByTestId("where")).toHaveTextContent("/admin/runs");
  });
});
