/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { OperatorProvider } from "../../../wardyn/operator-context";
import { NR_REVIEW } from "../../../wardyn/copy/new-run/review";
import { SCENARIO_PREFLIGHT } from "../../../../lib/new-run-fixtures/scenarios/preflight";
import type { RunIntent } from "../draft/run-intent";
import { NEW_RUN_SCREENS, NewRunRoute, type Seed, type SeedLoaders } from "../new-run-route";
import { ReviewScreen } from "./review-screen";

const { checks, wfetchMock } = vi.hoisted(() => ({
  checks: { current: {} as Record<string, unknown> },
  wfetchMock: vi.fn(),
}));
vi.mock("../draft/use-intent-checks", () => ({
  useDraftIdentity: () => ({ principal: "alice", resolved: true, revision: 1, authGeneration: 0 }),
  useIntentChecks: () => checks.current,
}));
vi.mock("../../../../lib/api/core", async (orig) => ({ ...(await orig<typeof import("../../../../lib/api/core")>()), wfetch: (...a: unknown[]) => wfetchMock(...a) }));

const RUN_ID = "11111111-1111-4111-8111-111111111111";
const INERT = Object.fromEntries(Object.keys(NEW_RUN_SCREENS).map((name) => [name, () => null])) as unknown as typeof NEW_RUN_SCREENS;
const AGENT: RunIntent = {
  shape: "agent_task", experience: "interactive", runner_pool_id: "pool-general", no_repositories_or_drives: true,
  workload: { kind: "agent_task", agent: "claude-code" },
  tools: [{ id: "claude-code", kind: "harness" }],
  startup: { kind: "harness", tool: "claude-code", prompt: "Fix the build" },
};
const json = (body: unknown) => new Response(JSON.stringify(body), { status: 200, headers: { "content-type": "application/json" } });

function open(intent: RunIntent, o: { walls?: Record<string, unknown> | "fail" } = {}) {
  const loaders: SeedLoaders = {
    template: async (): Promise<Seed> => { throw new Error("no template"); },
    again: async (): Promise<Seed> => ({ origin: { kind: "again", runId: RUN_ID, title: "Nightly" }, intent }),
  };
  wfetchMock.mockImplementation(async (path: string) => {
    if (path.startsWith("/me/drives")) return json({ drives: [], pool_checked: true });
    if (path === "/me/walls" && o.walls === "fail") throw new Error("walls unavailable");
    if (path === "/me/walls") return json({ spec: o.walls ?? {} });
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
const group = (id: string) => within(document.querySelector(`[data-review-group=${id}]`) as HTMLElement);
const ready = async () => screen.findByRole("button", { name: NR_REVIEW.LAUNCH_RUN });

beforeEach(() => {
  checks.current = {
    generation: 5,
    preflightRead: { generation: 5, at: Date.now() - 1000 },
    preflight: vi.fn(),
    preflightResult: SCENARIO_PREFLIGHT.member,
    preflightError: null,
    preflightReason: "",
    preview: { result: null, retry: vi.fn() },
  };
});
afterEach(() => {
  cleanup();
  wfetchMock.mockReset();
  vi.restoreAllMocks();
});

describe("Review: the person's walls", () => {
  it("are read once for the Access group and the policy card together, so their counts cannot differ", async () => {
    open(AGENT);
    await ready();
    await screen.findByLabelText(NR_REVIEW.POLICY_TITLE);
    await waitFor(() => expect(wfetchMock.mock.calls.some(([p]) => p === "/me/walls")).toBe(true));
    expect(wfetchMock.mock.calls.filter(([p]) => p === "/me/walls")).toHaveLength(1);
  });
});

describe("Review: Your walls apply (G4a)", () => {
  it("says which walls apply above the groups, not inside the Access group", async () => {
    open(AGENT, { walls: { denied_domains: ["x.example"], first_use_approval: "always_deny" } });
    await ready();
    const line = await screen.findByTestId("walls-apply");
    expect(line).toHaveTextContent("Your walls apply: 1 site never reached. Change your walls");
    expect(screen.getByRole("link", { name: "Change your walls" })).toHaveAttribute("target", "_blank");
    expect(document.querySelectorAll("[data-testid=walls-apply]")).toHaveLength(1);
    expect(group("access").queryByTestId("walls-apply")).toBeNull();
    expect(line.compareDocumentPosition(document.querySelector("[data-review-group=run]") as HTMLElement) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("says nothing for walls that name nothing, or when they could not be read", async () => {
    open(AGENT, { walls: "fail" });
    await ready();
    await screen.findByLabelText(NR_REVIEW.POLICY_TITLE);
    await waitFor(() => expect(wfetchMock.mock.calls.some(([p]) => p === "/me/walls")).toBe(true));
    expect(screen.queryByTestId("walls-apply")).toBeNull();
  });
});
