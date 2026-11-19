/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Desktop tile (its own tile, not a block of the attach card): a Linux desktop VM run only, opened through the UI gateway's
// ticket -> bind -> POST enter hand-off for the built-in desktop app, and off when the gateway is.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { RunDetail } from "../../lib/types";

const healthMock = vi.fn();
vi.mock("../../lib/api/health", () => ({ health: { health: (...a: unknown[]) => healthMock(...a) } }));
const attachTicketMock = vi.fn();
vi.mock("../../lib/api/runs", () => ({ runs: { getPolicy: () => new Promise(() => {}), attachTicket: (...a: unknown[]) => attachTicketMock(...a) } }));
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

import { DesktopCard } from "./run-detail-desktop";
import { OperatorProvider } from "../wardyn/operator-context";
import { RUN_SSH, UI_APPS_LANE } from "../wardyn/copy";
import { DESKTOP } from "../wardyn/copy/desktop";
import { KEEP_RUN } from "../wardyn/copy/keep-machine";
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
        <DesktopCard run={{ ...run, ...over }} />
      </OperatorProvider>
    </MemoryRouter>,
  );

// The chip is in the tile's header, a sibling of the region that holds the body.
const tile = (region: HTMLElement) => region.parentElement!.parentElement!;

// A stubbed window.open: the new tab the click opens before any request.
let tabStub: { opener: unknown; name: string; close: ReturnType<typeof vi.fn>; location: { replace: ReturnType<typeof vi.fn> } };
let openSpy: ReturnType<typeof vi.spyOn>;

beforeEach(() => {
  tabStub = { opener: {}, name: "", close: vi.fn(), location: { replace: vi.fn() } };
  openSpy = vi.spyOn(window, "open").mockReturnValue(tabStub as unknown as Window);
  healthMock.mockReset();
  attachTicketMock.mockReset();
  signInsGet.mockReset();
  signInsPause.mockReset();
  signInsResume.mockReset();
  // A run whose guest reported no sign-in store: the block says nothing about sign-ins.
  signInsGet.mockResolvedValue({ store: "", paused: false, can_pause: false, agent: "Claude Code" });
});

afterEach(() => openSpy.mockRestore());

describe("DesktopCard — Desktop", () => {
  it("opens a named blank tab before the ticket request, closes it when the ticket fails, and says when the browser blocks it", async () => {
    healthMock.mockResolvedValue({ status: "ok", ui_sandbox: { enabled: true, enter_post_url: "http://ui.local/__wardyn/enter" } });
    let fail: (e: Error) => void = () => {};
    attachTicketMock.mockReturnValue(new Promise((_, rej) => (fail = rej)));
    renderCard();
    const desk = await screen.findByRole("region", { name: DESKTOP.title });
    await within(tile(desk)).findByText(DESKTOP.ready);
    within(desk).getByRole("button", { name: UI_APPS_LANE.cta("desktop") }).click();
    await waitFor(() => expect(openSpy).toHaveBeenCalledTimes(1));
    expect(openSpy.mock.calls[0][1]).toMatch(/^wardyn-enter-/);
    expect(tabStub.opener).toBeNull();
    expect(tabStub.close).not.toHaveBeenCalled();
    fail(new Error("no ticket"));
    await waitFor(() => expect(tabStub.close).toHaveBeenCalled());
  });

  it("says the browser blocked the new tab, and requests no ticket, when window.open returns null", async () => {
    healthMock.mockResolvedValue({ status: "ok", ui_sandbox: { enabled: true, enter_post_url: "http://ui.local/__wardyn/enter" } });
    openSpy.mockReturnValue(null);
    renderCard();
    const desk = await screen.findByRole("region", { name: DESKTOP.title });
    await within(tile(desk)).findByText(DESKTOP.ready);
    within(desk).getByRole("button", { name: UI_APPS_LANE.cta("desktop") }).click();
    expect(await within(desk).findByText(RUN_SSH.POPUP_BLOCKED)).toBeInTheDocument();
    expect(attachTicketMock).not.toHaveBeenCalled();
  });

  it("opens the VM's desktop through a bound ticket and a POSTed form, as the built-in desktop app", async () => {
    healthMock.mockResolvedValue({ status: "ok", ui_sandbox: { enabled: true, enter_post_url: "http://ui.local/__wardyn/enter" } });
    attachTicketMock.mockResolvedValue("tkt_desk");
    const submitSpy = vi.spyOn(HTMLFormElement.prototype, "submit").mockImplementation(() => {});
    renderCard();
    const desk = await screen.findByRole("region", { name: DESKTOP.title });
    await within(tile(desk)).findByText(DESKTOP.ready);
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
    await within(tile(desk)).findByText(DESKTOP.statusOff);
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
describe("DesktopCard — Desktop sign-ins", () => {
  const gatewayOn = { status: "ok", ui_sandbox: { enabled: true, enter_post_url: "http://ui.local/__wardyn/enter" } };
  const allowed = { store: "memory", paused: false, can_pause: true, agent: "Claude Code" };

  it("state 1: Ready, the pause button beside Open desktop, and the memory note last in the tile", async () => {
    healthMock.mockResolvedValue(gatewayOn);
    signInsGet.mockResolvedValue(allowed);
    renderCard();
    const desk = await screen.findByRole("region", { name: DESKTOP.title });
    const pause = await within(desk).findByRole("button", { name: "Pause the agent to sign in" });
    expect(within(tile(desk)).getByText(DESKTOP.ready)).toBeInTheDocument();
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
    await within(tile(desk)).findByText("Agent paused");
    expect(signInsPause).toHaveBeenCalledWith(run.id);
    expect(within(tile(desk)).queryByText(DESKTOP.ready)).toBeNull();
    expect(within(desk).getByText("The agent is paused while you sign in. Once you resume it, it can use what you signed into.")).toBeInTheDocument();
    expect(within(desk).queryByRole("button", { name: "Pause the agent to sign in" })).toBeNull();
    within(desk).getByRole("button", { name: "Resume the agent" }).click();
    await within(desk).findByRole("button", { name: "Pause the agent to sign in" });
    expect(signInsResume).toHaveBeenCalledWith(run.id);
    expect(within(tile(desk)).getByText(DESKTOP.ready)).toBeInTheDocument();
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

  it("km3-desktop-signins-after: a run that keeps its machine says K-45 in place of the run disk's note", async () => {
    healthMock.mockResolvedValue(gatewayOn);
    signInsGet.mockResolvedValue({ ...allowed, store: "run_disk" });
    renderCard({ machine: { id: "m-dev", name: "dev", keep_at_end: true, scrubbed: false, runner_name: "Maya's MacBook", deleted: false } });
    const desk = await screen.findByRole("region", { name: DESKTOP.title });
    expect(await within(desk).findByText(KEEP_RUN.SIGN_INS_NOTE("Maya's MacBook"))).toBeInTheDocument();
    expect(within(desk).queryByText(DESKTOP.signInsNoteRunDisk)).toBeNull();
  });

  it("km3-desktop-signins-before: a run that keeps nothing says the run disk's note", async () => {
    healthMock.mockResolvedValue(gatewayOn);
    signInsGet.mockResolvedValue({ ...allowed, store: "run_disk" });
    renderCard();
    const desk = await screen.findByRole("region", { name: DESKTOP.title });
    expect(await within(desk).findByText(DESKTOP.signInsNoteRunDisk)).toBeInTheDocument();
  });

  it("says nothing about sign-ins for a run whose guest reported no store", async () => {
    healthMock.mockResolvedValue(gatewayOn);
    renderCard();
    const desk = await screen.findByRole("region", { name: DESKTOP.title });
    await within(tile(desk)).findByText(DESKTOP.ready);
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
    const alert = await within(desk).findByRole("alert");
    expect(alert).toHaveTextContent("the agent could not be paused; it keeps running");
    expect(within(tile(desk)).getByText(DESKTOP.ready)).toBeInTheDocument();
    // UXR-28: the error sits under the button row, above the notes, not at the foot of the tile. The kind line is the
    // tile's subtitle, above the buttons.
    const kind = within(desk).getByText(DESKTOP.kind);
    expect(alert.compareDocumentPosition(kind) & Node.DOCUMENT_POSITION_PRECEDING).toBeTruthy();
    const note = within(desk).getByText(UI_APPS_LANE.newTab);
    expect(alert.compareDocumentPosition(note) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });
});
