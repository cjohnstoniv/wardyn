/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Focus mode's contract (design board 2c), pinned where it is cheap to pin:
// entering it takes the shell's chrome away, Escape gives it back (WCAG 2.1.2),
// the dock opens and closes, and the bottom strip states the barrier / egress /
// credential facts as text — including the one it must NOT state, a credential
// mint that was denied.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";

const getLayout = vi.fn();
const putLayout = vi.fn();
vi.mock("../../../lib/api/run-layout", () => ({
  runLayout: {
    getLayout: (...a: unknown[]) => getLayout(...a),
    putLayout: (...a: unknown[]) => putLayout(...a),
  },
}));
// The polling evidence widgets fetch on mount; this suite is about the overlay.
vi.mock("../../../lib/api/runs", () => ({
  runs: {
    getFiles: vi.fn().mockRejectedValue(new Error("no runner in this test")),
    getResources: vi.fn().mockRejectedValue(new Error("no runner in this test")),
  },
}));

import { AppShell } from "../app-shell";
import { ThemeProvider } from "../../wardyn/theme-provider";
import { RunCanvas } from "./canvas";
import { FocusMode } from "./focus-mode";
import { RUN_COCKPIT } from "../../wardyn/copy";
import type { WidgetContext } from "./widget-registry";

const RUN = {
  id: "run-1a2b3c4d5e",
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
    // null keeps the ssh widget unavailable (owner-only), so this suite never
    // has to stand up the health / ssh-key fetches.
    principal: null,
    grants: [],
    egress: [],
    audit: [],
    onGoAudit: () => {},
    terminalPane: <div>the session</div>,
    ...overrides,
  };
}

beforeEach(() => {
  getLayout.mockReset();
  putLayout.mockReset();
  getLayout.mockResolvedValue({ preset: "live", layout: [] });
  putLayout.mockResolvedValue({ preset: "live", layout: [] });
});
afterEach(() => vi.unstubAllGlobals());

// The whole point of the mode: the canvas asks, the SHELL answers. Driven
// through the real AppShell rather than a stub context, because "the shell
// hides its own chrome" is the assertion.
function renderCockpitInShell() {
  vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("network down")));
  render(
    <MemoryRouter initialEntries={["/runs/run-1"]}>
      <ThemeProvider>
        <Routes>
          <Route element={<AppShell pendingApprovals={0} attentionCount={0} onSignOut={() => {}} />}>
            <Route path="/runs/:id" element={<RunCanvas ctx={ctx()} />} />
          </Route>
        </Routes>
      </ThemeProvider>
    </MemoryRouter>,
  );
}

describe("Focus mode — the shell gets out of the way", () => {
  it("hides the shell's header and sidebar on entry, and Escape gives them back", async () => {
    const user = userEvent.setup();
    renderCockpitInShell();
    await screen.findByRole("heading", { name: "Egress" });

    // Normal mode: the six-item shell is exactly as it always was.
    expect(screen.getByRole("banner")).toBeInTheDocument();
    expect(screen.getByRole("complementary")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: RUN_COCKPIT.enterFocus }));

    expect(screen.queryByRole("banner")).toBeNull();
    expect(screen.queryByRole("complementary")).toBeNull();
    expect(screen.getByRole("button", { name: RUN_COCKPIT.exitFocus })).toBeInTheDocument();

    // WCAG 2.1.2 — no keyboard trap.
    await user.keyboard("{Escape}");
    await waitFor(() => expect(screen.getByRole("banner")).toBeInTheDocument());
    expect(screen.getByRole("complementary")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: RUN_COCKPIT.exitFocus })).toBeNull();
  });

  it("the exit button restores the chrome too — the shortcut is not the only way out", async () => {
    const user = userEvent.setup();
    renderCockpitInShell();
    await screen.findByRole("heading", { name: "Egress" });

    await user.click(screen.getByRole("button", { name: RUN_COCKPIT.enterFocus }));
    expect(screen.queryByRole("banner")).toBeNull();

    await user.click(screen.getByRole("button", { name: RUN_COCKPIT.exitFocus }));
    await waitFor(() => expect(screen.getByRole("banner")).toBeInTheDocument());
  });
});

describe("Focus mode — the edge dock", () => {
  it("opens on a widget, closes, and comes back from the rail", async () => {
    const user = userEvent.setup();
    render(<FocusMode ctx={ctx()} onExit={() => {}} />);

    // The dock starts on the first widget in the registry order.
    expect(screen.getByRole("heading", { name: "Egress" })).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: RUN_COCKPIT.closeDock }));
    expect(screen.queryByRole("heading", { name: "Egress" })).toBeNull();

    // The rail survives a closed dock — otherwise ⌘\ would be the only way back.
    await user.click(screen.getByRole("button", { name: RUN_COCKPIT.showWidget("Identity") }));
    expect(screen.getByRole("heading", { name: "Identity" })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Egress" })).toBeNull();
  });

  it("⌘\\ toggles it", async () => {
    const user = userEvent.setup();
    render(<FocusMode ctx={ctx()} onExit={() => {}} />);
    expect(screen.getByRole("heading", { name: "Egress" })).toBeInTheDocument();

    await user.keyboard("{Meta>}\\{/Meta}");
    expect(screen.queryByRole("heading", { name: "Egress" })).toBeNull();

    await user.keyboard("{Meta>}\\{/Meta}");
    expect(screen.getByRole("heading", { name: "Egress" })).toBeInTheDocument();
  });
});

describe("Focus mode — the bottom strip states the facts", () => {
  const egress = [
    { id: "e1", time: "2026-01-01T00:00:00Z", domain: "api.anthropic.com", decision: "allow" },
    { id: "e2", time: "2026-01-01T00:01:00Z", domain: "proxy.golang.org", decision: "allow" },
    { id: "e3", time: "2026-01-01T00:02:00Z", domain: "api.github.com", decision: "pending" },
    { id: "e4", time: "2026-01-01T00:03:00Z", domain: "telemetry.vendor.io", decision: "deny" },
  ] as unknown as WidgetContext["egress"];

  // Scoped to the strip on purpose: the dock's Egress widget states "1 held"
  // too, and the strip's whole claim is that you can read the facts WITHOUT
  // opening the widget that holds them.
  const strip = () => within(screen.getByText(RUN_COCKPIT.shortcuts).closest("div")!);

  it("reads egress, credentials and the barrier without opening anything", () => {
    render(
      <FocusMode
        ctx={ctx({
          egress,
          grants: [{ id: "g1", scope: "repo:acme/widgets", audience: "github", state: "active" }],
          audit: [
            {
              id: "a1",
              time: "2026-01-01T00:04:00Z",
              actor_type: "agent",
              actor: "agent",
              action: "credential.mint",
              target: "github",
              outcome: "success",
            },
          ],
        })}
        onExit={() => {}}
      />,
    );

    expect(strip().getByText(RUN_COCKPIT.allow(2))).toBeInTheDocument();
    expect(strip().getByText(RUN_COCKPIT.held(1))).toBeInTheDocument();
    expect(strip().getByText(RUN_COCKPIT.deny(1))).toBeInTheDocument();
    expect(strip().getByText(RUN_COCKPIT.eligible(1))).toBeInTheDocument();
    expect(strip().getByText(RUN_COCKPIT.brokered(1))).toBeInTheDocument();
    // The metal ramp, by its barrier NAME — never the internal CC2 wire class.
    expect(strip().getByText("Wall")).toBeInTheDocument();
    expect(screen.queryByText("CC2")).toBeNull();
    expect(strip().getByText("docker")).toBeInTheDocument();
  });

  it("does NOT count a denied credential.mint as brokered", () => {
    render(
      <FocusMode
        ctx={ctx({
          grants: [{ id: "g1", scope: "repo:acme/widgets", audience: "github", state: "active" }],
          audit: [
            {
              id: "a1",
              time: "2026-01-01T00:04:00Z",
              actor_type: "agent",
              actor: "agent",
              // The broker audits DENIED mint attempts under this same action;
              // rendering one as brokered would claim a credential that was
              // never issued.
              action: "credential.mint",
              target: "github",
              outcome: "denied",
            },
          ],
        })}
        onExit={() => {}}
      />,
    );

    expect(strip().getByText(RUN_COCKPIT.eligible(1))).toBeInTheDocument();
    expect(strip().getByText(RUN_COCKPIT.brokered(0))).toBeInTheDocument();
    expect(screen.queryByText(RUN_COCKPIT.brokered(1))).toBeNull();
  });

  it("hints only the shortcuts it actually binds", () => {
    render(<FocusMode ctx={ctx()} onExit={() => {}} />);
    const strip = screen.getByText(RUN_COCKPIT.shortcuts);
    expect(strip).toBeInTheDocument();
    // ⌘K / ⌘1..4 / ⇧⌘F are on the board's strip and are NOT wired here.
    expect(strip.textContent).not.toMatch(/⌘K|⌘1|⇧⌘F/);
  });
});
