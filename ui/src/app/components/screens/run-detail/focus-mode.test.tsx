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
import * as React from "react";
import { act, render, screen, waitFor, within } from "@testing-library/react";
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
import { REAUTH_BAR } from "../../../lib/reauth-copy";
import { ReauthContext, useReauthController, type Reauth } from "../../../lib/reauth";
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
    onGoPolicy: () => {},
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
function renderCockpitInShell(Wrap: React.ComponentType<{ children: React.ReactNode }> = React.Fragment) {
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
        <Wrap>
          <Routes>
            <Route element={<AppShell pendingApprovals={0} attentionCount={0} onSignOut={() => {}} />}>
              <Route path="/runs/:id" element={<RunCanvas ctx={ctx()} />} />
            </Route>
          </Routes>
        </Wrap>
      </ThemeProvider>
    </MemoryRouter>,
  );
}

describe("Focus mode — the shell gets out of the way", () => {
  it("starts the overlay under the shell's banners, and drops the offset when focus ends", async () => {
    const user = userEvent.setup();
    const rect = vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockReturnValue({ bottom: 44 } as DOMRect);
    try {
      renderCockpitInShell();
      await screen.findByRole("heading", { name: "Egress" });
      expect(document.documentElement.style.getPropertyValue("--shell-banner-h")).toBe("");
      await user.click(screen.getByRole("button", { name: RUN_COCKPIT.enterFocus }));
      expect(document.documentElement.style.getPropertyValue("--shell-banner-h")).toBe("44px");
      const overlay = screen.getByRole("button", { name: RUN_COCKPIT.exitFocus }).closest(".fixed");
      expect(overlay).toHaveClass("top-[var(--shell-banner-h,0px)]");
      await user.keyboard("{Escape}");
      await waitFor(() => expect(document.documentElement.style.getPropertyValue("--shell-banner-h")).toBe(""));
    } finally {
      rect.mockRestore();
    }
  });

  it("follows the re-authentication bar coming and going while focus is on", async () => {
    const user = userEvent.setup();
    const BAR_H = 36;
    // jsdom has no layout and no resize events: a box is 44px plus the bar's height when the bar is inside it,
    // and a fake observer fires only for a box whose height changed, as the real one does.
    const height = (el: Element) => 44 + (el.textContent?.includes(REAUTH_BAR.BODY) ? BAR_H : 0);
    const rect = vi
      .spyOn(HTMLElement.prototype, "getBoundingClientRect")
      .mockImplementation(function (this: HTMLElement) {
        return { top: 0, bottom: height(this) } as DOMRect;
      });
    const watched = new Map<Element, number>();
    const fireResizes = () =>
      act(() => {
        for (const [el, last] of watched) {
          if (height(el) === last) continue;
          watched.set(el, height(el));
          liveObservers.forEach((o) => o.cb([], o as unknown as ResizeObserver));
        }
      });
    const liveObservers = new Set<{ cb: ResizeObserverCallback }>();
    vi.stubGlobal(
      "ResizeObserver",
      class {
        constructor(readonly cb: ResizeObserverCallback) {
          liveObservers.add(this);
        }
        observe(el: Element) {
          watched.set(el, height(el));
        }
        disconnect() {
          liveObservers.delete(this);
        }
        unobserve() {}
      },
    );
    let reauth: Reauth | undefined;
    function Controlled({ children }: { children: React.ReactNode }) {
      const c = useReauthController(() => {});
      reauth = c.reauth;
      return <ReauthContext.Provider value={c.reauth}>{children}</ReauthContext.Provider>;
    }
    const offset = () => document.documentElement.style.getPropertyValue("--shell-banner-h");
    try {
      renderCockpitInShell(Controlled);
      await screen.findByRole("heading", { name: "Egress" });
      await user.click(screen.getByRole("button", { name: RUN_COCKPIT.enterFocus }));
      expect(offset()).toBe("44px");
      act(() => reauth?.setPhase("bar"));
      await screen.findByText(REAUTH_BAR.BODY);
      fireResizes();
      expect(offset()).toBe(`${44 + BAR_H}px`);
      act(() => reauth?.setPhase("none"));
      await waitFor(() => expect(screen.queryByText(REAUTH_BAR.BODY)).toBeNull());
      fireResizes();
      expect(offset()).toBe("44px");
    } finally {
      rect.mockRestore();
    }
  });

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
          // B3: one of the four rows above is an egress.hold EVENT, and one
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

  // 0.9 (M-RN-3 RS3): a local run names its computer where the bare substrate word was; a remote run keeps it.
  it("names a local run's computer in place of the substrate word, and leaves a remote run's as it was", () => {
    const local = { ...RUN, placement: "local", runner_id: "11111111-2222-3333-4444-555555555555", runner_name: "laptop-7" };
    const { unmount } = render(<FocusMode ctx={ctx({ run: local as unknown as WidgetContext["run"] })} onExit={() => {}} />);
    expect(strip().getByText("On laptop-7")).toBeInTheDocument();
    expect(strip().queryByText("docker")).toBeNull();
    unmount();
    render(<FocusMode ctx={ctx({ run: { ...RUN, placement: "remote" } as unknown as WidgetContext["run"] })} onExit={() => {}} />);
    expect(strip().getByText("docker")).toBeInTheDocument();
    expect(strip().queryByText(/^On /)).toBeNull();
  });

  // B3 — the SECOND copy of the lying count (the first is the Egress widget's
  // own chip). The audit trail is append-only, so the egress.hold row for a
  // hold that was approved an hour ago is still there and always will be:
  // deriving "held" from it made the strip claim a hold on a run holding
  // nothing. allow/deny still come from the rows, because those ARE settled.
  it("states the LIVE held count, not the egress.hold rows in the trail", () => {
    render(<FocusMode ctx={ctx({ egress, heldCount: 0 })} onExit={() => {}} />);

    expect(strip().getByText(RUN_COCKPIT.held(0))).toBeInTheDocument();
    expect(strip().queryByText(RUN_COCKPIT.held(1))).toBeNull();
    // …and the settled halves are untouched by the change.
    expect(strip().getByText(RUN_COCKPIT.allow(2))).toBeInTheDocument();
    expect(strip().getByText(RUN_COCKPIT.deny(1))).toBeInTheDocument();
  });

  it("marks the counts as a floor only when the audit trail's pages left a gap", () => {
    const { unmount } = render(<FocusMode ctx={ctx({ egress, auditGap: true })} onExit={() => {}} />);
    expect(strip().getByText(new RegExp(RUN_COCKPIT.countsFloor))).toBeInTheDocument();
    unmount();
    render(<FocusMode ctx={ctx({ egress })} onExit={() => {}} />);
    expect(strip().queryByText(new RegExp(RUN_COCKPIT.countsFloor))).toBeNull();
  });

  it("counts a network-fault row as failed, not as deny, and hides the count when there is none", () => {
    const { unmount } = render(
      <FocusMode
        ctx={ctx({
          egress: [
            { id: "x1", time: aheadByHours(-1), domain: "a.example.com", decision: "failed" },
            { id: "x2", time: aheadByHours(-1), domain: "b.example.com", decision: "deny" },
          ],
        })}
        onExit={() => {}}
      />,
    );
    expect(strip().getByText(RUN_COCKPIT.failed(1))).toBeInTheDocument();
    expect(strip().getByText(RUN_COCKPIT.deny(1))).toBeInTheDocument();
    unmount();

    render(<FocusMode ctx={ctx({ egress })} onExit={() => {}} />);
    expect(strip().queryByText(RUN_COCKPIT.failed(0))).toBeNull();
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
