/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { COMPOSER_PATH, composerHref } from "../../../../lib/composer-url";
import { OperatorProvider } from "../../../wardyn/operator-context";
import { ASSISTANT } from "../../../wardyn/copy/assistant";
import { NR_START } from "../../../wardyn/copy/new-run/start";
import { NR_SHARED } from "../../../wardyn/copy/new-run/shared";
import { NEW_RUN_SCREENS, NewRunRoute } from "../new-run-route";
import { StartScreen } from "./start-screen";

// Each door asks once whether the assistant is ready; a test sets the answer.
const { getMyAssistantMock } = vi.hoisted(() => ({ getMyAssistantMock: vi.fn() }));
vi.mock("../../../../lib/api/assistant", () => ({ assistant: { getMyAssistant: (...a: unknown[]) => getMyAssistantMock(...a) } }));

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

beforeEach(() => getMyAssistantMock.mockReset().mockResolvedValue({ state: "off" }));
afterEach(cleanup);

describe("Start screen", () => {
  it("shows the three paths, the neutral Fastest chip and the trust line, and focuses the heading", async () => {
    show();
    expect(await screen.findByRole("heading", { level: 1, name: NR_START.TITLE })).toHaveFocus();
    expect(screen.getByText(NR_START.LEDE)).toBeInTheDocument();
    expect(screen.getByText(NR_START.GUIDED_BADGE)).toBeInTheDocument();
    expect(screen.getByText(NR_START.TRUST_LINE)).toBeInTheDocument();
  });

  // #3045: an unbuilt path is not a card. The entry screen has the two real paths and one muted roadmap line.
  it("offers only the two real paths, and names what is unbuilt in one muted line with no version", async () => {
    show();
    await screen.findByText(NR_START.GUIDED_TITLE);
    expect(screen.queryByText(NR_SHARED.COMING_SOON)).toBeNull();
    // Guided setup is a button, YAML / JSON a section; the hatched Coming soon article is gone.
    expect(document.querySelectorAll("article")).toHaveLength(0);
    expect(screen.getByRole("button", { name: new RegExp(`^${NR_START.GUIDED_TITLE}`) })).toBeInTheDocument();
    expect(screen.getByRole("region", { name: NR_START.DOC_TITLE })).toBeInTheDocument();
    const roadmap = screen.getByText("On the roadmap: scheduled runs and multi-agent runs.");
    expect(roadmap).toHaveClass("text-muted-foreground");
    expect(roadmap.textContent).not.toMatch(/\d/);
  });

  // The AI composer card is a door: a link to the composer for a new run when the assistant is ready, and otherwise
  // visible and shut, with the one reason, and nothing started or previewed.
  it("links the AI composer card to a new run draft when the assistant is ready", async () => {
    getMyAssistantMock.mockResolvedValue({ state: "ready", provider_name: "Org provider", model: "model-name" });
    show();
    const door = await screen.findByRole("link", { name: NR_START.AI_FOOT });
    expect(door).toHaveAttribute("href", composerHref({ kind: "run" }));
    expect(door.getAttribute("href")?.startsWith(COMPOSER_PATH)).toBe(true);
    expect(getMyAssistantMock).toHaveBeenCalledTimes(1);
    expect(getMyAssistantMock).toHaveBeenCalledWith("compose");
    expect(screen.getByText(NR_START.AI_TITLE)).toBeInTheDocument();
    expect(screen.getByText(NR_START.AI_BADGE)).toBeInTheDocument();
  });

  it("keeps the AI composer card visible, shut and without a link while the assistant is off", async () => {
    show();
    expect(await screen.findByText(ASSISTANT.OFF)).toBeInTheDocument();
    const door = screen.getByRole("button", { name: NR_START.AI_FOOT });
    expect(door).toBeDisabled();
    expect(screen.queryByRole("link", { name: NR_START.AI_FOOT })).toBeNull();
    expect(door.closest("a")).toBeNull();
    // Only the availability read was made: no preview, no session.
    expect(getMyAssistantMock).toHaveBeenCalledTimes(1);
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
