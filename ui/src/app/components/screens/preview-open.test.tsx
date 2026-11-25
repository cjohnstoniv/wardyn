/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Link, MemoryRouter, Route, Routes } from "react-router-dom";

const healthMock = vi.fn();
vi.mock("../../lib/api/health", () => ({ health: { health: (...a: unknown[]) => healthMock(...a) } }));
const ticketMock = vi.fn();
vi.mock("../../lib/api/previews", () => ({ previews: { ticket: (...a: unknown[]) => ticketMock(...a) } }));
const enterMock = vi.fn();
vi.mock("./run-detail-ssh", () => ({ enterUIApp: (...a: unknown[]) => enterMock(...a) }));

import { HttpError, setExpectedPrincipal } from "../../lib/api/core";
import { PREVIEW_REVOKED, PREVIEW_ROUTE, RUN_PREVIEWS } from "../wardyn/copy/run-previews";
import { PreviewOpenScreen } from "./preview-open";

const SANDBOX = { enabled: true, enter_post_url: "https://run-{run}.apps.example.com/__wardyn/enter", host_mode: true };
const refusal = (status: number, reason: string, message = "server words") => Object.assign(new HttpError(status, message), { reason });

function open() {
  return render(
    <MemoryRouter initialEntries={["/previews/7c41e0a9/8080"]}>
      <Routes>
        <Route path="/previews/:run/:port" element={<PreviewOpenScreen />} />
      </Routes>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  healthMock.mockReset().mockResolvedValue({ status: "ok", ui_sandbox: SANDBOX });
  ticketMock.mockReset().mockResolvedValue("tkt");
  enterMock.mockReset().mockResolvedValue(undefined);
});
afterEach(cleanup);

describe("/previews/{run}/{port}", () => {
  it("says Opening, mints the preview's ticket and enters its app in this tab", async () => {
    open();
    expect(screen.getByRole("status")).toHaveTextContent(PREVIEW_ROUTE.OPENING);
    await waitFor(() => expect(enterMock).toHaveBeenCalledTimes(1));
    expect(ticketMock).toHaveBeenCalledWith("7c41e0a9", 8080, "");
    expect(enterMock).toHaveBeenCalledWith("7c41e0a9", "__preview-8080", SANDBOX, expect.objectContaining({ ticket: "tkt", target: "_self" }));
    expect(enterMock.mock.calls[0][3].stale()).toBe(false);
  });

  it("the ticket asked for after the health read still names the person who opened the page", async () => {
    let healthy!: (v: unknown) => void;
    healthMock.mockImplementationOnce(() => new Promise((done) => (healthy = done)));
    setExpectedPrincipal("a@example.com");
    open();
    await waitFor(() => expect(healthMock).toHaveBeenCalledTimes(1));
    setExpectedPrincipal("b@example.com");
    healthy({ status: "ok", ui_sandbox: SANDBOX });
    await waitFor(() => expect(ticketMock).toHaveBeenCalledWith("7c41e0a9", 8080, "a@example.com"));
    setExpectedPrincipal("");
  });

  it("shows the revoked sentence in the refusal box for a 404 run_not_found, and enters nothing", async () => {
    ticketMock.mockRejectedValue(refusal(404, "run_not_found", "run not found"));
    open();
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(RUN_PREVIEWS.OPEN_FAILED(8080));
    expect(alert).toHaveTextContent(PREVIEW_REVOKED);
    expect(alert).not.toHaveTextContent("run not found");
    expect(enterMock).not.toHaveBeenCalled();
  });

  it.each([
    [404, "something_else", "a different not found"],
    [403, "run_not_found", "a forbidden with that reason"],
    [403, "preview_urls_off", "Preview URLs are off on this deployment. An administrator turns them on with features.preview_urls.enabled in site config."],
    [409, "run_background_only", "This run is a background task, so it has no web application gateway. Its logs, audit trail and status stay available."],
  ])("shows the server's own sentence for a %i with reason %s", async (status, reason, message) => {
    ticketMock.mockRejectedValue(refusal(status, reason, message));
    open();
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(message);
    expect(alert).not.toHaveTextContent(PREVIEW_REVOKED);
  });

  it("a health read that does not say ok asks for no ticket", async () => {
    healthMock.mockResolvedValue({});
    open();
    expect(await screen.findByRole("alert")).toHaveTextContent("Wardyn isn't answering. Try again.");
    expect(ticketMock).not.toHaveBeenCalled();
  });

  it("an entry that fails shows its own words", async () => {
    enterMock.mockRejectedValue(new Error("the gateway published no enter URL"));
    open();
    expect(await screen.findByRole("alert")).toHaveTextContent("the gateway published no enter URL");
  });
});

describe("/previews/{run}/{port}: only the attempt for the page on screen acts", () => {
  const held = () => {
    let answer!: (t: string) => void;
    ticketMock.mockImplementationOnce(() => new Promise<string>((res) => (answer = res)));
    return { answer: (t: string) => answer(t) };
  };

  it("under StrictMode mints one ticket and enters once, as the live attempt", async () => {
    render(
      <React.StrictMode>
        <MemoryRouter initialEntries={["/previews/7c41e0a9/8080"]}>
          <Routes>
            <Route path="/previews/:run/:port" element={<PreviewOpenScreen />} />
          </Routes>
        </MemoryRouter>
      </React.StrictMode>,
    );
    await waitFor(() => expect(enterMock).toHaveBeenCalledTimes(1));
    expect(ticketMock).toHaveBeenCalledTimes(1);
  });

  it("an attempt whose page was left never enters", async () => {
    const a = held();
    render(
      <MemoryRouter initialEntries={["/previews/run-a/3000"]}>
        <Link to="/runs">to runs</Link>
        <Routes>
          <Route path="/previews/:run/:port" element={<PreviewOpenScreen />} />
          <Route path="/runs" element={<p>runs list</p>} />
        </Routes>
      </MemoryRouter>,
    );
    await waitFor(() => expect(ticketMock).toHaveBeenCalledTimes(1));
    await userEvent.click(screen.getByRole("link", { name: "to runs" }));
    expect(screen.getByText("runs list")).toBeInTheDocument();
    await act(async () => a.answer("tkt-a"));
    expect(enterMock).not.toHaveBeenCalled();
  });
});
