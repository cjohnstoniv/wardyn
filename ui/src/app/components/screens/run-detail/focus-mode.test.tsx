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
import { MOD, chordLabel } from "../../wardyn/kbd";
import type { WidgetContext } from "./widget-registry";
import { aheadByHours } from "../../../lib/test-clock";

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
    // A non-owner, non-admin viewer keeps the ssh widget unavailable, so this
    // suite never has to stand up the health / ssh-key fetches.
    principal: null,
    operator: false,
    view: "user",
    grants: [],
    egress: [],
    heldCount: 0,
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
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL) =>
      // A /me that ANSWERS (operator). These suites are about the cockpit, not
      // identity: since B1's route-shell gate, a settled-but-unknown identity
      // renders the banner and no route at all — so a rejected /me here would
      // mount nothing to test. Everything else stays "network down".
      String(input).endsWith("/api/v1/me")
        ? Promise.resolve(
            new Response(
              JSON.stringify({ principal: "operator", method: "token", operator: true, role: "admin", identity_provider: "embedded" }),
              { status: 200, headers: { "content-type": "application/json" } },
            ),
          )
        : Promise.reject(new Error("network down")),
    ),
  );
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

    // Normal mode: the full shell is exactly as it always was.
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

  // F1-F6: the dock used to store the raw selection with no clamp — when the
  // open widget stops being dockable (ssh, once the run it belongs to
  // finishes), its rail button vanished but the panel kept the glass panel
  // open over a widget that can no longer place a tile (ConnectSSHCard
  // returns null).
  it("clamps a stale dock selection once its widget stops being dockable", async () => {
    // ticket: F1-F6
    const user = userEvent.setup();
    const running = { ...RUN, state: "RUNNING" } as WidgetContext["run"];
    const { rerender } = render(
      <FocusMode ctx={ctx({ principal: "me", operator: true, run: running })} onExit={() => {}} />,
    );

    await user.click(screen.getByRole("button", { name: RUN_COCKPIT.showWidget("Attach from your terminal") }));
    expect(screen.getByRole("region", { name: "Attach from your terminal" })).toBeInTheDocument();

    // The run finishes — ssh's own gate (state === "RUNNING") drops it.
    const finished = { ...RUN, state: "COMPLETED" } as WidgetContext["run"];
    rerender(
      <FocusMode
        ctx={ctx({ principal: "me", operator: true, finished: true, run: finished })}
        onExit={() => {}}
      />,
    );
    expect(screen.queryByRole("region", { name: "Attach from your terminal" })).not.toBeInTheDocument();
  });
});

describe("Focus mode — the bottom strip states the facts", () => {
  const egress = [
    { id: "e1", time: aheadByHours(-4), domain: "api.anthropic.com", decision: "allow" },
    { id: "e2", time: aheadByHours(-3), domain: "proxy.golang.org", decision: "allow" },
    { id: "e3", time: aheadByHours(-2), domain: "api.github.com", decision: "pending" },
    { id: "e4", time: aheadByHours(-1), domain: "telemetry.vendor.io", decision: "deny" },
  ] as unknown as WidgetContext["egress"];

  // Scoped to the strip on purpose: the dock's Egress widget states "1 held"
  // too, and the strip's whole claim is that you can read the facts WITHOUT
  // opening the widget that holds them.
  const strip = () => within(screen.getByText(RUN_COCKPIT.shortcutExitFocus).closest("div.flex.h-9")!);

  it("reads egress, credentials and the barrier without opening anything", () => {
    render(
      <FocusMode
        ctx={ctx({
          egress,
          // B3: one of the four rows above is an egress.pending EVENT, and one
          // approval is held right now. They agree here so the other
          // assertions stay readable; the case where they DISAGREE — which is
          // the bug — is the next test.
          heldCount: 1,
          grants: [{ id: "g1", scope: "repo:acme/widgets", audience: "github", state: "active" }],
          audit: [
            {
              id: "a1",
              time: aheadByHours(-0.5),
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

  // B3 — the SECOND copy of the lying count (the first is the Egress widget's
  // own chip). The audit trail is append-only, so the egress.pending row for a
  // hold that was approved an hour ago is still there and always will be:
  // deriving "held" from it made the strip claim a hold on a run holding
  // nothing. allow/deny still come from the rows, because those ARE settled.
  it("states the LIVE held count, not the egress.pending rows in the trail", () => {
    render(<FocusMode ctx={ctx({ egress, heldCount: 0 })} onExit={() => {}} />);

    expect(strip().getByText(RUN_COCKPIT.held(0))).toBeInTheDocument();
    expect(strip().queryByText(RUN_COCKPIT.held(1))).toBeNull();
    // …and the settled halves are untouched by the change.
    expect(strip().getByText(RUN_COCKPIT.allow(2))).toBeInTheDocument();
    expect(strip().getByText(RUN_COCKPIT.deny(1))).toBeInTheDocument();
  });

  it("does NOT count a denied credential.mint as brokered", () => {
    render(
      <FocusMode
        ctx={ctx({
          grants: [{ id: "g1", scope: "repo:acme/widgets", audience: "github", state: "active" }],
          audit: [
            {
              id: "a1",
              time: aheadByHours(-0.5),
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

  it("hints only the shortcuts it actually binds, as labelled key chips", () => {
    render(<FocusMode ctx={ctx()} onExit={() => {}} />);
    // Two chords, each a key cap beside the word for what it does.
    const caps = document.querySelectorAll("kbd");
    expect([...caps].map((k) => k.textContent)).toEqual([chordLabel([MOD, "\\"]), "Esc"]);
    expect(screen.getByText(RUN_COCKPIT.shortcutDock)).toBeInTheDocument();
    expect(screen.getByText(RUN_COCKPIT.shortcutExitFocus)).toBeInTheDocument();
    // ⌘K / ⌘1..4 / ⇧⌘F are on the board's strip and are NOT wired here.
    for (const cap of caps) expect(cap.textContent).not.toMatch(/⌘K|⌘1|⇧⌘F/);
  });
});
