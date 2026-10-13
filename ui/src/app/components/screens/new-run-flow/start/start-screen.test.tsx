/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { OperatorProvider } from "../../../wardyn/operator-context";
import { NR_START } from "../../../wardyn/copy/new-run/start";
import { NR_SHARED } from "../../../wardyn/copy/new-run/shared";
import { NEW_RUN_SCREENS, NewRunRoute } from "../new-run-route";
import { StartScreen } from "./start-screen";

function Where() {
  const location = useLocation();
  return <p data-testid="path">{location.pathname + location.search}</p>;
}
function show() {
  render(
    <MemoryRouter initialEntries={["/runs/new"]}>
      <OperatorProvider operator={false} principal="alice">
        <Routes>
          <Route path="/runs/new" element={<><NewRunRoute screens={{ ...NEW_RUN_SCREENS, start: StartScreen }} /><Where /></>} />
        </Routes>
      </OperatorProvider>
    </MemoryRouter>,
  );
}
const path = () => screen.getByTestId("path").textContent;

afterEach(cleanup);

describe("Start screen", () => {
  it("shows the three paths, the neutral Fastest chip and the trust line, and focuses the heading", async () => {
    show();
    expect(await screen.findByRole("heading", { level: 1, name: NR_START.TITLE })).toHaveFocus();
    expect(screen.getByText(NR_START.LEDE)).toBeInTheDocument();
    expect(screen.getByText(NR_START.GUIDED_BADGE)).toBeInTheDocument();
    expect(screen.getByText(NR_START.TRUST_LINE)).toBeInTheDocument();
  });

  it("holds the AI composer as Coming soon: disabled, says so in words, and nothing to press", async () => {
    show();
    const card = (await screen.findByText(NR_START.AI_TITLE)).closest("article");
    expect(card).toHaveAttribute("aria-disabled", "true");
    expect(card).toHaveTextContent(NR_SHARED.COMING_SOON);
    expect(card?.querySelector("button")).toBeNull();
  });

  it("opens the Shape step from Start guided setup", async () => {
    show();
    await userEvent.click(await screen.findByRole("button", { name: new RegExp(`^${NR_START.GUIDED_TITLE}.*${NR_START.GUIDED_FOOT}$`) }));
    await waitFor(() => expect(path()).toBe("/runs/new?step=shape"));
  });

  it("opens the run document in the format of the button pressed", async () => {
    show();
    await userEvent.click(await screen.findByRole("button", { name: NR_START.DOC_JSON }));
    await waitFor(() => expect(path()).toBe("/runs/new?step=document&format=json"));
  });
});
