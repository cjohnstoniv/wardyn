/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// X3-F13/R-1, retired by #1197 L3: the shell's badge poll used to pause while
// parked on /runs, because the old RunsScreen ran its own listRuns +
// listApprovals poll there and PUBLISHED its counts back up
// (usePublishAttention/AttentionPublisherProvider) to keep the badges fed
// without a redundant second read. #1197 L1b moved the "needs you" rule
// server-side (GET /me/attention), and L3's RunsScreen now reads that same
// endpoint off its own GET /runs?view= fetch instead of computing anything
// client-side — so there is nothing left for it to publish, and the shell's
// poll no longer has a reason to pause anywhere. This file now pins the
// opposite of its old name: the poll runs everywhere, /runs included, with no
// publish side-channel.
//
// Split out of App.test.tsx during the feat/v0.7.4 merge for a reason that
// still holds: that file's own H1/L4/M2/M3 suite (ui-setup-shell) stubs
// RunsScreen entirely (`vi.mock("./components/screens/runs", ...)`), and this
// file needs the REAL RunsScreen mounted at /runs — the two mock sets cannot
// share one file. This one instead mocks `./lib/api/core` (probeAuth) and
// `./lib/use-poll` (a recorder, not a real ticking interval) so App() reaches
// "authed" without a real network and its pollers' registrations are
// inspectable directly.
import { describe, it, expect, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

vi.mock("./lib/api/core", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./lib/api/core")>();
  return { ...actual, probeAuth: vi.fn().mockResolvedValue("authed") };
});
// The real RunsScreen mounted at /runs now calls listRunsFiltered — a real
// fetch would just throw (no backend in this test); mocked to a quiet empty
// page so the badge assertions below aren't muddied by an ErrorState board.
vi.mock("./lib/api/runs", () => ({
  runs: {
    listRunsFiltered: vi.fn().mockResolvedValue({ runs: [], truncated: false, hiddenOlder: 0, hiddenKilled: 0 }),
  },
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
// X3-F13's own recorder: usePoll's actual ticking is irrelevant here — what's
// under test is the PAUSED boolean each caller registers it with. Recording
// by function reference (App's refreshBadges/refreshHealth are
// useCallback-stable) means a later render's call OVERWRITES the earlier
// one, so reading the map after settling gives each poller's final, real
// paused state.
const pollRegistry = new Map<() => void | Promise<unknown>, { ms: number; paused: boolean }>();
vi.mock("./lib/use-poll", async (importOriginal) => ({
  // PollPauseContext stays real: App provides it around every route (#483).
  ...(await importOriginal<typeof import("./lib/use-poll")>()),
  usePoll: (fn: () => void | Promise<unknown>, ms: number, paused: boolean) => {
    pollRegistry.set(fn, { ms, paused });
  },
}));

import App from "./App";

describe("App — the shell's attention-badge poll (#1197 L3: never pauses on /runs)", () => {
  it("keeps ticking on /runs — the same as everywhere else", async () => {
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
    expect(unpaused5s).toBe(2); // health heartbeat + badges — no /runs pause left
  });

  it("neg: keeps ticking off /runs too, same count", async () => {
    pollRegistry.clear();
    render(
      <MemoryRouter initialEntries={["/admin/policies"]}>
        <App />
      </MemoryRouter>,
    );
    await waitFor(() => expect(screen.queryByText("Connecting to Wardyn…")).not.toBeInTheDocument());
    const unpaused5s = [...pollRegistry.values()].filter((v) => v.ms === 5000 && !v.paused).length;
    expect(unpaused5s).toBe(2); // health + badges
  });
});
