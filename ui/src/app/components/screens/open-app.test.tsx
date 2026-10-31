/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

const healthMock = vi.fn();
vi.mock("../../lib/api/health", () => ({ health: { health: (...a: unknown[]) => healthMock(...a) } }));
const appTicketMock = vi.fn();
vi.mock("../../lib/api/runs", () => ({ runs: { appTicket: (...a: unknown[]) => appTicketMock(...a) } }));
const enterMock = vi.fn();
vi.mock("./run-detail-ssh", () => ({ enterUIApp: (...a: unknown[]) => enterMock(...a) }));

import { HttpError } from "../../lib/api/core";
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
    expect(appTicketMock).toHaveBeenCalledWith("7c41e0a9", "api");
    expect(enterMock).toHaveBeenCalledWith("7c41e0a9", "api", SANDBOX, { ticket: "tkt", target: "_self" });
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
