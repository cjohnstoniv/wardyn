/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Preview URLs tile: its states, who sees which controls, and how an Open and a refusal read.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { RunDetail } from "../../../lib/types";
import type { RunPreview } from "../../../lib/types/previews";

const listMock = vi.fn();
const putMock = vi.fn();
const revokeMock = vi.fn();
const ticketMock = vi.fn();
vi.mock("../../../lib/api/previews", () => ({
  previews: {
    list: (...a: unknown[]) => listMock(...a),
    put: (...a: unknown[]) => putMock(...a),
    revoke: (...a: unknown[]) => revokeMock(...a),
    ticket: (...a: unknown[]) => ticketMock(...a),
  },
}));
const healthMock = vi.fn();
vi.mock("../../../lib/api/health", () => ({ health: { health: (...a: unknown[]) => healthMock(...a) } }));
const enterMock = vi.fn();
const openTabMock = vi.fn();
vi.mock("../run-detail-ssh", () => ({ enterUIApp: (...a: unknown[]) => enterMock(...a), openEnterTab: () => openTabMock() }));
let setup: { features?: { preview_urls?: boolean; preview_max_visibility?: "private" | "org" | "public" } } | null = null;
vi.mock("../../wardyn/model-access-context", () => ({ useShellSetupStatus: () => ({ status: setup, refresh: vi.fn() }) }));

import { HttpError } from "../../../lib/api/core";
import { RUN_MODE_REFUSAL } from "../../../lib/run-mode-refusals";
import { OperatorProvider } from "../../wardyn/operator-context";
import { RUN_PREVIEWS as T } from "../../wardyn/copy/run-previews";
import { aheadByHours } from "../../../lib/test-clock";
import { PreviewsPanel } from "./previews-panel";

const OWNER = "alice@example.com";
const run: RunDetail = {
  id: "3f2c9a1e-7b44-4c0d-9e21-5a6b8d0c1f73",
  created_at: aheadByHours(-1),
  updated_at: aheadByHours(-1),
  created_by: OWNER,
  agent: "claude-code",
  repo: "acme/widgets",
  task: "do the thing",
  confinement_class: "CC2",
  state: "RUNNING",
  spiffe_id: "spiffe://wardyn.local/agent-run/3f2c9a1e-7b44-4c0d-9e21-5a6b8d0c1f73",
  runner_target: "docker",
  placement: "",
};
const row = (port: number, visibility: RunPreview["visibility"], hours = 23): RunPreview => ({
  id: `p-${port}`,
  run_id: run.id,
  port,
  visibility,
  created_by: OWNER,
  created_at: aheadByHours(-1),
  updated_at: aheadByHours(-1),
  expires_at: aheadByHours(hours),
});
const refusal = (status: number, reason: string, message = "server words") => Object.assign(new HttpError(status, message), { reason });

function show(over: Partial<RunDetail> = {}, who: { principal?: string; security?: boolean; operator?: boolean } = {}) {
  return render(
    <OperatorProvider operator={who.operator ?? false} securityOperator={who.security ?? false} principal={who.principal ?? OWNER}>
      <PreviewsPanel run={{ ...run, ...over }} />
    </OperatorProvider>,
  );
}
const panel = () => screen.findByRole("region", { name: T.TITLE });
const SANDBOX = { enabled: true, enter_post_url: "https://run-{run}.apps.example.com/__wardyn/enter" };

let tab: { close: ReturnType<typeof vi.fn> };
beforeEach(() => {
  setup = { features: { preview_urls: true, preview_max_visibility: "org" } };
  tab = { close: vi.fn() };
  listMock.mockReset().mockResolvedValue([]);
  putMock.mockReset();
  revokeMock.mockReset().mockResolvedValue(undefined);
  ticketMock.mockReset().mockResolvedValue("tkt");
  healthMock.mockReset().mockResolvedValue({ status: "ok", ui_sandbox: SANDBOX });
  enterMock.mockReset().mockResolvedValue(undefined);
  openTabMock.mockReset().mockReturnValue(tab);
});
afterEach(cleanup);

describe("PreviewsPanel: the owner's tile", () => {
  it("with no previews says so, shows the intro, the open form with Private chosen and the foot note", async () => {
    show();
    const p = await panel();
    expect(await within(p).findByText(T.EMPTY)).toBeInTheDocument();
    expect(within(p).getByText(T.INTRO)).toBeInTheDocument();
    expect(within(p).getByText(T.FOOT)).toBeInTheDocument();
    expect(within(p).getByRole("radio", { name: T.PRIVATE })).toBeChecked();
    expect(within(p).getByRole("button", { name: T.CREATE })).toBeDisabled();
    expect(within(p).queryByRole("button", { name: T.NEW })).toBeNull();
    expect(listMock).toHaveBeenCalledWith(run.id);
  });

  it("lists each preview with its visibility, address and a relative expiry, and keeps the form behind New preview", async () => {
    listMock.mockResolvedValue([row(3000, "private"), row(8080, "org", 5)]);
    show();
    const p = await panel();
    const r3000 = await within(p).findByTestId("preview-3000");
    expect(r3000).toHaveTextContent("Port 3000");
    expect(r3000).toHaveTextContent(T.PRIVATE);
    expect(r3000).toHaveTextContent("localhost:3000");
    expect(r3000).toHaveTextContent("Expires in 23h");
    expect(within(p).getByTestId("preview-8080")).toHaveTextContent("Expires in 5h");
    expect(within(p).queryByRole("button", { name: T.CREATE })).toBeNull();
    await userEvent.click(within(p).getByRole("button", { name: T.NEW }));
    expect(within(p).getByRole("button", { name: T.CREATE })).toBeInTheDocument();
  });

  it("offers Copy link on an Organisation row only, and says Link copied after the click", async () => {
    listMock.mockResolvedValue([row(3000, "private"), row(8080, "org")]);
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    show();
    await panel();
    expect(within(await screen.findByTestId("preview-3000")).queryByRole("button", { name: T.COPY_LINK })).toBeNull();
    await userEvent.click(within(screen.getByTestId("preview-8080")).getByRole("button", { name: T.COPY_LINK }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(`${window.location.origin}/previews/${run.id}/8080`));
    expect(await within(screen.getByTestId("preview-8080")).findByRole("button", { name: T.LINK_COPIED })).toBeInTheDocument();
  });

  it("revoke is one click and leaves the row muted, with the Revoked chip and no buttons", async () => {
    listMock.mockResolvedValue([row(4173, "private")]);
    show();
    const r = await screen.findByTestId("preview-4173");
    await userEvent.click(within(r).getByRole("button", { name: T.REVOKE }));
    expect(revokeMock).toHaveBeenCalledWith(run.id, 4173);
    await waitFor(() => expect(within(screen.getByTestId("preview-4173")).getByText(T.REVOKED_CHIP)).toBeInTheDocument());
    expect(screen.getByTestId("preview-4173")).toHaveTextContent(T.REVOKED_NOTE);
    expect(within(screen.getByTestId("preview-4173")).queryByRole("button")).toBeNull();
  });

  it("creates a preview with the chosen port, length and visibility, shows it and closes the form", async () => {
    putMock.mockResolvedValue(row(8080, "org"));
    show();
    const p = await panel();
    await within(p).findByText(T.EMPTY);
    await userEvent.type(within(p).getByLabelText(T.PORT_LABEL), "8080");
    await userEvent.click(within(p).getByRole("radio", { name: T.ORG }));
    await userEvent.click(within(p).getByRole("button", { name: T.CREATE }));
    expect(putMock).toHaveBeenCalledWith(run.id, 8080, { visibility: "org", ttl_seconds: 86400 });
    expect(await within(p).findByTestId("preview-8080")).toBeInTheDocument();
    expect(within(p).queryByRole("button", { name: T.CREATE })).toBeNull();
  });

  it("ignores anything but digits in the port field", async () => {
    show();
    const p = await panel();
    await userEvent.type(await within(p).findByLabelText(T.PORT_LABEL), "80a8x0");
    expect(within(p).getByLabelText(T.PORT_LABEL)).toHaveValue("8080");
  });

  it("shows the server's sentence under the form as it came, and marks the port field for a port refusal", async () => {
    putMock.mockRejectedValue(refusal(400, "preview_port_reserved", "A preview needs a port from 1024 to 65535, other than 3128, which the run's proxy uses."));
    show();
    const p = await panel();
    const port = await within(p).findByLabelText(T.PORT_LABEL);
    await userEvent.type(port, "3128");
    await userEvent.click(within(p).getByRole("button", { name: T.CREATE }));
    expect(await within(p).findByRole("alert")).toHaveTextContent("A preview needs a port from 1024 to 65535, other than 3128, which the run's proxy uses.");
    expect(port).toHaveAttribute("aria-invalid", "true");
  });

  it("does not mark the port field for a refusal that is about something else", async () => {
    putMock.mockRejectedValue(refusal(409, "preview_data_out", "This run's data_out rule keeps content in, so it has no previews."));
    show();
    const p = await panel();
    const port = await within(p).findByLabelText(T.PORT_LABEL);
    await userEvent.type(port, "3000");
    await userEvent.click(within(p).getByRole("button", { name: T.CREATE }));
    expect(await within(p).findByRole("alert")).toHaveTextContent("This run's data_out rule keeps content in, so it has no previews.");
    expect(port).not.toHaveAttribute("aria-invalid", "true");
  });

  it("dims what the ceiling forbids and says why: Public under Organisation", async () => {
    show();
    const p = await panel();
    await within(p).findByText(T.EMPTY);
    expect(within(p).getByRole("radio", { name: T.PUBLIC })).toBeDisabled();
    expect(within(p).getByRole("radio", { name: T.ORG })).toBeEnabled();
    expect(within(p).getAllByText(T.CAPPED("Organisation"))).toHaveLength(1);
  });

  it("dims Organisation and Public under a Private ceiling, and an absent ceiling reads Private", async () => {
    for (const features of [{ preview_urls: true, preview_max_visibility: "private" as const }, { preview_urls: true }]) {
      setup = { features };
      const { unmount } = show();
      const p = await panel();
      await within(p).findByText(T.EMPTY);
      expect(within(p).getByRole("radio", { name: T.ORG })).toBeDisabled();
      expect(within(p).getByRole("radio", { name: T.PUBLIC })).toBeDisabled();
      expect(within(p).getAllByText(T.CAPPED("Private"))).toHaveLength(2);
      unmount();
    }
  });

  it("never offers Public, even under a ceiling of public: it reads as Organisation", async () => {
    setup = { features: { preview_urls: true, preview_max_visibility: "public" } };
    show();
    const p = await panel();
    await within(p).findByText(T.EMPTY);
    expect(within(p).getByRole("radio", { name: T.PUBLIC })).toBeDisabled();
  });
});

describe("PreviewsPanel: Open", () => {
  it("opens the tab first, mints the preview's ticket and enters the preview's app in it", async () => {
    listMock.mockResolvedValue([row(3000, "private")]);
    show();
    await userEvent.click(within(await screen.findByTestId("preview-3000")).getByRole("button", { name: T.OPEN }));
    await waitFor(() => expect(enterMock).toHaveBeenCalledTimes(1));
    expect(openTabMock).toHaveBeenCalledTimes(1);
    expect(ticketMock).toHaveBeenCalledWith(run.id, 3000);
    expect(enterMock).toHaveBeenCalledWith(run.id, "__preview-3000", SANDBOX, { ticket: "tkt", tab });
  });

  it("says the browser blocked the new tab and asks for no ticket when the tab does not open", async () => {
    listMock.mockResolvedValue([row(3000, "private")]);
    openTabMock.mockReturnValue(null);
    show();
    await userEvent.click(within(await screen.findByTestId("preview-3000")).getByRole("button", { name: T.OPEN }));
    expect(await screen.findByText(T.POPUP_BLOCKED)).toBeInTheDocument();
    expect(ticketMock).not.toHaveBeenCalled();
  });

  it("maps a 404 run_not_found from the ticket route to the ended sentence, and closes the tab", async () => {
    listMock.mockResolvedValue([row(3000, "private")]);
    ticketMock.mockRejectedValue(refusal(404, "run_not_found", "run not found"));
    show();
    const r = await screen.findByTestId("preview-3000");
    await userEvent.click(within(r).getByRole("button", { name: T.OPEN }));
    expect(await within(r).findByText(T.ENDED)).toBeInTheDocument();
    expect(within(r).getByText(T.OPEN_FAILED(3000))).toBeInTheDocument();
    expect(within(r).queryByText("run not found")).toBeNull();
    expect(tab.close).toHaveBeenCalled();
  });

  it.each([
    [404, "preview_unknown", "some other not found"],
    [403, "run_not_found", "a 403 with that reason"],
    [403, "preview_urls_off", "Preview URLs are off on this deployment. An administrator turns them on with features.preview_urls.enabled in site config."],
    [409, "preview_not_running", "This run is not running, so its previews can't be created or opened."],
    [503, "roster_unreadable", "The site configuration could not be read. Try again."],
  ])("shows the server's own sentence for a %i with reason %s", async (status, reason, message) => {
    listMock.mockResolvedValue([row(3000, "private")]);
    ticketMock.mockRejectedValue(refusal(status, reason, message));
    show();
    const r = await screen.findByTestId("preview-3000");
    await userEvent.click(within(r).getByRole("button", { name: T.OPEN }));
    expect(await within(r).findByText(message)).toBeInTheDocument();
    expect(within(r).queryByText(T.ENDED)).toBeNull();
  });
});

describe("PreviewsPanel: who sees what", () => {
  it("shows a security admin on a person's run the admin intro, every row, Revoke on each, Open on an Organisation row only, and no form", async () => {
    listMock.mockResolvedValue([row(3000, "private"), row(8080, "org")]);
    show({}, { principal: "root@example.com", security: true });
    const p = await panel();
    expect(await within(p).findByText(T.ADMIN_INTRO)).toBeInTheDocument();
    expect(within(p).queryByText(T.INTRO)).toBeNull();
    const priv = within(p).getByTestId("preview-3000");
    const org = within(p).getByTestId("preview-8080");
    expect(within(priv).queryByRole("button", { name: T.OPEN })).toBeNull();
    expect(within(priv).getByRole("button", { name: T.REVOKE })).toBeInTheDocument();
    expect(within(org).getByRole("button", { name: T.OPEN })).toBeInTheDocument();
    expect(within(org).queryByRole("button", { name: T.COPY_LINK })).toBeNull();
    expect(within(org).getByRole("button", { name: T.REVOKE })).toBeInTheDocument();
    expect(within(p).queryByRole("button", { name: T.NEW })).toBeNull();
    expect(within(p).queryByRole("button", { name: T.CREATE })).toBeNull();
    expect(within(p).getByText(T.FOOT)).toBeInTheDocument();
  });

  it("shows the run's owner the form even when they are also a security admin", async () => {
    show({}, { security: true });
    const p = await panel();
    expect(await within(p).findByRole("button", { name: T.CREATE })).toBeInTheDocument();
    expect(within(p).queryByText(T.ADMIN_INTRO)).toBeNull();
  });

  it("on a background run says only the background-task sentence: no list read, no form, no foot note", async () => {
    show({ experience: "background" });
    const p = await panel();
    expect(within(p).getByText(RUN_MODE_REFUSAL.BACKGROUND_ONLY("web application gateway"))).toBeInTheDocument();
    expect(within(p).queryByText(T.INTRO)).toBeNull();
    expect(within(p).queryByText(T.FOOT)).toBeNull();
    expect(within(p).queryByRole("button")).toBeNull();
    expect(listMock).not.toHaveBeenCalled();
  });

  it("says the server's sentence when the list cannot be read", async () => {
    listMock.mockRejectedValue(refusal(503, "store_unavailable", "The list could not be read."));
    show();
    const p = await panel();
    expect(await within(p).findByRole("alert")).toHaveTextContent("The list could not be read.");
  });
});
