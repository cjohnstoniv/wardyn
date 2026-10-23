/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Desktop block of the attach card (mock M6): a Linux desktop VM run only, opened through the UI gateway's
// ticket -> bind -> POST enter hand-off for the built-in desktop app, and off when the gateway is.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { RunDetail } from "../../lib/types";

const healthMock = vi.fn();
vi.mock("../../lib/api/health", () => ({ health: { health: (...a: unknown[]) => healthMock(...a) } }));
vi.mock("../../lib/api/ssh-keys", () => ({ sshKeys: { listKeys: async () => [] } }));
const attachTicketMock = vi.fn();
vi.mock("../../lib/api/runs", () => ({ runs: { attachTicket: (...a: unknown[]) => attachTicketMock(...a) } }));

import { ConnectSSHCard } from "./run-detail-ssh";
import { OperatorProvider } from "../wardyn/operator-context";
import { UI_APPS_LANE } from "../wardyn/copy";
import { DESKTOP } from "../wardyn/copy/desktop";
import { aheadByHours } from "../../lib/test-clock";

const OWNER = "alice@example.com";
const run: RunDetail = {
  id: "11111111-1111-1111-1111-111111111111",
  created_at: aheadByHours(-1),
  updated_at: aheadByHours(-1),
  created_by: OWNER,
  agent: "claude-code",
  repo: "acme/widgets",
  task: "do the thing",
  confinement_class: "CC2",
  state: "RUNNING",
  spiffe_id: "spiffe://wardyn.local/agent-run/11111111-1111-1111-1111-111111111111",
  runner_target: "docker",
  placement: "",
  interactive: true,
  machine_kind: "linux_vm",
};

const renderCard = (over: Partial<RunDetail> = {}) =>
  render(
    <MemoryRouter>
      <OperatorProvider operator={false} principal={OWNER}>
        <ConnectSSHCard run={{ ...run, ...over }} />
      </OperatorProvider>
    </MemoryRouter>,
  );

beforeEach(() => {
  healthMock.mockReset();
  attachTicketMock.mockReset();
});

describe("ConnectSSHCard — Desktop", () => {
  it("opens the VM's desktop through a bound ticket and a POSTed form, as the built-in desktop app", async () => {
    healthMock.mockResolvedValue({ status: "ok", ui_sandbox: { enabled: true, enter_post_url: "http://ui.local/__wardyn/enter" } });
    attachTicketMock.mockResolvedValue("tkt_desk");
    const submitSpy = vi.spyOn(HTMLFormElement.prototype, "submit").mockImplementation(() => {});
    renderCard();
    const desk = await screen.findByRole("region", { name: DESKTOP.title });
    await within(desk).findByText(DESKTOP.ready);
    expect(within(desk).getByText(DESKTOP.kind)).toBeInTheDocument();
    expect(within(desk).getByText(DESKTOP.phoneBefore)).toBeInTheDocument();
    within(desk).getByRole("button", { name: UI_APPS_LANE.cta("desktop") }).click();
    await waitFor(() => expect(submitSpy).toHaveBeenCalled());
    expect(attachTicketMock).toHaveBeenCalledWith(run.id, "ui");
    const form = submitSpy.mock.instances[0] as HTMLFormElement;
    expect(form.action).toBe("http://ui.local/__wardyn/enter");
    expect(Object.fromEntries(new FormData(form).entries())).toEqual({ run: run.id, app: "__desktop", ticket: "tkt_desk" });
    submitSpy.mockRestore();
  });

  it("says the deployment has it off, with no button, when the gateway is off", async () => {
    healthMock.mockResolvedValue({ status: "ok" });
    renderCard();
    const desk = await screen.findByRole("region", { name: DESKTOP.title });
    await within(desk).findByText(DESKTOP.statusOff);
    expect(desk.textContent).toContain("Off on this deployment. It relays the desktop of a sandbox to your browser through Wardyn.");
    expect(within(desk).queryByRole("button")).toBeNull();
  });

  it("is absent on a container run", async () => {
    healthMock.mockResolvedValue({ status: "ok", ui_sandbox: { enabled: true, enter_post_url: "http://ui.local/__wardyn/enter" } });
    renderCard({ machine_kind: undefined });
    await waitFor(() => expect(healthMock).toHaveBeenCalled());
    expect(screen.queryByRole("region", { name: DESKTOP.title })).toBeNull();
    expect(screen.queryByRole("button", { name: UI_APPS_LANE.cta("desktop") })).toBeNull();
  });
});
