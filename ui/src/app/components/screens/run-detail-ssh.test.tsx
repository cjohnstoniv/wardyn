/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// ConnectSSHCard (prompt-v3-ssh-pane.md): owner-only, running-only,
// gateway-enabled-only visibility, and the no-keys-yet lead-in. Standalone
// (ConnectSSHCard is exported for exactly this) rather than mounting the
// whole RunDetailScreen's fetch graph. Moved out of run-detail.test.tsx
// alongside the component (B4 file-size gate) — zero behavior change.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { RunDetail } from "../../lib/types";

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
import { aheadByHours } from "../../lib/test-clock";

const OWNER = "alice@example.com";
const baseRun: RunDetail = {
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
  interactive: false,
};

function renderCard(run: Partial<RunDetail> = {}, principal = OWNER, operator = true) {
  return render(
    <MemoryRouter>
      <OperatorProvider operator={operator} principal={principal}>
        <ConnectSSHCard run={{ ...baseRun, ...run }} />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

// Every health() fixture below carries status:"ok" because a real /healthz body
// always does (internal/api/healthz.go:61) — and the card reads exactly that
// bit to tell "the daemon answered, both gateways are off" apart from "no
// answer at all", which lib/api/health.ts reports as the same resolved `{}`.
beforeEach(() => {
  healthMock.mockReset();
  listKeysMock.mockReset();
  attachTicketMock.mockReset();
});

describe("ConnectSSHCard — visibility", () => {
  it("renders nothing for a MEMBER on a run they do not own", async () => {
    healthMock.mockResolvedValue({ status: "ok", ssh: { enabled: true, advertise_addr: "wardyn.corp.example:2222" } });
    listKeysMock.mockResolvedValue([{ fingerprint: "SHA256:x", principal: OWNER, name: "k", public_key: "", created_at: "" }]);
    const { container } = renderCard({}, "mallory@example.com", false);
    // Give any (unexpected) fetch a tick to resolve before asserting absence.
    await new Promise((r) => setTimeout(r, 0));
    expect(container.querySelector("section")).toBeNull();
    expect(healthMock).not.toHaveBeenCalled();
  });

  // The server's three lanes are owner-OR-admin (attach_ticket.go's isOperator,
  // uigateway.go's role check, sshgateway.go's admin arm). Hiding the card from
  // an admin offered less than the API already serves them.
  it("renders for an ADMIN on a run they do not own", async () => {
    healthMock.mockResolvedValue({ status: "ok", ssh: { enabled: true, advertise_addr: "wardyn.corp.example:2222" } });
    listKeysMock.mockResolvedValue([{ fingerprint: "SHA256:x", principal: "admin@example.com", name: "k", public_key: "", created_at: "" }]);
    const { container } = renderCard({}, "admin@example.com", true);
    await waitFor(() => expect(container.querySelector("section")).not.toBeNull());
    expect(healthMock).toHaveBeenCalled();
  });

  it("renders nothing for a stopped run, even when owned", async () => {
    healthMock.mockResolvedValue({ status: "ok", ssh: { enabled: true } });
    const { container } = renderCard({ state: "COMPLETED" });
    await new Promise((r) => setTimeout(r, 0));
    expect(container.querySelector("section")).toBeNull();
    expect(healthMock).not.toHaveBeenCalled();
  });

  // The CLI lane needs no gateway, so the card always renders for a running
  // run you own — SSH being off hides only the ssh command, never the whole
  // card — and it says plainly that SSH is the part that is off.
  it("with SSH disabled it still offers the CLI, and says what would turn SSH on", async () => {
    healthMock.mockResolvedValue({ status: "ok" });
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
    healthMock.mockResolvedValue({ status: "ok" });
    listKeysMock.mockResolvedValue([]);
    renderCard();
    await waitFor(() => expect(healthMock).toHaveBeenCalled());
    expect(screen.getByText(/same session/)).toBeInTheDocument();
    expect(screen.getByText(/WARDYN_ADMIN_TOKEN/)).toBeInTheDocument();
  });
});

// ponytail: "external:" is a bare description-string prefix (run-detail-ssh.tsx),
// not a typed field — these two cases are its whole contract.
describe("ConnectSSHCard — external-tool notice", () => {
  it("renders the external-tool line when description starts with 'external:'", async () => {
    healthMock.mockResolvedValue({ status: "ok" });
    listKeysMock.mockResolvedValue([]);
    renderCard({ description: "external:my-tool" });
    await waitFor(() => expect(healthMock).toHaveBeenCalled());
    expect(
      screen.getByText("Managed by an external tool — killing this run tears down that tool's workspace."),
    ).toBeInTheDocument();
  });

  it("omits the line when description is absent or does not start with 'external:'", async () => {
    healthMock.mockResolvedValue({ status: "ok" });
    listKeysMock.mockResolvedValue([]);
    renderCard({ description: "a run I described myself" });
    await waitFor(() => expect(healthMock).toHaveBeenCalled());
    expect(screen.queryByText(/Managed by an external tool/)).toBeNull();
  });
});

describe("ConnectSSHCard — content", () => {
  it("shows the full card (command, ssh config, fingerprint, VS Code disclosure) when a key is registered", async () => {
    healthMock.mockResolvedValue({
      status: "ok",
      ssh: { enabled: true, advertise_addr: "wardyn.corp.example:2222", host_key_fingerprint: "SHA256:abc123" },
    });
    listKeysMock.mockResolvedValue([
      { fingerprint: "SHA256:x", principal: OWNER, name: "laptop", public_key: "", created_at: aheadByHours(-1) },
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
    healthMock.mockResolvedValue({ status: "ok", ssh: { enabled: true, advertise_addr: "wardyn.corp.example:2222" } });
    listKeysMock.mockResolvedValue([]);
    renderCard();

    await screen.findByText("Add your SSH key first");
    expect(screen.getByText(`ssh ${baseRun.id}@wardyn.corp.example -p 2222`)).toBeInTheDocument();
    // Two "Manage SSH keys" affordances render in this state (the lead-in's
    // own CTA + the card's standing footer link) — both must point at Your
    // account (M-5, #636: /ssh-keys is gone, no alias).
    const links = screen.getAllByRole("link", { name: /manage ssh keys/i });
    expect(links.length).toBeGreaterThanOrEqual(1);
    for (const link of links) {
      expect(link).toHaveAttribute("href", "/account");
    }
  });

  it("does not claim 'no key is registered' when listKeys merely fails (transient error, not a confirmed empty list)", async () => {
    healthMock.mockResolvedValue({ status: "ok", ssh: { enabled: true, advertise_addr: "wardyn.corp.example:2222" } });
    listKeysMock.mockRejectedValue(new Error("network blip"));
    renderCard();

    await screen.findByText("Attach from your terminal");
    expect(screen.queryByText("Add your SSH key first")).toBeNull();
    expect(screen.queryByText(/no key is registered/i)).toBeNull();
  });

  it("omits the -p flag when advertise_addr carries no port", async () => {
    healthMock.mockResolvedValue({ status: "ok", ssh: { enabled: true, advertise_addr: "wardyn.corp.example" } });
    listKeysMock.mockResolvedValue([{ fingerprint: "SHA256:x", principal: OWNER, name: "k", public_key: "", created_at: "" }]);
    renderCard();

    await screen.findByText("Attach from your terminal");
    expect(screen.getByText(`ssh ${baseRun.id}@wardyn.corp.example`)).toBeInTheDocument();
  });

  it("splits a bracketed IPv6 advertise_addr into host + port, not a mangled fragment", async () => {
    healthMock.mockResolvedValue({ status: "ok", ssh: { enabled: true, advertise_addr: "[2001:db8::1]:2222" } });
    listKeysMock.mockResolvedValue([{ fingerprint: "SHA256:x", principal: OWNER, name: "k", public_key: "", created_at: "" }]);
    renderCard();

    await screen.findByText("Attach from your terminal");
    expect(screen.getByText(`ssh ${baseRun.id}@2001:db8::1 -p 2222`)).toBeInTheDocument();
  });

  it("treats a bare (unbracketed) IPv6 advertise_addr as host-only — no port to split off", async () => {
    healthMock.mockResolvedValue({ status: "ok", ssh: { enabled: true, advertise_addr: "2001:db8::1" } });
    listKeysMock.mockResolvedValue([{ fingerprint: "SHA256:x", principal: OWNER, name: "k", public_key: "", created_at: "" }]);
    renderCard();

    await screen.findByText("Attach from your terminal");
    expect(screen.getByText(`ssh ${baseRun.id}@2001:db8::1`)).toBeInTheDocument();
  });
});

// UI apps lane (docs/design/ui-sandboxes-prompt.md) — a third, independent
// sub-affordance under the SAME owner+running gate as SSH/CLI above.
describe("ConnectSSHCard — UI apps lane", () => {
  it("is hidden along with the whole card for a non-owner member or a stopped run", async () => {
    healthMock.mockResolvedValue({ status: "ok", ui_sandbox: { enabled: true, enter_url_template: "http://ui.local/__wardyn/enter?run={run}&app={app}&ticket={ticket}" } });
    listKeysMock.mockResolvedValue([]);
    const nonOwner = renderCard({ ui_apps: [{ name: "vscode", port: 8080 }] }, "mallory@example.com", false);
    await new Promise((r) => setTimeout(r, 0));
    expect(nonOwner.container.querySelector("section")).toBeNull();

    const stopped = renderCard({ state: "COMPLETED", ui_apps: [{ name: "vscode", port: 8080 }] });
    await new Promise((r) => setTimeout(r, 0));
    expect(stopped.container.querySelector("section")).toBeNull();
    expect(healthMock).not.toHaveBeenCalled();
  });

  it("off-state names WARDYN_UI_SANDBOX_LISTEN, byte-matching the frozen mock string", async () => {
    healthMock.mockResolvedValue({ status: "ok" });
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

  // mock M6: the off-state's one affordance is a pointer to the page that says
  // how to turn it on, placed next to the need rather than in a footer (§9).
  it("off-state carries the doc pointer, naming the page — not a dead link and not a button", async () => {
    healthMock.mockResolvedValue({ status: "ok" });
    listKeysMock.mockResolvedValue([]);
    renderCard();
    await waitFor(() => expect(healthMock).toHaveBeenCalled());

    expect(screen.getByText(UI_APPS_LANE.offDoc)).toBeInTheDocument();
    // The file is named, because the console does not serve docs/ — an <a href>
    // here would 404, and a button would pretend a viewer can set a server env
    // var. Same pattern as policy-panel.tsx's docs/POLICIES.md reference.
    expect(screen.getByText(UI_APPS_LANE.offDocPath)).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: /UI sandboxes/i })).toBeNull();
    expect(screen.queryByRole("button", { name: /UI sandboxes/i })).toBeNull();
    // …and it must not be PAINTED as one either: --info is the text-link colour
    // (§2), so wearing it on a non-link is a false affordance.
    expect(screen.getByText(UI_APPS_LANE.offDoc).closest("p")).toHaveClass("text-muted-foreground");
  });

  it("the doc pointer belongs to the off-state only — an enabled deployment does not show it", async () => {
    healthMock.mockResolvedValue({
      status: "ok",
      ui_sandbox: { enabled: true, enter_url_template: "http://ui.local/__wardyn/enter?run={run}&app={app}&ticket={ticket}" },
    });
    listKeysMock.mockResolvedValue([]);
    renderCard({ ui_apps: [{ name: "vscode", port: 8080 }] });
    await waitFor(() => expect(healthMock).toHaveBeenCalled());
    expect(screen.queryByText(UI_APPS_LANE.offDoc)).toBeNull();
  });

  it("names the policy field when enabled but the run declares no apps", async () => {
    healthMock.mockResolvedValue({ status: "ok", ui_sandbox: { enabled: true, enter_url_template: "http://ui.local/__wardyn/enter?run={run}&app={app}&ticket={ticket}" } });
    listKeysMock.mockResolvedValue([]);
    renderCard({ ui_apps: [] });
    await waitFor(() => expect(healthMock).toHaveBeenCalled());
    const noApps = screen.getByText(/On for this deployment/);
    expect(noApps.textContent).toBe(UI_APPS_LANE.noApps);
    expect(screen.getByText("ui_apps")).toBeInTheDocument(); // §6: policy field in Mono
    expect(screen.queryByRole("button", { name: /^Open /i })).toBeNull();
  });

  it("renders one row per declared app, byte-matching the frozen intro/new-tab/no-recording strings", async () => {
    healthMock.mockResolvedValue({ status: "ok", ui_sandbox: { enabled: true, enter_url_template: "http://ui.local/__wardyn/enter?run={run}&app={app}&ticket={ticket}" } });
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

  // #1220: the Open button POSTs the ticket via a hidden auto-submitted form
  // instead of putting it in a window.open URL — the whole point being that
  // the ticket never lands in a URL, browser history, or an access log.
  it("mints a ticket and POSTs it via a hidden form, never a URL", async () => {
    healthMock.mockResolvedValue({
      status: "ok",
      ui_sandbox: { enabled: true, enter_post_url: "http://ui.local/__wardyn/enter" },
    });
    listKeysMock.mockResolvedValue([]);
    attachTicketMock.mockResolvedValue("tkt_abc123");
    const openSpy = vi.spyOn(window, "open");
    const submitSpy = vi.spyOn(HTMLFormElement.prototype, "submit").mockImplementation(() => {});
    renderCard({ ui_apps: [{ name: "vscode", port: 8080 }] });
    await waitFor(() => expect(healthMock).toHaveBeenCalled());

    screen.getByRole("button", { name: UI_APPS_LANE.cta("vscode") }).click();
    await waitFor(() => expect(attachTicketMock).toHaveBeenCalledWith(baseRun.id));
    await waitFor(() => expect(submitSpy).toHaveBeenCalled());

    const form = submitSpy.mock.instances[0] as HTMLFormElement;
    expect(form.method).toBe("post");
    expect(form.action).toBe("http://ui.local/__wardyn/enter");
    expect(form.target).toBe("_blank");
    const values = Object.fromEntries(new FormData(form).entries());
    expect(values).toEqual({ run: baseRun.id, app: "vscode", ticket: "tkt_abc123" });
    // The form is submitted, not window.open'd — no URL ever carries the
    // ticket for this lane.
    expect(openSpy).not.toHaveBeenCalled();

    await screen.findByRole("button", { name: UI_APPS_LANE.cta("vscode") });
    expect(screen.queryByText(UI_APPS_LANE.errorTitle("vscode"))).toBeNull();
    submitSpy.mockRestore();
    openSpy.mockRestore();
  });

  it("substitutes {run} in the POST action for a host-mode template, and still never puts it in a URL", async () => {
    healthMock.mockResolvedValue({
      status: "ok",
      // WARDYN_UI_SANDBOX_ORIGIN_TEMPLATE (host mode) puts {run} in the HOST;
      // enter_post_url carries no query string at all.
      ui_sandbox: { enabled: true, enter_post_url: "https://run-{run}.ui.example.com/__wardyn/enter" },
    });
    listKeysMock.mockResolvedValue([]);
    attachTicketMock.mockResolvedValue("tkt_abc123");
    const submitSpy = vi.spyOn(HTMLFormElement.prototype, "submit").mockImplementation(() => {});
    renderCard({ ui_apps: [{ name: "vscode", port: 8080 }] });
    await waitFor(() => expect(healthMock).toHaveBeenCalled());

    screen.getByRole("button", { name: UI_APPS_LANE.cta("vscode") }).click();
    await waitFor(() => expect(submitSpy).toHaveBeenCalled());
    const form = submitSpy.mock.instances[0] as HTMLFormElement;
    expect(form.action).toBe(`https://run-${baseRun.id}.ui.example.com/__wardyn/enter`);
    expect(form.action).not.toContain("{run}");
    submitSpy.mockRestore();
  });

  // #1241: before the form POST, the console binds the ticket to this browser
  // with one credentialed fetch to the gateway's bind_url. The gateway refuses
  // an enter whose ticket was not bound, so a ticket minted in someone else's
  // browser cannot be pushed into this one.
  it("binds the ticket on the gateway with a credentialed fetch before submitting the enter form", async () => {
    healthMock.mockResolvedValue({
      status: "ok",
      ui_sandbox: {
        enabled: true,
        enter_post_url: "https://run-{run}.ui.example.com/__wardyn/enter",
        bind_url: "https://run-{run}.ui.example.com/__wardyn/bind",
      },
    });
    listKeysMock.mockResolvedValue([]);
    attachTicketMock.mockResolvedValue("tkt_abc123");
    const order: string[] = [];
    const fetchMock = vi.fn(async () => {
      order.push("bind");
      return new Response(null, { status: 204 });
    });
    vi.stubGlobal("fetch", fetchMock);
    const submitSpy = vi.spyOn(HTMLFormElement.prototype, "submit").mockImplementation(() => {
      order.push("submit");
    });
    try {
      renderCard({ ui_apps: [{ name: "vscode", port: 8080 }] });
      await waitFor(() => expect(healthMock).toHaveBeenCalled());

      screen.getByRole("button", { name: UI_APPS_LANE.cta("vscode") }).click();
      await waitFor(() => expect(submitSpy).toHaveBeenCalled());
      expect(order).toEqual(["bind", "submit"]);
      const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
      expect(url).toBe(`https://run-${baseRun.id}.ui.example.com/__wardyn/bind`);
      expect(init.method).toBe("POST");
      expect(init.credentials).toBe("include");
      // The ticket rides the body, never the URL, and nothing of the
      // console's own session travels to the gateway.
      expect(String(init.body)).toBe("ticket=tkt_abc123");
      expect(init.headers).toBeUndefined();
    } finally {
      submitSpy.mockRestore();
      vi.unstubAllGlobals();
    }
  });

  it("shows an error and never submits the enter form when the gateway refuses the bind", async () => {
    healthMock.mockResolvedValue({
      status: "ok",
      ui_sandbox: {
        enabled: true,
        enter_post_url: "http://ui.local/__wardyn/enter",
        bind_url: "http://ui.local/__wardyn/bind",
      },
    });
    listKeysMock.mockResolvedValue([]);
    attachTicketMock.mockResolvedValue("tkt_abc123");
    // A refused bind carries no CORS headers, so the browser rejects the fetch.
    vi.stubGlobal("fetch", vi.fn(async () => { throw new TypeError("Failed to fetch"); }));
    const submitSpy = vi.spyOn(HTMLFormElement.prototype, "submit").mockImplementation(() => {});
    try {
      renderCard({ ui_apps: [{ name: "vscode", port: 8080 }] });
      await waitFor(() => expect(healthMock).toHaveBeenCalled());

      screen.getByRole("button", { name: UI_APPS_LANE.cta("vscode") }).click();
      await screen.findByText(UI_APPS_LANE.errorTitle("vscode"));
      expect(screen.getByText(/must be served from the same site/)).toBeInTheDocument();
      expect(submitSpy).not.toHaveBeenCalled();
    } finally {
      submitSpy.mockRestore();
      vi.unstubAllGlobals();
    }
  });

  // Review F4: an older daemon than this console (dev-setup skew only — the
  // console is normally baked into the daemon serving it) publishes
  // enter_url_template but not enter_post_url. Submitting a form with an
  // empty action would silently POST run/app/ticket to the CONSOLE's own
  // current URL, burning the ticket for nothing — the console must fall back
  // to the GET template that daemon actually published instead.
  it("falls back to the GET template (window.open) when enter_post_url is absent but enter_url_template is present", async () => {
    healthMock.mockResolvedValue({
      status: "ok",
      ui_sandbox: {
        enabled: true,
        enter_url_template: "http://ui.local/__wardyn/enter?run={run}&app={app}&ticket={ticket}",
      },
    });
    listKeysMock.mockResolvedValue([]);
    attachTicketMock.mockResolvedValue("tkt_abc123");
    const openSpy = vi.spyOn(window, "open").mockReturnValue(null);
    const submitSpy = vi.spyOn(HTMLFormElement.prototype, "submit").mockImplementation(() => {});
    renderCard({ ui_apps: [{ name: "vscode", port: 8080 }] });
    await waitFor(() => expect(healthMock).toHaveBeenCalled());

    screen.getByRole("button", { name: UI_APPS_LANE.cta("vscode") }).click();
    await waitFor(() =>
      expect(openSpy).toHaveBeenCalledWith(
        `http://ui.local/__wardyn/enter?run=${baseRun.id}&app=vscode&ticket=tkt_abc123`,
        "_blank",
        "noopener",
      ),
    );
    // Never a form POST to the console's own (empty-action) URL.
    expect(submitSpy).not.toHaveBeenCalled();
    expect(screen.queryByText(UI_APPS_LANE.errorTitle("vscode"))).toBeNull();
    submitSpy.mockRestore();
    openSpy.mockRestore();
  });

  it("shows an error rather than POSTing to its own URL when neither enter_post_url nor enter_url_template is published", async () => {
    healthMock.mockResolvedValue({ status: "ok", ui_sandbox: { enabled: true } });
    listKeysMock.mockResolvedValue([]);
    attachTicketMock.mockResolvedValue("tkt_abc123");
    const openSpy = vi.spyOn(window, "open");
    const submitSpy = vi.spyOn(HTMLFormElement.prototype, "submit").mockImplementation(() => {});
    renderCard({ ui_apps: [{ name: "vscode", port: 8080 }] });
    await waitFor(() => expect(healthMock).toHaveBeenCalled());

    screen.getByRole("button", { name: UI_APPS_LANE.cta("vscode") }).click();
    await screen.findByText(UI_APPS_LANE.errorTitle("vscode"));
    expect(openSpy).not.toHaveBeenCalled();
    expect(submitSpy).not.toHaveBeenCalled();
    submitSpy.mockRestore();
    openSpy.mockRestore();
  });

  it("renders lane.error.launcher above the server's verbatim body when the ticket mint fails with that message, and leaves the other app untouched", async () => {
    healthMock.mockResolvedValue({ status: "ok", ui_sandbox: { enabled: true, enter_url_template: "http://ui.local/__wardyn/enter?run={run}&app={app}&ticket={ticket}" } });
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
      status: "ok",
      ssh: { enabled: true, advertise_addr: "wardyn.corp.example:2222" },
      ui_sandbox: { enabled: true, enter_url_template: "http://ui.local/__wardyn/enter?run={run}&app={app}&ticket={ticket}" },
    });
    listKeysMock.mockResolvedValue([
      { fingerprint: "SHA256:x", principal: OWNER, name: "k", public_key: "", created_at: "" },
    ]);
    renderCard({ ui_apps: [{ name: "vscode", port: 8080 }] });
    const uiHeading = await screen.findByText(UI_APPS_LANE.title);
    const sshCommand = screen.getByText(`ssh ${baseRun.id}@wardyn.corp.example -p 2222`);

    // The lane must follow the ssh command itself, not just the SSH heading
    // that introduces it.
    expect(sshCommand.compareDocumentPosition(uiHeading) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });
});

// The card's OFF copy is a claim about the DEPLOYMENT ("Off on this
// deployment... an operator turns it on by setting WARDYN_SSH_LISTEN"), so it
// must never render on a /healthz that never answered. The failure mode to
// guard is NOT a rejected promise — lib/api/health.ts swallows every non-ok
// response and every network/parse/abort error into a resolved `{}` — so
// these cases drive the REAL health() through a stubbed fetch, which is the
// shape a live 5xx or blip actually produces. `status:"ok"` (healthz.go:61,
// present in every successful body) is the only thing that separates
// "answered: both off" from "no answer".
describe("ConnectSSHCard — a FAILED /healthz asserts nothing about the deployment", () => {
  afterEach(() => vi.unstubAllGlobals());

  async function useRealHealth() {
    const actual = await vi.importActual<typeof import("../../lib/api/health")>(
      "../../lib/api/health",
    );
    healthMock.mockImplementation(() => actual.health.health());
  }

  function haveKeys() {
    listKeysMock.mockResolvedValue([
      { fingerprint: "SHA256:x", principal: OWNER, name: "k", public_key: "", created_at: "" },
    ]);
  }

  // Every way /healthz can fail to answer, as health() actually reports it.
  const failures: [string, () => unknown][] = [
    ["a 503 from the daemon", () => ({ ok: false, status: 503, json: async () => ({}) })],
    ["a refused connection", () => { throw new TypeError("Failed to fetch"); }],
  ];

  for (const [label, respond] of failures) {
    it(`shows neither lane's OFF copy on ${label}`, async () => {
      await useRealHealth();
      vi.stubGlobal("fetch", vi.fn(async () => respond() as Response));
      haveKeys();
      const { container } = renderCard();
      // The card itself still renders — the CLI lane needs no gateway at all.
      await waitFor(() => expect(container.querySelector("section")).not.toBeNull());
      await waitFor(() => expect(healthMock).toHaveBeenCalled());
      // Let the swallowed failure settle before asserting absence.
      await new Promise((r) => setTimeout(r, 0));

      // queryAll, not query: with the defect BOTH lanes claim it, and a
      // multiple-match throw would hide which.
      expect(screen.queryAllByText(/Off on this deployment/)).toHaveLength(0);
      expect(screen.queryAllByText(new RegExp(UI_APPS_LANE.off.slice(0, 40)))).toHaveLength(0);
      // Both headings stay: the lanes exist, we just have no answer about them.
      expect(screen.getByText("SSH")).toBeInTheDocument();
      expect(screen.getByText(UI_APPS_LANE.title)).toBeInTheDocument();
    });
  }

  // The other direction, through the same real code path: a daemon that DOES
  // answer, with neither gateway configured, must still say so.
  it("says OFF for both lanes when a real /healthz answers with neither gateway", async () => {
    await useRealHealth();
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => ({ ok: true, status: 200, json: async () => ({ status: "ok" }) }) as unknown as Response),
    );
    haveKeys();
    renderCard();
    // Both lanes' OFF paragraphs open with the same sentence (copy.ts:470).
    await waitFor(() => expect(screen.getAllByText(/Off on this deployment/).length).toBe(2));
    expect(screen.getByText(new RegExp(UI_APPS_LANE.off.slice(0, 40)))).toBeInTheDocument();
  });

  // And with the module mocked, the same answer shape — a body that carries
  // status but neither gateway key — so the pin cannot be satisfied by an
  // unreachable daemon's `{}`.
  it("says OFF when health() ANSWERS status:ok with neither lane present", async () => {
    healthMock.mockResolvedValue({ status: "ok" });
    haveKeys();
    renderCard();
    await waitFor(() => expect(screen.getAllByText(/Off on this deployment/).length).toBe(2));
    expect(screen.getByText(new RegExp(UI_APPS_LANE.off.slice(0, 40)))).toBeInTheDocument();
  });
});
