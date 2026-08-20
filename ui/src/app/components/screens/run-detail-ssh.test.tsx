/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// ConnectSSHCard (prompt-v3-ssh-pane.md): owner-only, running-only,
// gateway-enabled-only visibility, and the no-keys-yet lead-in. Standalone
// (ConnectSSHCard is exported for exactly this) rather than mounting the
// whole RunDetailScreen's fetch graph. Moved out of run-detail.test.tsx
// alongside the component (B4 file-size gate) — zero behavior change.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { AgentRun } from "../../lib/types";

const healthMock = vi.fn();
vi.mock("../../lib/api/health", () => ({
  health: { health: (...a: unknown[]) => healthMock(...a) },
}));

const listKeysMock = vi.fn();
vi.mock("../../lib/api/ssh-keys", () => ({
  sshKeys: { listKeys: (...a: unknown[]) => listKeysMock(...a) },
}));

const attachTicketMock = vi.fn();
vi.mock("../../lib/api/runs", () => ({
  runs: { attachTicket: (...a: unknown[]) => attachTicketMock(...a) },
}));

import { ConnectSSHCard } from "./run-detail-ssh";
import { OperatorProvider } from "../wardyn/operator-context";
import { UI_APPS_LANE } from "../wardyn/copy";

const OWNER = "alice@example.com";
const baseRun: AgentRun = {
  id: "11111111-1111-1111-1111-111111111111",
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
  created_by: OWNER,
  agent: "claude-code",
  repo: "acme/widgets",
  task: "do the thing",
  confinement_class: "CC2",
  state: "RUNNING",
  spiffe_id: "spiffe://wardyn.local/agent-run/11111111-1111-1111-1111-111111111111",
  runner_target: "docker",
  interactive: false,
};

function renderCard(run: Partial<AgentRun> = {}, principal = OWNER) {
  return render(
    <MemoryRouter>
      <OperatorProvider operator principal={principal}>
        <ConnectSSHCard run={{ ...baseRun, ...run }} />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  healthMock.mockReset();
  listKeysMock.mockReset();
  attachTicketMock.mockReset();
});

describe("ConnectSSHCard — visibility", () => {
  it("renders nothing for a run the caller does not own", async () => {
    healthMock.mockResolvedValue({ ssh: { enabled: true, advertise_addr: "wardyn.corp.example:2222" } });
    listKeysMock.mockResolvedValue([{ fingerprint: "SHA256:x", principal: OWNER, name: "k", public_key: "", created_at: "" }]);
    const { container } = renderCard({}, "mallory@example.com");
    // Give any (unexpected) fetch a tick to resolve before asserting absence.
    await new Promise((r) => setTimeout(r, 0));
    expect(container.querySelector("section")).toBeNull();
    expect(healthMock).not.toHaveBeenCalled();
  });

  it("renders nothing for a stopped run, even when owned", async () => {
    healthMock.mockResolvedValue({ ssh: { enabled: true } });
    const { container } = renderCard({ state: "COMPLETED" });
    await new Promise((r) => setTimeout(r, 0));
    expect(container.querySelector("section")).toBeNull();
    expect(healthMock).not.toHaveBeenCalled();
  });

  // This card used to return NULL with SSH off — the compose default — so most
  // operators saw a live browser terminal and nothing anywhere saying a real
  // terminal could reach the same session. The CLI lane needs no gateway, so the
  // card now always renders for a running run you own, and says plainly that SSH
  // is the part that is off.
  it("with SSH disabled it still offers the CLI, and says what would turn SSH on", async () => {
    healthMock.mockResolvedValue({});
    listKeysMock.mockResolvedValue([]);
    renderCard();
    await waitFor(() => expect(healthMock).toHaveBeenCalled());
    expect(screen.getByText("Attach from your terminal")).toBeInTheDocument();
    // jsdom's origin is not the CLI's default, so the block also carries a
    // WARDYN_URL= prefix — match on the command within it.
    expect(
      screen.getByText((t) => t.includes(`wardyn attach ${baseRun.id}`)),
    ).toBeInTheDocument();
    // Both SSH and the UI-apps lane are off with an empty healthz response,
    // so match SSH's off text specifically rather than the shared "Off on
    // this deployment" prefix both lanes now share.
    expect(screen.getByText(/Off on this deployment\. It gives you/)).toBeInTheDocument();
    expect(screen.getByText(/WARDYN_SSH_LISTEN/)).toBeInTheDocument();
    // The ssh command itself must NOT appear — there is no gateway to reach.
    expect(screen.queryByText(/^ssh run_1@/)).toBeNull();
  });

  it("the CLI lane names the same live session the page's terminal is attached to", async () => {
    healthMock.mockResolvedValue({});
    listKeysMock.mockResolvedValue([]);
    renderCard();
    await waitFor(() => expect(healthMock).toHaveBeenCalled());
    expect(screen.getByText(/same session/)).toBeInTheDocument();
    expect(screen.getByText(/WARDYN_ADMIN_TOKEN/)).toBeInTheDocument();
  });
});

describe("ConnectSSHCard — content", () => {
  it("shows the full card (command, ssh config, fingerprint, VS Code disclosure) when a key is registered", async () => {
    healthMock.mockResolvedValue({
      ssh: { enabled: true, advertise_addr: "wardyn.corp.example:2222", host_key_fingerprint: "SHA256:abc123" },
    });
    listKeysMock.mockResolvedValue([
      { fingerprint: "SHA256:x", principal: OWNER, name: "laptop", public_key: "", created_at: "2026-01-01T00:00:00Z" },
    ]);
    renderCard();

    await screen.findByText("Attach from your terminal");
    expect(screen.getByText(`ssh ${baseRun.id}@wardyn.corp.example -p 2222`)).toBeInTheDocument();
    // C3.2b: the wardyn ssh <run-id> shortcut line sits above "ssh config".
    expect(screen.getByText((t) => t.includes(`wardyn ssh ${baseRun.id}`))).toBeInTheDocument();
    expect(screen.getByText(/ED25519 SHA256:abc123/)).toBeInTheDocument();
    expect(screen.getByText("ssh config")).toBeInTheDocument();
    expect(screen.getByText("VS Code Remote-SSH")).toBeInTheDocument();
    // No-keys lead-in must NOT show once a key exists.
    expect(screen.queryByText("Add your SSH key first")).toBeNull();
  });

  it("leads with 'Add your SSH key first' when the caller has no keys, but still shows the real command", async () => {
    healthMock.mockResolvedValue({ ssh: { enabled: true, advertise_addr: "wardyn.corp.example:2222" } });
    listKeysMock.mockResolvedValue([]);
    renderCard();

    await screen.findByText("Add your SSH key first");
    expect(screen.getByText(`ssh ${baseRun.id}@wardyn.corp.example -p 2222`)).toBeInTheDocument();
    // Two "Manage SSH keys" affordances render in this state (the lead-in's
    // own CTA + the card's standing footer link) — both must point at the
    // keys screen.
    const links = screen.getAllByRole("link", { name: /manage ssh keys/i });
    expect(links.length).toBeGreaterThanOrEqual(1);
    for (const link of links) {
      expect(link).toHaveAttribute("href", "/ssh-keys");
    }
  });

  it("does not claim 'no key is registered' when listKeys merely fails (transient error, not a confirmed empty list)", async () => {
    healthMock.mockResolvedValue({ ssh: { enabled: true, advertise_addr: "wardyn.corp.example:2222" } });
    listKeysMock.mockRejectedValue(new Error("network blip"));
    renderCard();

    await screen.findByText("Attach from your terminal");
    expect(screen.queryByText("Add your SSH key first")).toBeNull();
    expect(screen.queryByText(/no key is registered/i)).toBeNull();
  });

  it("omits the -p flag when advertise_addr carries no port", async () => {
    healthMock.mockResolvedValue({ ssh: { enabled: true, advertise_addr: "wardyn.corp.example" } });
    listKeysMock.mockResolvedValue([{ fingerprint: "SHA256:x", principal: OWNER, name: "k", public_key: "", created_at: "" }]);
    renderCard();

    await screen.findByText("Attach from your terminal");
    expect(screen.getByText(`ssh ${baseRun.id}@wardyn.corp.example`)).toBeInTheDocument();
  });

  it("splits a bracketed IPv6 advertise_addr into host + port, not a mangled fragment", async () => {
    healthMock.mockResolvedValue({ ssh: { enabled: true, advertise_addr: "[2001:db8::1]:2222" } });
    listKeysMock.mockResolvedValue([{ fingerprint: "SHA256:x", principal: OWNER, name: "k", public_key: "", created_at: "" }]);
    renderCard();

    await screen.findByText("Attach from your terminal");
    expect(screen.getByText(`ssh ${baseRun.id}@2001:db8::1 -p 2222`)).toBeInTheDocument();
  });

  it("treats a bare (unbracketed) IPv6 advertise_addr as host-only — no port to split off", async () => {
    healthMock.mockResolvedValue({ ssh: { enabled: true, advertise_addr: "2001:db8::1" } });
    listKeysMock.mockResolvedValue([{ fingerprint: "SHA256:x", principal: OWNER, name: "k", public_key: "", created_at: "" }]);
    renderCard();

    await screen.findByText("Attach from your terminal");
    expect(screen.getByText(`ssh ${baseRun.id}@2001:db8::1`)).toBeInTheDocument();
  });
});

// UI apps lane (docs/design/ui-sandboxes-prompt.md) — a third, independent
// sub-affordance under the SAME owner+running gate as SSH/CLI above.
describe("ConnectSSHCard — UI apps lane", () => {
  it("is hidden along with the whole card for a non-owner or a stopped run", async () => {
    healthMock.mockResolvedValue({ ui_sandbox: { enabled: true, enter_url_template: "http://ui.local/__wardyn/enter?run={run}&app={app}&ticket={ticket}" } });
    listKeysMock.mockResolvedValue([]);
    const nonOwner = renderCard({ ui_apps: [{ name: "vscode", port: 8080 }] }, "mallory@example.com");
    await new Promise((r) => setTimeout(r, 0));
    expect(nonOwner.container.querySelector("section")).toBeNull();

    const stopped = renderCard({ state: "COMPLETED", ui_apps: [{ name: "vscode", port: 8080 }] });
    await new Promise((r) => setTimeout(r, 0));
    expect(stopped.container.querySelector("section")).toBeNull();
    expect(healthMock).not.toHaveBeenCalled();
  });

  it("off-state names WARDYN_UI_SANDBOX_LISTEN, byte-matching the frozen mock string", async () => {
    healthMock.mockResolvedValue({});
    listKeysMock.mockResolvedValue([]);
    renderCard();
    await waitFor(() => expect(healthMock).toHaveBeenCalled());
    // The env var renders in <Mono> (mock visual rule §6), so the frozen
    // string spans several nodes — the paragraph's text content still has to
    // byte-match the canonical table.
    const off = screen.getByText(/Off on this deployment\. It relays/);
    expect(off.textContent).toBe(UI_APPS_LANE.off);
    expect(screen.getByText("WARDYN_UI_SANDBOX_LISTEN")).toBeInTheDocument();
    // No CTA when the gateway is off.
    expect(screen.queryByRole("button", { name: /^Open /i })).toBeNull();
  });

  it("names the policy field when enabled but the run declares no apps", async () => {
    healthMock.mockResolvedValue({ ui_sandbox: { enabled: true, enter_url_template: "http://ui.local/__wardyn/enter?run={run}&app={app}&ticket={ticket}" } });
    listKeysMock.mockResolvedValue([]);
    renderCard({ ui_apps: [] });
    await waitFor(() => expect(healthMock).toHaveBeenCalled());
    const noApps = screen.getByText(/On for this deployment/);
    expect(noApps.textContent).toBe(UI_APPS_LANE.noApps);
    expect(screen.getByText("ui_apps")).toBeInTheDocument(); // §6: policy field in Mono
    expect(screen.queryByRole("button", { name: /^Open /i })).toBeNull();
  });

  it("renders one row per declared app, byte-matching the frozen intro/new-tab/no-recording strings", async () => {
    healthMock.mockResolvedValue({ ui_sandbox: { enabled: true, enter_url_template: "http://ui.local/__wardyn/enter?run={run}&app={app}&ticket={ticket}" } });
    listKeysMock.mockResolvedValue([]);
    renderCard({ ui_apps: [{ name: "vscode", port: 8080 }, { name: "docs", port: 3000, path: "/readme" }] });
    await waitFor(() => expect(healthMock).toHaveBeenCalled());

    expect(screen.getByText(UI_APPS_LANE.title)).toBeInTheDocument();
    expect(screen.getByText(UI_APPS_LANE.intro)).toBeInTheDocument();
    expect(screen.getByText("vscode")).toBeInTheDocument();
    expect(screen.getByText("localhost:8080/")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: UI_APPS_LANE.cta("vscode") })).toBeInTheDocument();
    expect(screen.getByText("docs")).toBeInTheDocument();
    expect(screen.getByText("localhost:3000/readme")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: UI_APPS_LANE.cta("docs") })).toBeInTheDocument();
    expect(screen.getByText(UI_APPS_LANE.newTab)).toBeInTheDocument();
    expect(screen.getByText(UI_APPS_LANE.noRecording)).toBeInTheDocument();
  });

  it("mints a ticket and opens the app on the UI-sandbox origin, substituting run/app/ticket", async () => {
    healthMock.mockResolvedValue({ ui_sandbox: { enabled: true, enter_url_template: "http://ui.local/__wardyn/enter?run={run}&app={app}&ticket={ticket}" } });
    listKeysMock.mockResolvedValue([]);
    attachTicketMock.mockResolvedValue("tkt_abc123");
    const openSpy = vi.spyOn(window, "open").mockReturnValue(null);
    renderCard({ ui_apps: [{ name: "vscode", port: 8080 }] });
    await waitFor(() => expect(healthMock).toHaveBeenCalled());

    screen.getByRole("button", { name: UI_APPS_LANE.cta("vscode") }).click();
    await waitFor(() => expect(attachTicketMock).toHaveBeenCalledWith(baseRun.id));
    await waitFor(() =>
      expect(openSpy).toHaveBeenCalledWith(
        `http://ui.local/__wardyn/enter?run=${baseRun.id}&app=vscode&ticket=tkt_abc123`,
        "_blank",
        "noopener",
      ),
    );
    // "noopener" makes window.open return null even when the tab DID open, so
    // the old `!win` branch showed the popup-blocked error on every success.
    await screen.findByRole("button", { name: UI_APPS_LANE.cta("vscode") });
    expect(screen.queryByText(UI_APPS_LANE.errorTitle("vscode"))).toBeNull();
    openSpy.mockRestore();
  });

  it("substitutes EVERY placeholder, including the {run} host-mode templates carry twice", async () => {
    healthMock.mockResolvedValue({
      ui_sandbox: {
        enabled: true,
        // WARDYN_UI_SANDBOX_ORIGIN_TEMPLATE (host mode) — {run} in the host AND
        // the query. A single String.replace fills only the host, leaving
        // `?run={run}` literal for uuid.Parse to reject on the server.
        enter_url_template:
          "https://run-{run}.ui.example.com/__wardyn/enter?run={run}&app={app}&ticket={ticket}",
      },
    });
    listKeysMock.mockResolvedValue([]);
    attachTicketMock.mockResolvedValue("tkt_abc123");
    const openSpy = vi.spyOn(window, "open").mockReturnValue(null);
    renderCard({ ui_apps: [{ name: "vscode", port: 8080 }] });
    await waitFor(() => expect(healthMock).toHaveBeenCalled());

    screen.getByRole("button", { name: UI_APPS_LANE.cta("vscode") }).click();
    await waitFor(() =>
      expect(openSpy).toHaveBeenCalledWith(
        `https://run-${baseRun.id}.ui.example.com/__wardyn/enter?run=${baseRun.id}&app=vscode&ticket=tkt_abc123`,
        "_blank",
        "noopener",
      ),
    );
    expect(openSpy.mock.calls[0][0]).not.toContain("{run}");
    openSpy.mockRestore();
  });

  it("renders lane.error.launcher above the server's verbatim body when the ticket mint fails with that message, and leaves the other app untouched", async () => {
    healthMock.mockResolvedValue({ ui_sandbox: { enabled: true, enter_url_template: "http://ui.local/__wardyn/enter?run={run}&app={app}&ticket={ticket}" } });
    listKeysMock.mockResolvedValue([]);
    attachTicketMock.mockRejectedValue(
      new Error("no UI launcher in this image: /usr/local/bin/wardyn-ui-vscode not found"),
    );
    renderCard({ ui_apps: [{ name: "vscode", port: 8080 }, { name: "docs", port: 3000 }] });
    await waitFor(() => expect(healthMock).toHaveBeenCalled());

    screen.getByRole("button", { name: UI_APPS_LANE.cta("vscode") }).click();
    await screen.findByText(UI_APPS_LANE.errorTitle("vscode"));
    const launcher = screen.getByText(/This image has no/);
    expect(launcher.textContent).toBe(UI_APPS_LANE.errorLauncher("vscode"));
    // §6: the launcher path and the image directory render in Mono.
    expect(screen.getByText("/usr/local/bin/wardyn-ui-vscode")).toBeInTheDocument();
    expect(screen.getByText("deploy/images/vscode/")).toBeInTheDocument();
    expect(
      screen.getByText("no UI launcher in this image: /usr/local/bin/wardyn-ui-vscode not found"),
    ).toBeInTheDocument();
    // The other row's button is untouched — still its normal CTA, not busy.
    expect(screen.getByRole("button", { name: UI_APPS_LANE.cta("docs") })).toBeInTheDocument();
  });

  it("renders the UI apps lane LAST — after the ssh command, per the mock's S3 order", async () => {
    healthMock.mockResolvedValue({
      ssh: { enabled: true, advertise_addr: "wardyn.corp.example:2222" },
      ui_sandbox: { enabled: true, enter_url_template: "http://ui.local/__wardyn/enter?run={run}&app={app}&ticket={ticket}" },
    });
    listKeysMock.mockResolvedValue([
      { fingerprint: "SHA256:x", principal: OWNER, name: "k", public_key: "", created_at: "" },
    ]);
    renderCard({ ui_apps: [{ name: "vscode", port: 8080 }] });
    const uiHeading = await screen.findByText(UI_APPS_LANE.title);
    const sshCommand = screen.getByText(`ssh ${baseRun.id}@wardyn.corp.example -p 2222`);

    // The lane was originally inserted right after the SSH *heading*, which put
    // it AHEAD of the ssh command the heading introduces.
    expect(sshCommand.compareDocumentPosition(uiHeading) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });
});
