/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A support helper's doors on the run page (board M5c): the Desktop and Attach tiles draw for the helper of an open
// grant, a lane per open door and one sentence (P-01) for a closed one, and none of the owner's own controls. The
// owner's page is asserted beside it, unchanged.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { RunDetail } from "../../lib/types";
import type { SupportDoor } from "../../lib/api/support";

const healthMock = vi.fn();
vi.mock("../../lib/api/health", () => ({ health: { health: (...a: unknown[]) => healthMock(...a) } }));
const listKeysMock = vi.fn();
vi.mock("../../lib/api/ssh-keys", () => ({ sshKeys: { listKeys: (...a: unknown[]) => listKeysMock(...a) } }));
const attachTicketMock = vi.fn();
vi.mock("../../lib/api/runs", () => ({
  runs: { attachTicket: (...a: unknown[]) => attachTicketMock(...a), getPolicy: () => new Promise(() => {}) },
}));
const signInsGet = vi.fn();
vi.mock("../../lib/api/desktop-sign-ins", () => ({
  desktopSignIns: { get: (...a: unknown[]) => signInsGet(...a), pause: vi.fn(), resume: vi.fn() },
}));
// The Files panel has its own tests; here it is only whether it is drawn.
vi.mock("./run-detail/desktop-files-panel", () => ({ DesktopFilesPanel: () => <div data-testid="files-panel" /> }));

import { DesktopCard } from "./run-detail-desktop";
import { ConnectSSHCard } from "./run-detail-ssh";
import { HttpError } from "../../lib/api/core";
import { RUN_OWNER_ONLY } from "../../lib/run-entry";
import { OperatorProvider } from "../wardyn/operator-context";
import { SupportHelperDoorsContext } from "../wardyn/support-helper-context";
import { DESKTOP_FILES, RUN_CLI, UI_APPS_LANE } from "../wardyn/copy";
import { DESKTOP } from "../wardyn/copy/desktop";
import { INBOUND_APP } from "../wardyn/copy/ui-apps";
import { RUN_SYNC } from "../wardyn/copy/runners";
import { SUPPORT_HELPER } from "../wardyn/copy/support";
import { aheadByHours } from "../../lib/test-clock";

const OWNER = "maya@example.com";
const HELPER = "sam@example.com";
const ALL: SupportDoor[] = ["terminal", "commands", "files", "forward", "web_apps", "desktop"];
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
  agent_exec_id: "exec-1",
  ui_apps: [
    { name: "code-server", port: 8080, path: "/" },
    { name: "preview", port: 3000, path: "/", inbound: true, callers: ["ci-bot"] },
  ],
};
const HEALTH = {
  status: "ok",
  ssh: { enabled: true, advertise_addr: "ssh.example.com:2222" },
  ui_sandbox: { enabled: true, enter_post_url: "http://ui.local/__wardyn/enter", host_mode: true, inbound_url_template: "https://{run}.apps.example.com/{app}/" },
};
const KEY = { fingerprint: "SHA256:x", principal: HELPER, name: "k", public_key: "", created_at: "" };

// doors: null is the owner's page (and any admin's with no open grant); a list is the helper of an open grant, an admin in the admin view.
function wrap(doors: readonly SupportDoor[] | null, who: "owner" | "helper", ui: React.ReactNode) {
  return render(
    <MemoryRouter>
      <OperatorProvider operator={who === "helper"} principal={who === "helper" ? HELPER : OWNER}>
        <SupportHelperDoorsContext.Provider value={doors}>{ui}</SupportHelperDoorsContext.Provider>
      </OperatorProvider>
    </MemoryRouter>,
  );
}
const desk = (doors: readonly SupportDoor[] | null, who: "owner" | "helper" = "helper", over: Partial<RunDetail> = {}) =>
  wrap(doors, who, <DesktopCard run={{ ...run, ...over }} />);
const attach = (doors: readonly SupportDoor[] | null, who: "owner" | "helper" = "helper", over: Partial<RunDetail> = {}) =>
  wrap(doors, who, <ConnectSSHCard run={{ ...run, ...over }} />);
const closed = (...d: SupportDoor[]) => SUPPORT_HELPER.doorsClosed(d);

let openSpy: ReturnType<typeof vi.spyOn>;
beforeEach(() => {
  healthMock.mockReset();
  healthMock.mockResolvedValue(HEALTH);
  listKeysMock.mockReset();
  listKeysMock.mockResolvedValue([KEY]);
  attachTicketMock.mockReset();
  signInsGet.mockReset();
  signInsGet.mockResolvedValue({ store: "memory", paused: false, can_pause: true, agent: "Claude Code" });
  openSpy = vi.spyOn(window, "open").mockReturnValue({ opener: {}, name: "", close: vi.fn(), location: { replace: vi.fn() } } as unknown as Window);
});
afterEach(() => openSpy.mockRestore());

describe("P-01: the closed-door sentence", () => {
  it("names one, two and three or more doors in the banner's order, joined with and", () => {
    expect(closed("files")).toBe("The run's owner did not allow Files in this help session.");
    expect(closed("forward", "files")).toBe("The run's owner did not allow Files and Forward in this help session.");
    expect(closed("desktop", "forward", "terminal")).toBe("The run's owner did not allow Terminal, Forward and Desktop in this help session.");
    expect(closed("terminal", "commands", "files", "forward")).toBe(
      "The run's owner did not allow Terminal, Commands, Files and Forward in this help session.",
    );
  });
});

describe("DesktopCard for a helper", () => {
  it("every door: Open desktop and the Files panel, and none of the owner's own controls", async () => {
    desk(ALL);
    const tile = await screen.findByRole("region", { name: DESKTOP.title });
    await within(tile.parentElement!.parentElement!).findByText(DESKTOP.ready);
    expect(within(tile).getByRole("button", { name: UI_APPS_LANE.cta("desktop") })).toBeInTheDocument();
    expect(within(tile).getByTestId("files-panel")).toBeInTheDocument();
    expect(within(tile).queryByTestId("helper-doors-closed")).toBeNull();
    // The sign-in button, its fetch and the Mac window are the owner's.
    expect(within(tile).queryByRole("button", { name: DESKTOP.pauseToSignIn })).toBeNull();
    expect(signInsGet).not.toHaveBeenCalled();
  });

  it("the owner's tile is as it was: the sign-in button and its read", async () => {
    desk(null, "owner");
    const tile = await screen.findByRole("region", { name: DESKTOP.title });
    expect(await within(tile).findByRole("button", { name: DESKTOP.pauseToSignIn })).toBeInTheDocument();
    expect(within(tile).getByTestId("files-panel")).toBeInTheDocument();
    expect(signInsGet).toHaveBeenCalled();
  });

  it("files only: P-01 where Open desktop would be, no button, no chip, and the Files panel", async () => {
    desk(["files"]);
    const tile = await screen.findByRole("region", { name: DESKTOP.title });
    await waitFor(() => expect(within(tile).getByTestId("files-panel")).toBeInTheDocument());
    expect(within(tile).getByText(closed("desktop"))).toBeInTheDocument();
    expect(within(tile).queryByRole("button")).toBeNull();
    expect(within(tile.parentElement!.parentElement!).queryByText(DESKTOP.ready)).toBeNull();
    expect(within(tile).queryByText(UI_APPS_LANE.newTab)).toBeNull();
  });

  it("desktop only: Open desktop, and the Files heading says P-01 with no panel", async () => {
    desk(["desktop"]);
    const tile = await screen.findByRole("region", { name: DESKTOP.title });
    expect(await within(tile).findByRole("button", { name: UI_APPS_LANE.cta("desktop") })).toBeInTheDocument();
    expect(within(tile).getByText(DESKTOP_FILES.title)).toBeInTheDocument();
    expect(within(tile).getByText(closed("files"))).toBeInTheDocument();
    expect(within(tile).queryByTestId("files-panel")).toBeNull();
  });

  it.each<[string, readonly SupportDoor[] | null, "owner" | "helper", Partial<RunDetail>]>([
    ["web_apps only: no open lane, not drawn", ["web_apps"], "helper", {}],
    ["an admin with no open grant", null, "helper", {}],
    ["a helper on a run that is not running", ["desktop", "files"], "helper", { state: "COMPLETED" }],
    ["a helper on a background run", ["desktop", "files"], "helper", { experience: "background" }],
  ])("draws nothing for %s", async (_n, doors, who, over) => {
    const { container } = desk(doors, who, over);
    await new Promise((r) => setTimeout(r, 0));
    expect(container.querySelector("section")).toBeNull();
  });

  it("files only draws no tile where the card has no Files lane: the gateway off, or a macOS VM with no declared UI app", async () => {
    healthMock.mockResolvedValue({ status: "ok", ssh: HEALTH.ssh });
    const off = desk(["files"]);
    await waitFor(() => expect(healthMock).toHaveBeenCalled());
    await new Promise((r) => setTimeout(r, 0));
    expect(off.container.querySelector("section")).toBeNull();
    off.unmount();
    healthMock.mockResolvedValue(HEALTH);
    const mac = desk(["files"], "helper", { machine_kind: "macos_vm", ui_apps: [] });
    await new Promise((r) => setTimeout(r, 0));
    expect(mac.container.querySelector("section")).toBeNull();
    expect(screen.queryByTestId("files-panel")).toBeNull();
  });

  it("a click answered support_grant_ended reads P-02; run_owner_only keeps its words", async () => {
    attachTicketMock.mockRejectedValueOnce(new HttpError(403, "support grant ended", "support_grant_ended"));
    desk(ALL);
    const tile = await screen.findByRole("region", { name: DESKTOP.title });
    (await within(tile).findByRole("button", { name: UI_APPS_LANE.cta("desktop") })).click();
    expect(await within(tile).findByText(SUPPORT_HELPER.ended)).toBeInTheDocument();
    attachTicketMock.mockRejectedValueOnce(new HttpError(403, "only the person", "run_owner_only"));
    within(tile).getByRole("button", { name: UI_APPS_LANE.cta("desktop") }).click();
    expect(await within(tile).findByText(RUN_OWNER_ONLY)).toBeInTheDocument();
  });
});

describe("the non-VM run's Files panel rides the files door alone", () => {
  const container = { machine_kind: undefined } as Partial<RunDetail>;

  it("files without web apps: the panel is drawn, with no P-01", async () => {
    attach(["files"], "helper", container);
    expect(await screen.findByTestId("files-panel")).toBeInTheDocument();
    expect(screen.queryByText(closed("files"), { exact: true })).toBeNull();
  });

  it("web apps without files: the Files heading says P-01 and no panel is drawn", async () => {
    attach(["web_apps"], "helper", container);
    await screen.findByRole("button", { name: UI_APPS_LANE.cta("code-server") });
    expect(screen.getByText(DESKTOP_FILES.title)).toBeInTheDocument();
    expect(screen.getAllByText(closed("files"), { exact: true }).length).toBeGreaterThan(0);
    expect(screen.queryByTestId("files-panel")).toBeNull();
  });
});

describe("ConnectSSHCard for a helper", () => {
  it("every door: SSH, Sync and UI apps; no CLI lane, no Manage SSH keys, no Only you, no Copy", async () => {
    const { container } = attach(ALL);
    expect(await screen.findByText("ssh " + run.id + "@ssh.example.com -p 2222")).toBeInTheDocument();
    expect(screen.getByText(RUN_SYNC.LANE_TITLE)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: UI_APPS_LANE.cta("code-server") })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: UI_APPS_LANE.cta("preview") })).toBeInTheDocument();
    // The Inbound tag and the callers stay, as facts.
    expect(screen.getByText(INBOUND_APP.TAG)).toBeInTheDocument();
    expect(screen.getByText("ci-bot")).toBeInTheDocument();
    expect(screen.queryByText(RUN_CLI.TITLE)).toBeNull();
    expect(screen.queryByText("Manage SSH keys")).toBeNull();
    expect(screen.queryByText(INBOUND_APP.ONLY_YOU)).toBeNull();
    expect(screen.queryByText(INBOUND_APP.COPY_LINK)).toBeNull();
    expect(screen.queryByText(INBOUND_APP.COPY_ADDRESS)).toBeNull();
    expect(container.querySelector('[data-testid="helper-doors-closed"]')).toBeNull();
  });

  it("the owner's card is as it was: the CLI lane, Manage SSH keys, Only you and both Copy buttons", async () => {
    attach(null, "owner");
    expect(await screen.findByText(RUN_CLI.TITLE)).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText("Manage SSH keys")).toBeInTheDocument());
    expect(screen.getByText(INBOUND_APP.ONLY_YOU)).toBeInTheDocument();
    expect(screen.getByText(INBOUND_APP.COPY_LINK)).toBeInTheDocument();
    expect(screen.getByText(INBOUND_APP.COPY_ADDRESS)).toBeInTheDocument();
    expect(screen.queryByTestId("helper-doors-closed")).toBeNull();
  });

  it("terminal and commands only: the SSH command keeps and names Files and Forward; Sync and UI apps say P-01", async () => {
    attach(["terminal", "commands"]);
    expect(await screen.findByText("ssh " + run.id + "@ssh.example.com -p 2222")).toBeInTheDocument();
    expect(screen.getByText(closed("files", "forward"))).toBeInTheDocument();
    expect(screen.getByText(RUN_SYNC.LANE_TITLE)).toBeInTheDocument();
    expect(screen.getByText(closed("files"))).toBeInTheDocument();
    expect(screen.getByText(UI_APPS_LANE.title)).toBeInTheDocument();
    expect(screen.getByText(closed("web_apps"))).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: UI_APPS_LANE.cta("code-server") })).toBeNull();
    expect(screen.queryByText(RUN_SYNC.LANE_COMMAND(run.id))).toBeNull();
  });

  it("web apps only: SSH and Sync keep their headings and say P-01 with no command; the app rows draw", async () => {
    attach(["web_apps"]);
    expect(await screen.findByRole("button", { name: UI_APPS_LANE.cta("code-server") })).toBeInTheDocument();
    expect(screen.getByText(closed("terminal", "commands", "files", "forward"))).toBeInTheDocument();
    expect(screen.getByText(closed("files"))).toBeInTheDocument();
    expect(screen.queryByText("ssh " + run.id + "@ssh.example.com -p 2222")).toBeNull();
    expect(screen.queryByText(RUN_CLI.TITLE)).toBeNull();
  });

  it("files only on a run with no web apps door: the Files lane is the open one", async () => {
    attach(["files"]);
    expect(await screen.findByText(RUN_SYNC.LANE_COMMAND(run.id))).toBeInTheDocument();
    expect(screen.getByText(closed("terminal", "commands", "forward"))).toBeInTheDocument();
    expect(screen.getByText(closed("web_apps"))).toBeInTheDocument();
  });

  it("a helper with no key registered reads plain words, with no link", async () => {
    listKeysMock.mockResolvedValue([]);
    attach(ALL);
    expect(await screen.findByText("Add your SSH key first")).toBeInTheDocument();
    expect(screen.queryByText("Manage SSH keys")).toBeNull();
    expect(screen.queryByRole("link")).toBeNull();
  });

  it("desktop only: the tile has no open lane and is not drawn", async () => {
    const { container } = attach(["desktop"]);
    await new Promise((r) => setTimeout(r, 0));
    expect(container.querySelector("section, [data-testid]")).toBeNull();
    expect(healthMock).not.toHaveBeenCalled();
  });

  it("a web app click answered support_grant_ended reads P-02, not the owner-only sentence", async () => {
    attachTicketMock.mockRejectedValue(new HttpError(403, "support grant ended", "support_grant_ended"));
    attach(ALL);
    (await screen.findByRole("button", { name: UI_APPS_LANE.cta("code-server") })).click();
    expect(await screen.findByText(SUPPORT_HELPER.ended)).toBeInTheDocument();
    expect(screen.queryByText(RUN_OWNER_ONLY)).toBeNull();
  });
});
