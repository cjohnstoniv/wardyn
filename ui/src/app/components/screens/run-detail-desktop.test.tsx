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
const signInsGet = vi.fn();
const signInsPause = vi.fn();
const signInsResume = vi.fn();
vi.mock("../../lib/api/desktop-sign-ins", () => ({
  desktopSignIns: {
    get: (...a: unknown[]) => signInsGet(...a),
    pause: (...a: unknown[]) => signInsPause(...a),
    resume: (...a: unknown[]) => signInsResume(...a),
  },
}));

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
  signInsGet.mockReset();
  signInsPause.mockReset();
  signInsResume.mockReset();
  // A run whose guest reported no sign-in store: the block says nothing about sign-ins.
  signInsGet.mockResolvedValue({ store: "", paused: false, can_pause: false, agent: "Claude Code" });
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

// Sign-ins in a desktop (#2972, board R11-2972-DesktopSignIns): the three states, byte for byte.
describe("ConnectSSHCard — Desktop sign-ins", () => {
  const gatewayOn = { status: "ok", ui_sandbox: { enabled: true, enter_post_url: "http://ui.local/__wardyn/enter" } };
  const allowed = { store: "memory", paused: false, can_pause: true, agent: "Claude Code" };

  it("state 1: Ready, the pause button beside Open desktop, and the memory note last in the block", async () => {
    healthMock.mockResolvedValue(gatewayOn);
    signInsGet.mockResolvedValue(allowed);
    renderCard();
    const desk = await screen.findByRole("region", { name: DESKTOP.title });
    const pause = await within(desk).findByRole("button", { name: "Pause the agent to sign in" });
    expect(within(desk).getByText(DESKTOP.ready)).toBeInTheDocument();
    expect(within(desk).getByRole("button", { name: UI_APPS_LANE.cta("desktop") })).toBeInTheDocument();
    const note = within(desk).getByText(
      "Sign-ins you make in this desktop are kept in this machine's memory and are gone when it shuts down: at the run's end, or when the run is killed. While the run goes on, the agent can use them too. Closing this tab doesn't remove them. Anything you save to a file stays with the run's files, where the agent can read it.",
    );
    expect(desk.lastElementChild).toBe(note);
    expect(pause).toBeEnabled();
    expect(signInsGet).toHaveBeenCalledWith(run.id, expect.anything());
  });

  it("state 2: Pause turns the chip to Agent paused, shows the disclosure, and Resume continues the agent", async () => {
    healthMock.mockResolvedValue(gatewayOn);
    signInsGet.mockResolvedValue(allowed);
    signInsPause.mockResolvedValue({ ...allowed, paused: true, paused_at: aheadByHours(0) });
    signInsResume.mockResolvedValue(allowed);
    renderCard();
    const desk = await screen.findByRole("region", { name: DESKTOP.title });
    (await within(desk).findByRole("button", { name: "Pause the agent to sign in" })).click();
    await within(desk).findByText("Agent paused");
    expect(signInsPause).toHaveBeenCalledWith(run.id);
    expect(within(desk).queryByText(DESKTOP.ready)).toBeNull();
    expect(within(desk).getByText("The agent is paused while you sign in. Once you resume it, it can use what you signed into.")).toBeInTheDocument();
    expect(within(desk).queryByRole("button", { name: "Pause the agent to sign in" })).toBeNull();
    within(desk).getByRole("button", { name: "Resume the agent" }).click();
    await within(desk).findByRole("button", { name: "Pause the agent to sign in" });
    expect(signInsResume).toHaveBeenCalledWith(run.id);
    expect(within(desk).getByText(DESKTOP.ready)).toBeInTheDocument();
    expect(within(desk).queryByText(DESKTOP.pausedNote)).toBeNull();
  });

  it("state 3: Not allowed for this agent shows no pause button and names the agent", async () => {
    healthMock.mockResolvedValue(gatewayOn);
    signInsGet.mockResolvedValue({ store: "none", paused: false, can_pause: false, agent: "Claude Code" });
    renderCard();
    const desk = await screen.findByRole("region", { name: DESKTOP.title });
    await within(desk).findByText("Your admin doesn't allow sign-ins in this desktop while Claude Code runs, so this desktop keeps none.");
    expect(within(desk).queryByRole("button", { name: DESKTOP.pauseToSignIn })).toBeNull();
    expect(within(desk).queryByText(DESKTOP.signInsNote)).toBeNull();
  });

  it("says nothing about sign-ins for a run whose guest reported no store", async () => {
    healthMock.mockResolvedValue(gatewayOn);
    renderCard();
    const desk = await screen.findByRole("region", { name: DESKTOP.title });
    await within(desk).findByText(DESKTOP.ready);
    await waitFor(() => expect(signInsGet).toHaveBeenCalled());
    expect(within(desk).queryByRole("button", { name: DESKTOP.pauseToSignIn })).toBeNull();
    expect(desk.textContent).not.toContain("Sign-ins");
  });

  it("shows the server's refusal and keeps the agent's state when a pause fails", async () => {
    healthMock.mockResolvedValue(gatewayOn);
    signInsGet.mockResolvedValue(allowed);
    signInsPause.mockRejectedValue(new Error("the agent could not be paused; it keeps running"));
    renderCard();
    const desk = await screen.findByRole("region", { name: DESKTOP.title });
    (await within(desk).findByRole("button", { name: DESKTOP.pauseToSignIn })).click();
    expect(await within(desk).findByRole("alert")).toHaveTextContent("the agent could not be paused; it keeps running");
    expect(within(desk).getByText(DESKTOP.ready)).toBeInTheDocument();
  });
});
