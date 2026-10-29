/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { INBOUND_APP, UI_APPS_LANE } from "../../wardyn/copy/ui-apps";
import { UIAppRow, type InboundFacts } from "./ui-app-row";

afterEach(cleanup);

const RUN = "7c41e0a9-0000-0000-0000-000000000000";
const HOST: InboundFacts = { host_mode: true, inbound_url_template: "https://{run}.apps.example.com/r/{run}/{app}/" };

function row(app: Parameters<typeof UIAppRow>[0]["app"], facts: InboundFacts | null = HOST, primary = true) {
  return render(<UIAppRow runId={RUN} app={app} primary={primary} busy={false} onOpen={vi.fn()} facts={facts} />);
}

describe("UIAppRow", () => {
  it("shows an inbound app with its tag, callers as chips and both Copy buttons", () => {
    row({ name: "api", port: 8080, path: "/", inbound: true, callers: ["group:payments", "service:billing"] });
    const card = screen.getByTestId("ui-app-api");
    expect(within(card).getByText(INBOUND_APP.TAG)).toBeInTheDocument();
    expect(within(card).getByText("localhost:8080/")).toBeInTheDocument();
    expect(within(card).getByText(INBOUND_APP.CALLERS)).toBeInTheDocument();
    expect(within(card).getByText("group:payments")).toBeInTheDocument();
    expect(within(card).getByText("service:billing")).toBeInTheDocument();
    expect(within(card).queryByText(INBOUND_APP.ONLY_YOU)).toBeNull();
    expect(within(card).getByRole("button", { name: INBOUND_APP.COPY_LINK })).toBeInTheDocument();
    expect(within(card).getByRole("button", { name: INBOUND_APP.COPY_ADDRESS })).toBeInTheDocument();
    expect(within(card).getByRole("button", { name: UI_APPS_LANE.cta("api") })).toBeEnabled();
  });

  it("says Only you for an app with no callers, and gives it no Copy buttons or tag when it is not inbound", () => {
    row({ name: "admin", port: 9090, path: "/admin" }, HOST, false);
    expect(screen.getByText(INBOUND_APP.ONLY_YOU)).toBeInTheDocument();
    expect(screen.queryByText(INBOUND_APP.TAG)).toBeNull();
    expect(screen.queryByRole("button", { name: INBOUND_APP.COPY_LINK })).toBeNull();
    expect(screen.queryByText(INBOUND_APP.CALLERS)).toBeNull();
  });

  it("says Only you for an inbound app that names no one besides its owner", () => {
    row({ name: "api", port: 8080, inbound: true });
    expect(screen.getByText(INBOUND_APP.ONLY_YOU)).toBeInTheDocument();
    expect(screen.getByText(INBOUND_APP.TAG)).toBeInTheDocument();
  });

  it("copies the /open link for people and the run's address for API clients", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    row({ name: "api", port: 8080, inbound: true });
    await userEvent.click(screen.getByRole("button", { name: INBOUND_APP.COPY_LINK }));
    expect(writeText).toHaveBeenLastCalledWith(`${window.location.origin}/open/${RUN}/api`);
    await userEvent.click(screen.getByRole("button", { name: INBOUND_APP.COPY_ADDRESS }));
    expect(writeText).toHaveBeenLastCalledWith(`https://${RUN}.apps.example.com/r/${RUN}/api/`);
  });

  it("offers no API address when the server publishes no template", () => {
    row({ name: "api", port: 8080, inbound: true }, { host_mode: true });
    expect(screen.queryByRole("button", { name: INBOUND_APP.COPY_ADDRESS })).toBeNull();
    expect(screen.getByRole("button", { name: INBOUND_APP.COPY_LINK })).toBeInTheDocument();
  });

  it("explains a shared host only for an inbound app, and only when the deployment serves one", () => {
    const { unmount } = row({ name: "api", port: 8080, inbound: true }, { host_mode: false, inbound_url_template: HOST.inbound_url_template });
    expect(screen.getByText(INBOUND_APP.PATH_MODE_NOTE)).toBeInTheDocument();
    unmount();
    row({ name: "api", port: 8080, inbound: true }, HOST);
    expect(screen.queryByText(INBOUND_APP.PATH_MODE_NOTE)).toBeNull();
  });
});
