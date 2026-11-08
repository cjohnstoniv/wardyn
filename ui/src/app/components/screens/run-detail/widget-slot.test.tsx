/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// R4-F021: the per-widget ErrorBoundary landed on the CANVAS only (6118c5c3),
// while focus mode's dock renders the same RUN_WIDGETS entries. A widget that
// threw in the dock therefore escaped to app-shell's boundary and replaced the
// whole main region — session included — on the one screen where the session IS
// the page. Both renderers now mount widgets through WidgetSlot, so this suite
// asserts the SAME contract twice: one throwing widget, the session survives.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

const getLayout = vi.fn();
const putLayout = vi.fn();
vi.mock("../../../lib/api/run-layout", () => ({
  runLayout: {
    getLayout: (...a: unknown[]) => getLayout(...a),
    putLayout: (...a: unknown[]) => putLayout(...a),
  },
}));
vi.mock("../../../lib/api/runs", () => ({
  runs: {
    getFiles: vi.fn().mockRejectedValue(new Error("no runner in this test")),
    getResources: vi.fn().mockRejectedValue(new Error("no runner in this test")),
    getModelUsage: vi.fn().mockResolvedValue({ totals: { input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, cache_write_tokens: 0, reasoning_tokens: 0, calls: 0, uncounted_calls: 0, partial_calls: 0 }, by_model: [], opaque_tunnels: 0 }),
  },
}));
// The unmapped-wire-value case the boundary exists for, on the widget both
// renderers show first.
vi.mock("./widgets", async () => {
  const real = await vi.importActual<typeof import("./widgets")>("./widgets");
  return {
    ...real,
    EgressWidget: () => {
      throw new Error("unmapped wire value");
    },
  };
});

import { AppShell } from "../app-shell";
import { ThemeProvider } from "../../wardyn/theme-provider";
import { RunCanvas } from "./canvas";
import { FocusMode } from "./focus-mode";
import type { WidgetContext } from "./widget-registry";
import { WidgetSlot } from "./widget-slot";

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

function ctx(): WidgetContext {
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
    terminalPane: <div data-testid="hero">the session</div>,
  };
}

beforeEach(() => {
  getLayout.mockReset().mockResolvedValue({ preset: "live", layout: [] });
  putLayout.mockReset().mockResolvedValue({ preset: "live", layout: [] });
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
  // The boundary logs the caught error on purpose (error-boundary.tsx's
  // componentDidCatch); silence it so a deliberate throw is not read as noise.
  vi.spyOn(console, "error").mockImplementation(() => {});
});
afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe("one throwing widget is contained — in BOTH renderers of RUN_WIDGETS", () => {
  it("the canvas keeps the session and shows the widget's own error card", async () => {
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
    expect(await screen.findByText("unmapped wire value")).toBeInTheDocument();
    expect(screen.getByTestId("hero")).toBeInTheDocument();
    // The shell's own boundary names its region by the route's pathname, never "Runs".
    expect(screen.queryByText(/Something went wrong rendering \/runs\/run-1/)).toBeNull();
  });

  it("focus mode does too — the dock degrades, the full-screen session stays up", () => {
    render(<FocusMode ctx={ctx()} onExit={() => {}} />);
    expect(screen.getByText("unmapped wire value")).toBeInTheDocument();
    expect(screen.getByTestId("hero")).toBeInTheDocument();
    // The dock's boundary names the WIDGET, so the operator is told which card
    // failed rather than being handed a blank overlay.
    expect(screen.getByText(/Something went wrong rendering Egress/)).toBeInTheDocument();
  });

  it("switching the dock to a healthy widget drops the previous widget's error", () => {
    render(<FocusMode ctx={ctx()} onExit={() => {}} />);
    expect(screen.getByText(/Something went wrong rendering Egress/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Show Sandbox" }));
    expect(screen.getByRole("region", { name: "Sandbox" })).toBeInTheDocument();
    expect(screen.queryByText(/Something went wrong rendering/)).toBeNull();
    expect(screen.queryByText("unmapped wire value")).toBeNull();
  });
});

// #3940: the more-to-scroll cue is a class on the slot, so the canvas tile and the dock panel both get it.
describe("the more-to-scroll cue", () => {
  it("is on the slot every renderer mounts a widget through", () => {
    render(
      <WidgetSlot region="Egress" resetKey="r">
        <section data-testid="card">card</section>
      </WidgetSlot>,
    );
    expect(screen.getByTestId("card").parentElement).toHaveClass("tile-scroll");
  });

  // The thumb is the cue that must reach 3:1 (the tint cannot). Read the real tokens, in both themes.
  it("draws its thumb in a token that is at least 3:1 against the card, light and dark", () => {
    const css = readFileSync(resolve(process.cwd(), "src/styles/theme.css"), "utf8");
    const token = (block: string, name: string) => new RegExp(`${name}:\\s*(#[0-9a-fA-F]{6})`).exec(block)?.[1];
    const channel = (c: number) => (c / 255 <= 0.03928 ? c / 255 / 12.92 : ((c / 255 + 0.055) / 1.055) ** 2.4);
    const luminance = (hex: string) => {
      const n = parseInt(hex.slice(1), 16);
      return 0.2126 * channel(n >> 16) + 0.7152 * channel((n >> 8) & 255) + 0.0722 * channel(n & 255);
    };
    const contrast = (a: string, b: string) => {
      const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x);
      return (hi + 0.05) / (lo + 0.05);
    };
    const rule = /\.tile-scroll > section:only-child > :last-child::-webkit-scrollbar-thumb \{[^}]*background: var\((--[a-z-]+)\)/.exec(css);
    expect(rule, "the thumb rule").not.toBeNull();
    const thumb = rule![1];
    const light = css.slice(css.indexOf(":root {"), css.indexOf("\n.dark {"));
    const dark = css.slice(css.indexOf("\n.dark {"));
    for (const [name, block] of [["light", light], ["dark", dark]] as const) {
      const fg = token(block, thumb);
      const bg = token(block, "--card");
      expect(fg && bg, `${name} tokens`).toBeTruthy();
      expect(contrast(fg!, bg!), `${name} thumb over card`).toBeGreaterThanOrEqual(3);
    }
  });
});
