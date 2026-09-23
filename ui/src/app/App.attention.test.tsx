/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// X3-F13/R-1: split out of App.test.tsx during the feat/v0.7.4 merge — that
// file's own H1/L4/M2/M3 suite (ui-setup-shell) stubs RunsScreen entirely
// (`vi.mock("./components/screens/runs", ...)`), and R-1's own case below
// needs the REAL RunsScreen (it is what publishes the counts this suite
// asserts on) — the two mock sets cannot share one file. This one instead
// mocks `./lib/api/core` (probeAuth) and `./lib/use-poll` (a recorder, not a
// real ticking interval) so App() reaches "authed" without a real network and
// its pollers' registrations are inspectable directly.
import { describe, it, expect, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

vi.mock("./lib/api/core", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./lib/api/core")>();
  return { ...actual, probeAuth: vi.fn().mockResolvedValue("authed") };
});
// R-1's suite drives real listRuns/listApprovals data through App AND the
// real RunsScreen it routes to — real fetches would just throw (no backend in
// this test), which health.ts/setup.ts already swallow internally, but
// runs.ts/approvals.ts do not.
const listRunsMock = vi.fn().mockResolvedValue([]);
const listApprovalsMock = vi.fn().mockResolvedValue([]);
vi.mock("./lib/api/runs", () => ({ runs: { listRuns: (...a: unknown[]) => listRunsMock(...a) } }));
vi.mock("./lib/api/approvals", () => ({
  approvals: { listApprovals: (...a: unknown[]) => listApprovalsMock(...a) },
}));
// AppShell's own useMeta needs a resolved identity (identityResolved: true) or
// its nav renders NO items at all (fail-closed, unlike the role/operator
// defaults) — a real /me fetch here just throws (no backend), which
// health.ts's whoami() swallows into `null`, permanently unresolved.
vi.mock("./lib/api/health", () => ({
  health: {
    whoami: vi.fn().mockResolvedValue({
      principal: "operator",
      method: "token",
      operator: true,
      role: "admin",
      identity_provider: "embedded",
    }),
    health: vi.fn().mockResolvedValue({}),
    readyz: vi.fn().mockResolvedValue({ status: "ok" }),
    logout: vi.fn().mockResolvedValue(true),
  },
}));
// X3-F13: usePoll's actual ticking is irrelevant here — what's under test is
// the PAUSED boolean each caller registers it with. Recording by function
// reference (App's refreshBadges/refreshHealth are useCallback-stable) means
// a later render's call OVERWRITES the earlier one, so reading the map after
// settling gives each poller's final, real paused state.
const pollRegistry = new Map<() => void | Promise<unknown>, { ms: number; paused: boolean }>();
vi.mock("./lib/use-poll", async (importOriginal) => ({
  // PollPauseContext stays real: App provides it around every route (#483).
  ...(await importOriginal<typeof import("./lib/use-poll")>()),
  usePoll: (fn: () => void | Promise<unknown>, ms: number, paused: boolean) => {
    pollRegistry.set(fn, { ms, paused });
  },
}));

import App from "./App";

// X3-F13: the shell's own attention-badge tick (refreshBadges, two unscoped
// LIST_LIMIT reads — "the most expensive tick in the shell") duplicates work
// the Runs board already does on its own 3s poll while parked there.
describe("App — the shell's attention-badge poll pauses on /runs (X3-F13)", () => {
  it("stops ticking while parked on /runs — only the health heartbeat stays live", async () => {
    pollRegistry.clear();
    render(
      <MemoryRouter initialEntries={["/runs"]}>
        <App />
      </MemoryRouter>,
    );
    // Past the "checking" gate (mocked probeAuth resolves "authed") — only
    // once auth has actually settled does the badges poller's registered
    // `paused` reflect the real rule, not the "checking" default.
    await waitFor(() => expect(screen.queryByText("Connecting to Wardyn…")).not.toBeInTheDocument());
    const unpaused5s = [...pollRegistry.values()].filter((v) => v.ms === 5000 && !v.paused).length;
    expect(unpaused5s).toBe(1); // the health heartbeat only
  });

  // Neg: off /runs, the badge poll is the one thing keeping the sidebar count
  // live for a human looking at a DIFFERENT screen.
  it("neg: keeps ticking off /runs", async () => {
    pollRegistry.clear();
    render(
      <MemoryRouter initialEntries={["/policies"]}>
        <App />
      </MemoryRouter>,
    );
    await waitFor(() => expect(screen.queryByText("Connecting to Wardyn…")).not.toBeInTheDocument());
    const unpaused5s = [...pollRegistry.values()].filter((v) => v.ms === 5000 && !v.paused).length;
    expect(unpaused5s).toBe(2); // health + badges
  });
});

// R-1: X3-F13 pausing App's own badge poll on /runs only keeps the nav badges
// live because RunsScreen publishes its counts back up (runs.tsx's
// publishAttention call) — this proves the WHOLE path, not just the pause.
describe("App — the Runs board publishes its counts while the shell's own poll is paused (R-1)", () => {
  it("a new PENDING approval reported by the board increments the Approvals badge", async () => {
    pollRegistry.clear();
    listRunsMock.mockResolvedValue([]);
    listApprovalsMock.mockResolvedValue([]);
    render(
      <MemoryRouter initialEntries={["/runs"]}>
        <App />
      </MemoryRouter>,
    );
    // Past the "checking" gate (mocked probeAuth resolves "authed") — only
    // once auth has actually settled does the badges poller's registered
    // `paused` reflect the real rule, not the "checking" default.
    await waitFor(() => expect(screen.queryByText("Connecting to Wardyn…")).not.toBeInTheDocument());
    await waitFor(() => expect(screen.getByRole("link", { name: "Approvals" })).toBeInTheDocument());

    // A new PENDING approval arrives on the board's next tick. usePoll is
    // mocked to a recorder here (see above) — invoke the callback RunsScreen
    // itself registered (its 3s board poll) directly, rather than depending
    // on a real timer neither this mock nor fake timers would advance.
    listApprovalsMock.mockResolvedValue([
      {
        id: "a1",
        run_id: "run-x",
        kind: "tool_call",
        state: "PENDING",
        requested_at: new Date().toISOString(),
        requested_scope: { tool: "r1.exec" },
      },
    ]);
    const boardTick = [...pollRegistry.entries()].find(([, v]) => v.ms === 3000)?.[0];
    expect(boardTick, "RunsScreen's own board poll must be registered").toBeDefined();
    await boardTick!();

    // \s* not \s+: jsdom's accessible-name computation does not insert a
    // space between the label span and the badge span the way a real
    // browser's accessibility tree does (they carry no literal whitespace
    // text node between them) — "Approvals1" is the real jsdom name.
    await waitFor(() => expect(screen.getByRole("link", { name: /^Approvals\s*1$/ })).toBeInTheDocument());
  });
});
