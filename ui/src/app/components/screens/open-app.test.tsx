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
const appTicketMock = vi.fn();
vi.mock("../../lib/api/runs", () => ({ runs: { appTicket: (...a: unknown[]) => appTicketMock(...a) } }));
const enterMock = vi.fn();
vi.mock("./run-detail-ssh", () => ({ enterUIApp: (...a: unknown[]) => enterMock(...a) }));

import { HttpError, setExpectedPrincipal } from "../../lib/api/core";
import { OPEN_APP } from "../wardyn/copy/ui-apps";
import { OpenAppScreen } from "./open-app";

const SANDBOX = { enabled: true, enter_post_url: "https://run-{run}.apps.example.com/__wardyn/enter", host_mode: true };
const refusal = (status: number, reason: string) => Object.assign(new HttpError(status, "server words"), { reason });

function open() {
  return render(
    <MemoryRouter initialEntries={["/open/7c41e0a9/api"]}>
      <Routes>
        <Route path="/open/:run/:app" element={<OpenAppScreen />} />
      </Routes>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  healthMock.mockReset().mockResolvedValue({ status: "ok", ui_sandbox: SANDBOX });
  appTicketMock.mockReset().mockResolvedValue("tkt");
  enterMock.mockReset().mockResolvedValue(undefined);
});
afterEach(cleanup);

describe("/open/{run}/{app}", () => {
  it("says Opening, mints the app's ticket and enters in this tab", async () => {
    open();
    expect(screen.getByRole("status")).toHaveTextContent("Opening api…");
    await waitFor(() => expect(enterMock).toHaveBeenCalledTimes(1));
    expect(appTicketMock).toHaveBeenCalledWith("7c41e0a9", "api", "");
    expect(enterMock).toHaveBeenCalledWith("7c41e0a9", "api", SANDBOX, expect.objectContaining({ ticket: "tkt", target: "_self" }));
    expect(enterMock.mock.calls[0][3].stale()).toBe(false);
  });

  it("the ticket asked for after the health read still names the person who opened the page", async () => {
    // The server then refuses it rather than mint a ticket for whoever signed in meanwhile.
    let healthy!: (v: unknown) => void;
    healthMock.mockImplementationOnce(() => new Promise((done) => (healthy = done)));
    setExpectedPrincipal("a@example.com");
    open();
    await waitFor(() => expect(healthMock).toHaveBeenCalledTimes(1));

    setExpectedPrincipal("b@example.com");
    healthy({ status: "ok", ui_sandbox: SANDBOX });
    await waitFor(() => expect(appTicketMock).toHaveBeenCalledWith("7c41e0a9", "api", "a@example.com"));
    setExpectedPrincipal("");
  });

  it.each([
    ["a person who is not a caller, or a run that is gone", refusal(404, "run_not_found"), OPEN_APP.NOT_SHARED],
    ["a run that is not running", refusal(409, "attach_not_running"), OPEN_APP.NOT_RUNNING],
    ["a run that has ended", refusal(409, "attach_run_kept"), OPEN_APP.NOT_RUNNING],
    ["a named caller on a shared host", refusal(403, "inbound_needs_host_mode"), OPEN_APP.SHARED_HOST],
  ])("shows the sentence for %s", async (_name, error, text) => {
    appTicketMock.mockRejectedValue(error);
    open();
    expect(await screen.findByRole("alert")).toHaveTextContent(text);
    expect(enterMock).not.toHaveBeenCalled();
  });

  it.each([
    ["no answer", {}, "Wardyn isn't answering. Try again."],
    ["a 503", { failedStatus: 503 }, "Wardyn couldn't open this app. Try again."],
    ["a 403", { failedStatus: 403 }, "Wardyn couldn't open this app."],
    ["a 2xx body that does not parse", { failedStatus: 200 }, "Wardyn's answer couldn't be read."],
  ])("a health read with %s says so without asking for a ticket", async (_name, h, text) => {
    healthMock.mockResolvedValue(h);
    open();
    expect(await screen.findByRole("alert")).toHaveTextContent(text);
    expect(appTicketMock).not.toHaveBeenCalled();
  });

  it("says the gateway is off without asking for a ticket", async () => {
    healthMock.mockResolvedValue({ status: "ok" });
    open();
    expect(await screen.findByRole("alert")).toHaveTextContent(OPEN_APP.GATEWAY_OFF);
    expect(appTicketMock).not.toHaveBeenCalled();
  });

  it("shows another failure in the server's words", async () => {
    appTicketMock.mockRejectedValue(refusal(500, ""));
    open();
    expect(await screen.findByRole("alert")).toHaveTextContent("server words");
  });
});

describe("/open/{run}/{app}: only the attempt for the page on screen acts", () => {
  // A ticket that answers when the test says so.
  const held = () => {
    let answer!: (t: string) => void;
    let refuse!: (e: unknown) => void;
    appTicketMock.mockImplementationOnce(() => new Promise<string>((res, rej) => ((answer = res), (refuse = rej))));
    return { answer: (t: string) => answer(t), refuse: (e: unknown) => refuse(e) };
  };
  function Pages() {
    return (
      <MemoryRouter initialEntries={["/open/run-a/api"]}>
        <Link to="/open/run-b/web">to b</Link>
        <Link to="/runs">to runs</Link>
        <Routes>
          <Route path="/open/:run/:app" element={<OpenAppScreen />} />
          <Route path="/runs" element={<p>runs list</p>} />
        </Routes>
      </MemoryRouter>
    );
  }

  it("under StrictMode mints one ticket and enters once, as the live attempt", async () => {
    render(
      <React.StrictMode>
        <MemoryRouter initialEntries={["/open/7c41e0a9/api"]}>
          <Routes>
            <Route path="/open/:run/:app" element={<OpenAppScreen />} />
          </Routes>
        </MemoryRouter>
      </React.StrictMode>,
    );
    await waitFor(() => expect(enterMock).toHaveBeenCalledTimes(1));
    expect(appTicketMock).toHaveBeenCalledTimes(1);
    expect(enterMock.mock.calls[0][3].stale()).toBe(false);
  });

  it("an attempt superseded by another app's link never enters; the new one enters once", async () => {
    const a = held();
    render(<Pages />);
    await waitFor(() => expect(appTicketMock).toHaveBeenCalledWith("run-a", "api", ""));
    await userEvent.click(screen.getByRole("link", { name: "to b" }));
    await waitFor(() => expect(enterMock).toHaveBeenCalledTimes(1));
    expect(enterMock.mock.calls[0].slice(0, 2)).toEqual(["run-b", "web"]);
    await act(async () => a.answer("tkt-a"));
    expect(enterMock).toHaveBeenCalledTimes(1);
  });

  it("an attempt whose page was left never enters", async () => {
    const a = held();
    render(<Pages />);
    await waitFor(() => expect(appTicketMock).toHaveBeenCalledTimes(1));
    await userEvent.click(screen.getByRole("link", { name: "to runs" }));
    expect(screen.getByText("runs list")).toBeInTheDocument();
    await act(async () => a.answer("tkt-a"));
    expect(enterMock).not.toHaveBeenCalled();
  });

  it("a superseded attempt's late refusal never replaces the new page", async () => {
    const a = held();
    held();
    render(<Pages />);
    await waitFor(() => expect(appTicketMock).toHaveBeenCalledTimes(1));
    await userEvent.click(screen.getByRole("link", { name: "to b" }));
    await waitFor(() => expect(appTicketMock).toHaveBeenCalledTimes(2));
    await act(async () => a.refuse(refusal(404, "run_not_found")));
    expect(screen.getByRole("status")).toHaveTextContent("Opening web…");
    expect(screen.queryByRole("alert")).toBeNull();
  });
});
