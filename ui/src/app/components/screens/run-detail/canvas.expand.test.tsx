/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #3940: a run page tile expands to a large view that holds the SAME live widget, moved there. These pin what must not
// change when it moves: the instance (state, polling, focus-free work), what the viewer can see and do, and the keys.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

const getLayout = vi.fn();
const putLayout = vi.fn();
vi.mock("../../../lib/api/run-layout", () => ({
  runLayout: {
    getLayout: (...a: unknown[]) => getLayout(...a),
    putLayout: (...a: unknown[]) => putLayout(...a),
  },
}));
const getFiles = vi.hoisted(() => vi.fn());
vi.mock("../../../lib/api/runs", () => ({
  runs: {
    getPolicy: () => new Promise(() => {}),
    getFiles: (...a: unknown[]) => getFiles(...a),
    getResources: vi.fn().mockRejectedValue(new Error("no runner in this test")),
    getModelUsage: vi.fn().mockResolvedValue({ totals: { input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, cache_write_tokens: 0, reasoning_tokens: 0, calls: 0, uncounted_calls: 0, partial_calls: 0 }, by_model: [], opaque_tunnels: 0 }),
    attachTicket: vi.fn(),
    watchTicket: vi.fn(),
  },
}));
const healthMock = vi.hoisted(() => vi.fn());
vi.mock("../../../lib/api/health", () => ({ health: { health: (...a: unknown[]) => healthMock(...a) } }));
vi.mock("../../../lib/api/ssh-keys", () => ({
  sshKeys: { listKeys: vi.fn().mockResolvedValue([{ id: "k1", name: "laptop", fingerprint: "SHA256:x" }]) },
}));
vi.mock("../../../lib/api/desktop-sign-ins", () => ({
  desktopSignIns: {
    get: vi.fn().mockResolvedValue({ store: "run_disk", paused: false, can_pause: true, agent: "Claude Code" }),
    pause: vi.fn(),
    resume: vi.fn(),
  },
}));
const measured = vi.hoisted(() => ({ px: {} as Record<string, number> }));
vi.mock("./fit-layout", async (orig) => ({
  ...(await orig<typeof import("./fit-layout")>()),
  measureTile: (tile: HTMLElement) => measured.px[tile.dataset.widget ?? ""] ?? 0,
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), message: vi.fn(), error: vi.fn() } }));
const focusState = vi.hoisted(() => ({ on: false }));
vi.mock("../app-shell", () => ({ useFocusMode: () => ({ focus: focusState.on, setFocus: () => {} }) }));
const listApprovalsMock = vi.hoisted(() => vi.fn());
vi.mock("../../../lib/api/approvals", () => ({
  approvals: { listApprovals: (...a: unknown[]) => listApprovalsMock(...a), approve: vi.fn(), deny: vi.fn() },
}));

import { makeApproval } from "../../../../test/factories";
import { RUN_COCKPIT } from "../../wardyn/copy";
import { DESKTOP } from "../../wardyn/copy/desktop";
import { LiveApprovals } from "../../wardyn/live-approvals";
import { OperatorProvider } from "../../wardyn/operator-context";
import { WidgetCard } from "../../wardyn/primitives";
import { RunCanvas } from "./canvas";
import type { WidgetContext } from "./widget-registry";

const RUN = {
  id: "run-1",
  created_at: new Date().toISOString(),
  updated_at: new Date().toISOString(),
  created_by: "me",
  agent: "claude-code",
  repo: "acme/widgets",
  task: "audit the egress proxy",
  confinement_class: "CC2",
  state: "RUNNING",
  spiffe_id: "spiffe://wardyn.local/agent-run/run-1",
  runner_target: "docker",
  interactive: false,
};

function ctx(overrides: Partial<WidgetContext> = {}): WidgetContext {
  return {
    run: RUN as unknown as WidgetContext["run"],
    finished: false,
    principal: null,
    operator: false,
    view: "user",
    grants: [],
    egress: [],
    heldCount: 0,
    audit: [],
    onGoAudit: () => {},
    onGoPolicy: () => {},
    terminalPane: <div>the session</div>,
    ...overrides,
  };
}

const withLayout = (layout: { widget: string; x: number; y: number; w: number; h: number }[]) =>
  getLayout.mockResolvedValue({ preset: "live", layout, updated_at: new Date().toISOString() });
const TERMINAL = { widget: "terminal", x: 0, y: 0, w: 8, h: 12 };

const expandName = (label: string) => RUN_COCKPIT.expandWidget(label);
const control = (label: string) => screen.findByRole("button", { name: expandName(label) });
async function open(user: ReturnType<typeof userEvent.setup>, label: string) {
  await user.click(await control(label));
  return screen.findByRole("dialog", { name: label });
}

// The admin's own run, as canvas.test.tsx has it: the ssh and Desktop tiles read the principal themselves.
const adminCtx = (over: Partial<WidgetContext> = {}) =>
  ctx({
    operator: true,
    principal: "admin@example.com",
    run: { ...RUN, created_by: "svc", operator_owned: true, state: "RUNNING" } as WidgetContext["run"],
    ...over,
  });
const adminUI = (c: WidgetContext) => (
  <MemoryRouter>
    <OperatorProvider operator principal="admin@example.com">
      <RunCanvas ctx={c} />
    </OperatorProvider>
  </MemoryRouter>
);

beforeEach(() => {
  measured.px = {};
  focusState.on = false;
  getLayout.mockReset().mockResolvedValue({ preset: "live", layout: [] });
  putLayout.mockReset().mockResolvedValue({ preset: "live", layout: [] });
  getFiles.mockReset().mockResolvedValue({ vcs: "git", files: [{ path: "internal/a.go", status: "M", added: 1, deleted: 0 }], truncated: false });
  healthMock.mockReset().mockResolvedValue({ status: "ok", ssh: { enabled: true, advertise_addr: "ssh.example:2222" } });
  listApprovalsMock.mockReset().mockResolvedValue([]);
});
afterEach(() => vi.restoreAllMocks());

describe("RunCanvas — Expand", () => {
  it("is on every tile but the Terminal, named for the card, and says it opens a dialog", async () => {
    render(<RunCanvas ctx={ctx()} />);
    const files = await control("Files changed");
    expect(files).toHaveAttribute("title", expandName("Files changed"));
    expect(files).toHaveAttribute("aria-haspopup", "dialog");
    for (const label of ["Egress", "Sandbox", "Credentials", "Identity"]) {
      expect(screen.queryByRole("button", { name: expandName(label) }), label).not.toBeNull();
    }
    expect(within(screen.getByTestId("run-terminal-pane")).queryByRole("button", { name: /^Expand / })).toBeNull();
  });

  it("is not drawn by a card outside the canvas", () => {
    render(
      <WidgetCard title="Elsewhere">
        <p>body</p>
      </WidgetCard>,
    );
    expect(screen.queryByRole("button", { name: /^Expand / })).toBeNull();
  });

  it("opens the same live instance, not a second one: the node is the same and nothing is read again", async () => {
    const user = userEvent.setup();
    render(<RunCanvas ctx={ctx()} />);
    const heading = await screen.findByRole("heading", { name: "Files changed" });
    await waitFor(() => expect(getFiles).toHaveBeenCalledTimes(1));

    const dialog = await open(user, "Files changed");
    // The dialog's own (screen-reader) title and the card's heading, which is the node that was in the tile.
    expect(within(dialog).getAllByRole("heading", { name: "Files changed" })).toContain(heading);
    expect(heading.closest("[role=dialog]")).toBe(dialog);
    // It starts below the shell's top bar and banners, so the daemon banner stays above it.
    expect(dialog).toHaveClass("top-[calc(var(--shell-banner-h,3.5rem)_+_0.5rem)]", "bottom-auto");

    await user.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(screen.getByRole("heading", { name: "Files changed" })).toBe(heading);
    expect(heading.closest("[data-widget]")).toHaveAttribute("data-widget", "files");
    expect(getFiles).toHaveBeenCalledTimes(1);
  });

  it("keeps an opened <details> open through opening and closing", async () => {
    const user = userEvent.setup();
    render(<RunCanvas ctx={ctx({ grants: [{ id: "g1", scope: "repo:acme/widgets", audience: "github", state: "active" }] as WidgetContext["grants"] })} />);
    const summary = await screen.findByText(RUN_COCKPIT.credentialDetails);
    await user.click(summary);
    expect(summary.closest("details")).toHaveAttribute("open");

    const dialog = await open(user, "Credentials");
    expect(within(dialog).getByText(RUN_COCKPIT.credentialDetails).closest("details")).toHaveAttribute("open");
    await user.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(screen.getByText(RUN_COCKPIT.credentialDetails).closest("details")).toHaveAttribute("open");
  });

  it("carries the scroll position over the move, which the browser resets", async () => {
    const user = userEvent.setup();
    render(<RunCanvas ctx={ctx()} />);
    const heading = await screen.findByRole("heading", { name: "Files changed" });
    const body = heading.closest("section")!.lastElementChild as HTMLElement;
    const at = { top: 300, left: 12 };
    // What a browser does: a detached element reads 0, and a move resets what was there.
    Object.defineProperty(body, "scrollTop", { configurable: true, get: () => (body.isConnected ? at.top : 0), set: (v: number) => void (at.top = v) });
    Object.defineProperty(body, "scrollLeft", { configurable: true, get: () => (body.isConnected ? at.left : 0), set: (v: number) => void (at.left = v) });
    const append = Node.prototype.appendChild;
    vi.spyOn(Node.prototype, "appendChild").mockImplementation(function <T extends Node>(this: Node, child: T): T {
      const out = append.call(this, child) as T;
      if ((child as unknown as HTMLElement).dataset?.widgetHost === "files") {
        at.top = 0;
        at.left = 0;
      }
      return out;
    });

    await open(user, "Files changed");
    expect(at).toEqual({ top: 300, left: 12 });
    at.top = 120;
    await user.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(at.top).toBe(120);
  });

  it("keeps the dialog open for a click inside the widget, and closes it for a click on the overlay", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(<RunCanvas ctx={ctx({ grants: [{ id: "g1", scope: "repo:acme/widgets", audience: "github", state: "active" }] as WidgetContext["grants"] })} />);
    const dialog = await open(user, "Credentials");
    await user.click(within(dialog).getByText(RUN_COCKPIT.credentialDetails));
    expect(within(dialog).getByText(RUN_COCKPIT.credentialDetails).closest("details")).toHaveAttribute("open");
    expect(screen.getByRole("dialog", { name: "Credentials" })).toBeInTheDocument();

    await user.click(document.querySelector('[data-slot="dialog-overlay"]')!);
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("opens with the keyboard, lands on the card's body, and Escape puts focus back on the control", async () => {
    const user = userEvent.setup();
    render(<RunCanvas ctx={ctx()} />);
    const button = await control("Identity");
    button.focus();
    await user.keyboard("{Enter}");
    const dialog = await screen.findByRole("dialog", { name: "Identity" });
    await waitFor(() => expect(dialog.querySelector("[data-widget-body]")).toHaveFocus());
    // The control keeps its place in the header, but is no longer something to press or tab to.
    expect(within(dialog).getByRole("button", { name: expandName("Identity"), hidden: true })).toHaveAttribute("tabindex", "-1");

    await user.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await waitFor(() => expect(screen.getByRole("button", { name: expandName("Identity") })).toHaveFocus());
  });

  it("is not available while arranging: the control keeps its place, unseen and out of the tab order", async () => {
    const user = userEvent.setup();
    render(<RunCanvas ctx={ctx()} />);
    await control("Egress");
    await user.click(screen.getByRole("button", { name: "Edit layout" }));
    const buttons = document.querySelectorAll("[data-widget-expand]");
    expect(buttons.length).toBeGreaterThan(0);
    buttons.forEach((b) => {
      expect(b).toHaveClass("invisible");
      expect(b).toHaveAttribute("tabindex", "-1");
    });
  });

  it("is not available in focus mode, and never reopens when focus ends", async () => {
    const user = userEvent.setup();
    const { rerender } = render(<RunCanvas ctx={ctx()} />);
    await open(user, "Egress");
    focusState.on = true;
    rerender(<RunCanvas ctx={ctx()} />);
    expect(screen.queryByRole("dialog")).toBeNull();
    focusState.on = false;
    rerender(<RunCanvas ctx={ctx()} />);
    await control("Egress");
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("stays out of the way of a finished run's fit: the tile is empty while expanded and is not measured", async () => {
    const user = userEvent.setup();
    measured.px = { identity: 700 };
    const done = () => ctx({ finished: true, evidence: { files: 3, sandbox: null } });
    const { rerender } = render(<RunCanvas ctx={done()} />);
    const tile = async () => (await screen.findByRole("heading", { name: "Identity" })).closest(".react-grid-item") as HTMLElement;
    await waitFor(async () => expect(parseFloat((await tile()).style.height)).toBeGreaterThanOrEqual(700));
    const before = (await tile()).style.height;

    await open(user, "Identity");
    // The tile now holds nothing, so a real measure would read 0; the fake reads 100. Neither may reach `fitted`.
    measured.px = { identity: 100 };
    rerender(<RunCanvas ctx={done()} />);
    expect(document.querySelector('[data-widget="identity"]')).toHaveStyle({ height: before });

    await user.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await waitFor(async () => expect(parseFloat((await tile()).style.height)).toBeLessThan(700));
  });
});

describe("RunCanvas — Expand keeps what a viewer can see and do", () => {
  const sshLayout = () => withLayout([TERMINAL, { widget: "ssh", x: 8, y: 0, w: 4, h: 5 }]);
  const SSH = "Attach from your terminal";

  it("offers no ssh control to the admin view, even on the admin's own run", async () => {
    sshLayout();
    render(adminUI(adminCtx({ view: "admin", run: { ...RUN, created_by: "admin@example.com", state: "RUNNING" } as WidgetContext["run"] })));
    await screen.findByTestId("run-terminal-pane");
    await waitFor(() => expect(getLayout).toHaveBeenCalled());
    expect(screen.queryByRole("button", { name: expandName(SSH) })).toBeNull();
    expect(screen.queryByRole("heading", { name: SSH })).toBeNull();
  });

  it("closes an expanded ssh card when the view turns to the admin's, and does not reopen it when it turns back", async () => {
    const user = userEvent.setup();
    sshLayout();
    const { rerender } = render(adminUI(adminCtx()));
    await open(user, SSH);
    rerender(adminUI(adminCtx({ view: "admin" })));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(screen.queryByRole("heading", { name: SSH })).toBeNull();
    rerender(adminUI(adminCtx({ view: "user" })));
    await control(SSH);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("closes an expanded ssh card when the run finishes", async () => {
    const user = userEvent.setup();
    sshLayout();
    const { rerender } = render(adminUI(adminCtx()));
    await open(user, SSH);
    rerender(
      adminUI(adminCtx({ finished: true, run: { ...RUN, created_by: "svc", operator_owned: true, state: "KILLED" } as WidgetContext["run"] })),
    );
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(screen.queryByRole("heading", { name: SSH })).toBeNull();
  });

  const SERVICE_RUN = { ...RUN, service: { restart: "on_failure" }, ends_at: null } as unknown as WidgetContext["run"];
  it("closes an expanded Service panel when the service stops", async () => {
    const user = userEvent.setup();
    const { rerender } = render(<RunCanvas ctx={ctx({ run: SERVICE_RUN })} />);
    const dialog = await open(user, "Service");
    expect(within(dialog).getByTestId("service-panel")).toBeInTheDocument();
    rerender(<RunCanvas ctx={ctx({ run: { ...SERVICE_RUN, state: "KILLED" } as WidgetContext["run"], finished: true })} />);
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(screen.queryByTestId("service-panel")).toBeNull();
  });

  it("makes the Service card the tile's one root, so the fill rule reaches it and its footnote is never clipped", async () => {
    render(<RunCanvas ctx={ctx({ run: SERVICE_RUN })} />);
    const card = await screen.findByTestId("service-panel");
    expect(card.tagName).toBe("SECTION");
    expect(card.parentElement).toHaveClass("tile-scroll");
    expect(card.parentElement!.children).toHaveLength(1);
  });

  const MAC = { ...RUN, machine_kind: "macos_vm", ui_apps: [{ name: "novnc", port: 6080 }] };
  const desktopUI = (run: object) => (
    <MemoryRouter>
      <OperatorProvider operator={false} principal="me">
        <RunCanvas ctx={ctx({ run: run as WidgetContext["run"], principal: "me" })} />
      </OperatorProvider>
    </MemoryRouter>
  );
  beforeEach(() => {
    healthMock.mockResolvedValue({ status: "ok", ui_sandbox: { enabled: true, enter_post_url: "http://ui.local/__wardyn/enter" } });
    withLayout([TERMINAL, { widget: "desktop", x: 8, y: 0, w: 4, h: 4 }]);
  });

  it("keeps a view-only macOS desktop view only when expanded: the chip and Watch, no Pause, no phone line", async () => {
    const user = userEvent.setup();
    render(desktopUI({ ...MAC, macos: { desktop: "observe" } }));
    const dialog = await open(user, "Desktop");
    expect(await within(dialog).findByText(DESKTOP.viewOnlyChip)).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: DESKTOP.watch })).toBeInTheDocument();
    expect(within(dialog).queryByRole("button", { name: DESKTOP.pauseToSignIn })).toBeNull();
    expect(within(dialog).queryByText(DESKTOP.phoneBefore)).toBeNull();
    expect(within(dialog).getByText(DESKTOP.observeOnlyMacOS)).toBeInTheDocument();
  });

  it("says the same words expanded as the tile: a background watch can't type, paste or copy out", async () => {
    const user = userEvent.setup();
    render(desktopUI({ ...MAC, experience: "background", background_watch: { state: "open" } }));
    const dialog = await open(user, "Desktop");
    expect(within(dialog).getByText("You can watch this desktop. Typing, clicking, pasting and clipboard copy are disabled.")).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: DESKTOP.watch })).toBeInTheDocument();
  });

  it("keeps the desktop's own state across the move: the same instance asks /healthz once", async () => {
    const user = userEvent.setup();
    render(desktopUI({ ...MAC, macos: { desktop: "observe" } }));
    await screen.findByText(DESKTOP.viewOnlyChip);
    const asked = healthMock.mock.calls.length;
    await open(user, "Desktop");
    await user.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(healthMock.mock.calls.length).toBe(asked);
  });
});

describe("RunCanvas — Expand and the approvals that hold the run", () => {
  it("seats the decision row in the dialog, keeps its state across open and close, and gives it back to the terminal tile", async () => {
    const user = userEvent.setup();
    listApprovalsMock.mockResolvedValue(
      [1, 2, 3].map((n) =>
        makeApproval({ id: `a${n}`, run_id: "run-1", kind: "egress_domain", requested_scope: { host: `h${n}.example` }, state: "PENDING", requested_at: "" }),
      ),
    );
    render(<RunCanvas ctx={ctx({ decisionPane: <LiveApprovals runId="run-1" /> })} />);
    const terminal = screen.getByTestId("run-terminal-pane");
    await user.click(await within(terminal).findByTestId("live-approvals-show-all"));

    const dialog = await open(user, "Egress");
    expect(within(terminal).queryByTestId("live-approvals-show-all")).toBeNull();
    expect(within(dialog).getByTestId("live-approvals-show-all")).toHaveTextContent("Show fewer");

    await user.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(within(screen.getByTestId("run-terminal-pane")).getByTestId("live-approvals-show-all")).toHaveTextContent("Show fewer");
  });
});

describe("RunCanvas — Escape, topmost first", () => {
  it("closes a Deny confirm over the expanded view first, and the view on the next press", async () => {
    const user = userEvent.setup();
    listApprovalsMock.mockResolvedValue([
      makeApproval({ id: "t1", run_id: "run-1", kind: "tool_call", requested_scope: { tool: "bash.exec", cmd: "ls" }, state: "PENDING", requested_at: "" }),
    ]);
    render(<RunCanvas ctx={ctx({ decisionPane: <LiveApprovals runId="run-1" /> })} />);
    const dialog = await open(user, "Egress");
    await user.click(await within(dialog).findByRole("button", { name: "Deny" }));
    const confirm = await screen.findByRole("alertdialog");
    await user.keyboard("{Escape}");
    await waitFor(() => expect(confirm).not.toBeInTheDocument());
    expect(screen.getByRole("dialog", { name: "Egress" })).toBeInTheDocument();
    await user.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });
});

describe("RunCanvas — the large view never shrinks while it is open", () => {
  it("holds the tallest height it has reached, however short the widget gets after", async () => {
    const user = userEvent.setup();
    const watchers: ResizeObserverCallback[] = [];
    vi.stubGlobal(
      "ResizeObserver",
      class {
        constructor(cb: ResizeObserverCallback) {
          watchers.push(cb);
        }
        observe() {}
        unobserve() {}
        disconnect() {}
      },
    );
    const height = { px: 400 };
    vi.spyOn(HTMLElement.prototype, "offsetHeight", "get").mockImplementation(function (this: HTMLElement) {
      return this.dataset.slot === "dialog-content" ? height.px : 0;
    });
    try {
      render(<RunCanvas ctx={ctx()} />);
      const dialog = await open(user, "Files changed");
      const fire = () => act(() => watchers.forEach((cb) => cb([], {} as ResizeObserver)));
      fire();
      expect(dialog.style.getPropertyValue("--expand-min")).toBe("400px");
      height.px = 150;
      fire();
      expect(dialog.style.getPropertyValue("--expand-min")).toBe("400px");
      height.px = 520;
      fire();
      expect(dialog.style.getPropertyValue("--expand-min")).toBe("520px");
    } finally {
      vi.unstubAllGlobals();
    }
  });
});
