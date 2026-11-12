/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Review and a window on the Mac (board WIN-2, frames w2-f1-after and w2-review-on): a refusal that names the
// desktop_window field clears the choice where it was made, is never a blocker, and the draft is checked again
// without it; a refusal that only shares a reason holds Launch as any refusal does and keeps the choice.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { OperatorProvider } from "../../../wardyn/operator-context";
import { NR_REVIEW } from "../../../wardyn/copy/new-run/review";
import { NR_WINDOW } from "../../../wardyn/copy/new-run/window";
import { SCENARIO_PREFLIGHT } from "../../../../lib/new-run-fixtures/scenarios/preflight";
import type { RunIntent } from "../draft/run-intent";
import { NEW_RUN_SCREENS, NewRunRoute, type Seed, type SeedLoaders } from "../new-run-route";
import { ReviewScreen } from "./review-screen";

const { checks, bodies, wfetchMock } = vi.hoisted(() => ({
  checks: { current: {} as Record<string, unknown> },
  bodies: [] as unknown[],
  wfetchMock: vi.fn(),
}));
vi.mock("../draft/use-intent-checks", () => ({
  useDraftIdentity: () => ({ principal: "alice", resolved: true, revision: 1, authGeneration: 0 }),
  useIntentChecks: (p: { body: unknown }) => {
    bodies.push(p.body);
    return checks.current;
  },
}));
vi.mock("../../../../lib/api/core", async (orig) => ({ ...(await orig<typeof import("../../../../lib/api/core")>()), wfetch: (...a: unknown[]) => wfetchMock(...a) }));

const RUN_ID = "11111111-1111-4111-8111-111111111111";
const INERT = Object.fromEntries(Object.keys(NEW_RUN_SCREENS).map((name) => [name, () => null])) as unknown as typeof NEW_RUN_SCREENS;
const MAC: RunIntent = {
  shape: "agent_task", experience: "interactive", runner_pool_id: "pool-general", no_repositories_or_drives: true,
  workload: { kind: "agent_task", agent: "claude-code" },
  tools: [{ id: "claude-code", kind: "harness" }],
  machine_kind: "macos_vm", desktop_window: "at_start",
};
const UNRECORDED = "This run's recording level is Screen and actions, and a window on your Mac isn't recorded, so Wardyn won't open one. Watch its desktop from the run page instead.";
const PROFILE = "Your organisation's governance profile doesn't allow an interactive desktop on this run.";
const json = (body: unknown) => new Response(JSON.stringify(body), { status: 200, headers: { "content-type": "application/json" } });

function open(intent: RunIntent) {
  const loaders: SeedLoaders = {
    template: async (): Promise<Seed> => { throw new Error("no template"); },
    again: async (): Promise<Seed> => ({ origin: { kind: "again", runId: RUN_ID, title: "Nightly" }, intent }),
  };
  wfetchMock.mockImplementation(async (path: string) => {
    if (path.startsWith("/me/drives")) return json({ drives: [], pool_checked: true });
    if (path === "/me/walls") return json({ spec: {} });
    if (path === "/templates") return json({ templates: [], publish: { org: false, groups: [], groups_unverified: false } });
    throw new Error(`unexpected ${path}`);
  });
  render(
    <MemoryRouter initialEntries={[`/runs/new?again=${RUN_ID}`]}>
      <OperatorProvider operator={false} principal="alice">
        <Routes>
          <Route path="/runs/new" element={<NewRunRoute loaders={loaders} screens={{ ...INERT, review: (p) => <ReviewScreen {...p} /> }} />} />
        </Routes>
      </OperatorProvider>
    </MemoryRouter>,
  );
}
const refused = (text: string, reason: string, field: string) => ({
  generation: 5,
  preflightRead: { generation: 5, at: Date.now() - 1000 },
  preflight: vi.fn(),
  preflightResult: null,
  preflightError: text,
  preflightReason: reason,
  preflightField: field,
  preview: { result: null, retry: vi.fn() },
});
const lastBody = () => bodies[bodies.length - 1] as Record<string, unknown> | null;
const blockerList = () => document.getElementById("nr-blockers") as HTMLElement;

beforeEach(() => {
  bodies.length = 0;
});
afterEach(cleanup);

describe("Review: a refused window on the Mac", () => {
  it("a refusal naming desktop_window clears the choice in Where it runs, is no blocker, and the draft is checked again without it", async () => {
    checks.current = refused(UNRECORDED, "mac_window_unrecorded", "desktop_window");
    open(MAC);
    expect(await screen.findByText(NR_WINDOW.CLEARED)).toBeInTheDocument();
    expect(screen.getByText(UNRECORDED)).toBeInTheDocument();
    await waitFor(() => expect(lastBody()).not.toBeNull());
    expect(lastBody()).not.toHaveProperty("desktop_window");
    expect(lastBody()).toMatchObject({ machine_kind: "macos_vm" });
    expect(within(blockerList()).queryByText(UNRECORDED)).toBeNull();
    expect(screen.queryByText(NR_WINDOW.REVIEW_SEVERAL_MACS)).toBeNull();
  });

  it("a refusal that shares a reason and names no field is a blocker, and the window stays chosen", async () => {
    checks.current = refused(PROFILE, "governance_unit", "");
    open(MAC);
    await screen.findByRole("button", { name: NR_REVIEW.LAUNCH_RUN });
    await waitFor(() => expect(within(blockerList()).getByText(PROFILE)).toBeInTheDocument());
    expect(screen.queryByText(NR_WINDOW.CLEARED)).toBeNull();
    expect(screen.getByText(NR_WINDOW.REVIEW_SEVERAL_MACS)).toBeInTheDocument();
    expect(lastBody()).toMatchObject({ desktop_window: "at_start" });
    expect(screen.getByRole("button", { name: NR_REVIEW.LAUNCH_RUN })).toHaveAttribute("aria-disabled", "true");
  });

  it("answered, the window is one more line in Where it runs (w2-review-on) and the body carries it", async () => {
    checks.current = { ...refused("", "", ""), preflightError: null, preflightResult: SCENARIO_PREFLIGHT.member };
    open(MAC);
    expect(await screen.findByText(NR_WINDOW.REVIEW_SEVERAL_MACS)).toBeInTheDocument();
    expect(lastBody()).toMatchObject({ desktop_window: "at_start" });
  });
});
