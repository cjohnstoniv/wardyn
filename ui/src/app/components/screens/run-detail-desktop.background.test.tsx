/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Desktop tile on a background run: Watch only when the run's background_watch says open,
// one sentence (the server's own) for every refusal, and the Files panel moved here from the Attach tile with the
// gate it had. The tile's button comes first.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { RunBackgroundWatch, RunDetail } from "../../lib/types";

const healthMock = vi.fn();
vi.mock("../../lib/api/health", () => ({ health: { health: (...a: unknown[]) => healthMock(...a) } }));
const attachTicketMock = vi.fn();
const watchTicketMock = vi.fn();
vi.mock("../../lib/api/runs", () => ({
  runs: { getPolicy: () => new Promise(() => {}), attachTicket: (...a: unknown[]) => attachTicketMock(...a), watchTicket: (...a: unknown[]) => watchTicketMock(...a) },
}));
const signInsGet = vi.fn();
vi.mock("../../lib/api/desktop-sign-ins", () => ({
  desktopSignIns: { get: (...a: unknown[]) => signInsGet(...a), pause: vi.fn(), resume: vi.fn() },
}));
vi.mock("../../lib/api/ssh-keys", () => ({ sshKeys: { listKeys: async () => [] } }));
vi.mock("./run-detail/desktop-files-panel", () => ({ DesktopFilesPanel: () => <div data-testid="files-panel" /> }));

import { DesktopCard } from "./run-detail-desktop";
import { ConnectSSHCard } from "./run-detail-ssh";
import { OperatorProvider } from "../wardyn/operator-context";
import { RUN_SSH, UI_APPS_LANE } from "../wardyn/copy";
import { DESKTOP } from "../wardyn/copy/desktop";
import { RUN_MODE_REFUSAL } from "../../lib/run-mode-refusals";
import { aheadByHours } from "../../lib/test-clock";

const OWNER = "alice@example.com";
const run: RunDetail = {
  id: "33333333-3333-3333-3333-333333333333",
  created_at: aheadByHours(-1),
  updated_at: aheadByHours(-1),
  created_by: OWNER,
  agent: "claude-code",
  repo: "acme/widgets",
  task: "do the thing",
  confinement_class: "CC2",
  state: "RUNNING",
  spiffe_id: "spiffe://wardyn.local/agent-run/33333333-3333-3333-3333-333333333333",
  runner_target: "docker",
  placement: "",
  machine_kind: "macos_vm",
  experience: "background",
};
const gatewayOn = { status: "ok", ui_sandbox: { enabled: true, enter_post_url: "http://ui.local/__wardyn/enter" } };

const renderCard = (over: Partial<RunDetail> = {}) =>
  render(
    <MemoryRouter>
      <OperatorProvider operator={false} principal={OWNER}>
        <DesktopCard run={{ ...run, ...over }} />
      </OperatorProvider>
    </MemoryRouter>,
  );
const bg = (state: RunBackgroundWatch["state"], message?: string): Partial<RunDetail> => ({ background_watch: { state, message } });
const tile = (region: HTMLElement) => region.parentElement!.parentElement!;
const desk = () => screen.findByRole("region", { name: DESKTOP.title });

// A stubbed window.open: the new tab the click opens before any request.
let tabStub: { opener: unknown; name: string; close: ReturnType<typeof vi.fn>; location: { replace: ReturnType<typeof vi.fn> } };
let openSpy: ReturnType<typeof vi.spyOn>;

beforeEach(() => {
  tabStub = { opener: {}, name: "", close: vi.fn(), location: { replace: vi.fn() } };
  openSpy = vi.spyOn(window, "open").mockReturnValue(tabStub as unknown as Window);
  healthMock.mockReset();
  attachTicketMock.mockReset();
  watchTicketMock.mockReset();
  signInsGet.mockReset();
  healthMock.mockResolvedValue(gatewayOn);
  signInsGet.mockResolvedValue({ store: "run_disk", paused: false, can_pause: true, agent: "Claude Code" });
});

afterEach(() => openSpy.mockRestore());

describe("DesktopCard: a background macOS run", () => {
  it("Watch opens the tab before the watch ticket is minted, and closes it when the ticket is refused", async () => {
    let fail: (e: Error) => void = () => {};
    watchTicketMock.mockReturnValue(new Promise((_, rej) => (fail = rej)));
    renderCard(bg("open"));
    const d = await desk();
    within(d).getByRole("button", { name: DESKTOP.watch }).click();
    await waitFor(() => expect(openSpy).toHaveBeenCalledTimes(1));
    expect(tabStub.close).not.toHaveBeenCalled();
    fail(new Error("not allowed right now"));
    await waitFor(() => expect(tabStub.close).toHaveBeenCalled());
  });

  it("Watch says the browser blocked the new tab, and mints no ticket, when window.open returns null", async () => {
    openSpy.mockReturnValue(null);
    renderCard(bg("open"));
    const d = await desk();
    within(d).getByRole("button", { name: DESKTOP.watch }).click();
    expect(await within(d).findByText(RUN_SSH.POPUP_BLOCKED)).toBeInTheDocument();
    expect(watchTicketMock).not.toHaveBeenCalled();
  });

  it("open: the kind line as the tile's subtitle, Watch desktop next, then the three sentences, View only in the header, no sign-ins and no Files", async () => {
    renderCard({ ...bg("open"), ui_apps: [{ name: "novnc", port: 6080 }] });
    const d = await desk();
    const button = within(d).getByRole("button", { name: DESKTOP.watch });
    expect(d.firstElementChild).toHaveTextContent(DESKTOP.kindMacOS);
    expect(d.children[1]).toContainElement(button);
    expect(within(d).getAllByRole("button")).toHaveLength(1);
    expect(within(tile(d)).getByText(DESKTOP.viewOnlyChip)).toBeInTheDocument();
    expect(within(d).getByText(DESKTOP.kindMacOS)).toBeInTheDocument();
    for (const line of [DESKTOP.backgroundWatchTile, DESKTOP.backgroundWatchExposure, UI_APPS_LANE.newTab]) {
      expect(within(d).getByText(line)).toBeInTheDocument();
    }
    expect(within(d).queryByText(DESKTOP.observeOnlyMacOS)).toBeNull();
    expect(screen.queryByTestId("files-panel")).toBeNull();
    expect(signInsGet).not.toHaveBeenCalled();
  });

  it("Watch spends the watch ticket, not the terminal's, at app __desktop", async () => {
    watchTicketMock.mockResolvedValue("tkt_watch");
    const submitSpy = vi.spyOn(HTMLFormElement.prototype, "submit").mockImplementation(() => {});
    renderCard(bg("open"));
    const d = await desk();
    within(d).getByRole("button", { name: DESKTOP.watch }).click();
    await waitFor(() => expect(submitSpy).toHaveBeenCalled());
    expect(watchTicketMock).toHaveBeenCalledWith(run.id);
    expect(attachTicketMock).not.toHaveBeenCalled();
    const form = submitSpy.mock.instances[0] as HTMLFormElement;
    expect(form.action).toBe("http://ui.local/__wardyn/enter");
    expect(Object.fromEntries(new FormData(form).entries())).toEqual({ run: run.id, app: "__desktop", ticket: "tkt_watch" });
    submitSpy.mockRestore();
  });

  it("a refused ticket shows its error in the tile and opens nothing", async () => {
    watchTicketMock.mockRejectedValue(new Error("not allowed right now"));
    const submitSpy = vi.spyOn(HTMLFormElement.prototype, "submit").mockImplementation(() => {});
    renderCard(bg("open"));
    const d = await desk();
    within(d).getByRole("button", { name: DESKTOP.watch }).click();
    expect(await within(d).findByRole("alert")).toHaveTextContent("not allowed right now");
    expect(submitSpy).not.toHaveBeenCalled();
    submitSpy.mockRestore();
  });

  it.each<[RunBackgroundWatch["state"], string]>([
    ["site_off", "Watching background desktops is turned off for this install. An admin can turn it on."],
    ["governance", "Your organisation's governance profile doesn't allow watching a background run's desktop."],
    ["sign_ins_off", "A sentence the server wrote for sign-ins."],
    ["old_boot", "This run can't be watched. It started before this Mac's runner could keep a background desktop view only."],
    ["unconfirmed", "Wardyn can't confirm this desktop is view only, so it can't be watched."],
    ["offline", "This Mac's runner is offline, so Wardyn can't confirm the desktop is view only. Wardyn checks again when it reconnects."],
    ["paused", "This run is paused. You can watch it once it is running again."],
  ])("%s: the server's sentence, View only in the header, and no button", async (state, message) => {
    renderCard(bg(state, message));
    const d = await desk();
    expect(await within(d).findByText(message)).toBeInTheDocument();
    expect(within(tile(d)).getByText(DESKTOP.viewOnlyChip)).toBeInTheDocument();
    expect(within(d).queryByRole("button")).toBeNull();
    expect(within(d).queryByText(DESKTOP.backgroundWatchExposure)).toBeNull();
  });

  it("control: the sentence and no chip, no button", async () => {
    const message = "This run is a background task on a Mac that allows desktop control, so its desktop can't be watched. Its logs, audit trail and status stay available.";
    renderCard(bg("control", message));
    const d = await desk();
    expect(await within(d).findByText(message)).toBeInTheDocument();
    expect(within(tile(d)).queryByText(DESKTOP.viewOnlyChip)).toBeNull();
    expect(within(d).queryByRole("button")).toBeNull();
  });

  it("not_run_owner says nothing and offers nothing", async () => {
    renderCard(bg("not_run_owner"));
    const d = await desk();
    await waitFor(() => expect(healthMock).toHaveBeenCalled());
    expect(within(d).queryByRole("button")).toBeNull();
    expect(d.textContent).toBe(DESKTOP.kindMacOS);
  });

  it("an older server that sends no background_watch offers nothing", async () => {
    renderCard();
    const d = await desk();
    await waitFor(() => expect(healthMock).toHaveBeenCalled());
    expect(within(d).queryByRole("button")).toBeNull();
    expect(d.textContent).toBe(DESKTOP.kindMacOS);
  });

  it("with the gateway off, an open watch has no button and says the deployment has it off, and promises no watching", async () => {
    healthMock.mockResolvedValue({ status: "ok" });
    renderCard(bg("open"));
    const d = await desk();
    expect(await within(d).findByText(/Off on this deployment/)).toBeInTheDocument();
    expect(within(d).queryByRole("button")).toBeNull();
    for (const line of [DESKTOP.backgroundWatchTile, DESKTOP.backgroundWatchExposure, UI_APPS_LANE.newTab]) {
      expect(within(d).queryByText(line)).toBeNull();
    }
  });

  it("a finished background run keeps only the disk line, with the tile Off", async () => {
    renderCard({ state: "COMPLETED", macos_disk: { state: "removing" } });
    const d = await desk();
    expect(within(tile(d)).getByText(DESKTOP.statusOff)).toBeInTheDocument();
    expect(within(d).getByText(DESKTOP.diskRemoving)).toBeInTheDocument();
    expect(within(d).queryByRole("button")).toBeNull();
  });
});

describe("DesktopCard: a background Linux VM run", () => {
  it("has the existing no-desktop sentence, no chip and no button, and asks for no sign-ins", async () => {
    renderCard({ machine_kind: "linux_vm", ui_apps: [{ name: "novnc", port: 6080 }] });
    const d = await desk();
    expect(within(d).getByText(RUN_MODE_REFUSAL.BACKGROUND_ONLY("desktop"))).toBeInTheDocument();
    expect(within(d).getByText(DESKTOP.kind)).toBeInTheDocument();
    expect(within(d).queryByRole("button")).toBeNull();
    expect(within(tile(d)).queryByText(DESKTOP.ready)).toBeNull();
    expect(within(tile(d)).queryByText(DESKTOP.viewOnlyChip)).toBeNull();
    expect(screen.queryByTestId("files-panel")).toBeNull();
    await waitFor(() => expect(healthMock).toHaveBeenCalled());
    expect(signInsGet).not.toHaveBeenCalled();
  });
});

describe("DesktopCard: the Files panel moved here with its gate", () => {
  const interactive: Partial<RunDetail> = { experience: "interactive", machine_kind: "linux_vm", interactive: true };
  const apps = [{ name: "novnc", port: 6080 }];

  it("shows under the buttons, before the sign-ins note, when the run declares a UI app", async () => {
    renderCard({ ...interactive, ui_apps: apps });
    const d = await desk();
    const panel = await screen.findByTestId("files-panel");
    expect(d).toContainElement(panel);
    const buttons = within(d).getByRole("button", { name: UI_APPS_LANE.cta("desktop") });
    expect(buttons.compareDocumentPosition(panel) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("shows for a Linux VM's built-in desktop with no declared UI app", async () => {
    renderCard({ ...interactive });
    const d = await desk();
    expect(d).toContainElement(await screen.findByTestId("files-panel"));
  });

  it("is absent on a macOS VM with no declared UI app", async () => {
    renderCard({ ...interactive, machine_kind: "macos_vm" });
    await desk();
    await waitFor(() => expect(healthMock).toHaveBeenCalled());
    expect(screen.queryByTestId("files-panel")).toBeNull();
  });

  it("is absent when the gateway is off", async () => {
    healthMock.mockResolvedValue({ status: "ok" });
    renderCard({ ...interactive, ui_apps: apps });
    await desk();
    await waitFor(() => expect(healthMock).toHaveBeenCalled());
    expect(screen.queryByTestId("files-panel")).toBeNull();
  });

  it("is absent once the run has ended", async () => {
    renderCard({ ...interactive, ui_apps: apps, state: "COMPLETED" });
    await waitFor(() => expect(screen.queryByRole("region", { name: DESKTOP.title })).toBeNull());
    expect(screen.queryByTestId("files-panel")).toBeNull();
  });
});

describe("ConnectSSHCard: the desktop left it", () => {
  const renderAttach = (over: Partial<RunDetail>) =>
    render(
      <MemoryRouter>
        <OperatorProvider operator={false} principal={OWNER}>
          <ConnectSSHCard run={{ ...run, ...over }} />
        </OperatorProvider>
      </MemoryRouter>,
    );

  it("an interactive VM run's Attach card holds no Desktop region and no desktop buttons", async () => {
    renderAttach({ experience: "interactive", machine_kind: "linux_vm", ui_apps: [{ name: "novnc", port: 6080 }] });
    await screen.findByText("Attach from your terminal");
    await waitFor(() => expect(healthMock).toHaveBeenCalled());
    expect(screen.queryByRole("region", { name: DESKTOP.title })).toBeNull();
    expect(screen.queryByRole("button", { name: UI_APPS_LANE.cta("desktop") })).toBeNull();
    expect(screen.queryByTestId("files-panel")).toBeNull();
    expect(signInsGet).not.toHaveBeenCalled();
  });

  const apps = [{ name: "vscode", port: 8080 }];

  it("keeps the Files panel for a container run with a declared UI app, which has no Desktop tile", async () => {
    renderAttach({ experience: "interactive", machine_kind: undefined, interactive: true, ui_apps: apps });
    expect(await screen.findByTestId("files-panel")).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: DESKTOP.title })).toBeNull();
  });

  it("has no Files panel for a container run with no declared UI app", async () => {
    renderAttach({ experience: "interactive", machine_kind: undefined, interactive: true });
    await screen.findByText("Attach from your terminal");
    await waitFor(() => expect(healthMock).toHaveBeenCalled());
    expect(screen.queryByTestId("files-panel")).toBeNull();
  });

  it("has no Files panel, and no card, once the container run has ended", () => {
    const { container } = renderAttach({ experience: "interactive", machine_kind: undefined, state: "COMPLETED", ui_apps: apps });
    expect(container).toBeEmptyDOMElement();
  });

  it("has no Files panel for a container run when the gateway is off", async () => {
    healthMock.mockResolvedValue({ status: "ok" });
    renderAttach({ experience: "interactive", machine_kind: undefined, interactive: true, ui_apps: apps });
    await screen.findByText("Attach from your terminal");
    await waitFor(() => expect(healthMock).toHaveBeenCalled());
    expect(screen.queryByTestId("files-panel")).toBeNull();
  });

  it("shows a VM run's Files panel once, in the Desktop tile and not the Attach tile", async () => {
    render(
      <MemoryRouter>
        <OperatorProvider operator={false} principal={OWNER}>
          <DesktopCard run={{ ...run, experience: "interactive", machine_kind: "linux_vm", interactive: true, ui_apps: apps }} />
          <ConnectSSHCard run={{ ...run, experience: "interactive", machine_kind: "linux_vm", interactive: true, ui_apps: apps }} />
        </OperatorProvider>
      </MemoryRouter>,
    );
    await screen.findByRole("region", { name: DESKTOP.title });
    await waitFor(() => expect(screen.getAllByTestId("files-panel")).toHaveLength(1));
    expect(screen.getByRole("region", { name: DESKTOP.title })).toContainElement(screen.getByTestId("files-panel"));
  });

  it("a background run has no Attach card at all", async () => {
    const { container } = renderAttach({ ...bg("open") });
    await waitFor(() => expect(healthMock).toHaveBeenCalled());
    expect(container).toBeEmptyDOMElement();
  });
});
