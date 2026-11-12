/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { OperatorProvider } from "../../../wardyn/operator-context";
import { NR_REVIEW } from "../../../wardyn/copy/new-run/review";
import { PREFLIGHT_FRESH_MS } from "../draft/launch-gate";
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

function open(intent: RunIntent, onLaunch?: () => void) {
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
          <Route path="/runs/new" element={<NewRunRoute loaders={loaders} screens={{ ...INERT, review: (p) => <ReviewScreen {...p} onLaunch={onLaunch} /> }} />} />
        </Routes>
      </OperatorProvider>
    </MemoryRouter>,
  );
}
const ready = async () => screen.findByRole("button", { name: NR_REVIEW.LAUNCH_RUN });
const blockerList = () => document.getElementById("nr-blockers") as HTMLElement;
const preflight = vi.fn();
const answered = (age: number, over: Record<string, unknown> = {}) => ({
  generation: 5,
  preflightRead: { generation: 5, at: Date.now() - age },
  preflight,
  preflightResult: SCENARIO_PREFLIGHT.member,
  preflightError: null,
  preflightReason: "",
  preview: { result: null, retry: vi.fn() },
  ...over,
});

beforeEach(() => {
  preflight.mockReset();
  checks.current = answered(1000);
});
afterEach(() => {
  cleanup();
  wfetchMock.mockReset();
  vi.restoreAllMocks();
});

describe("Review: a check that ages out or is still running", () => {
  it("says the check is over a minute old, and Launch asks the server again instead of launching", async () => {
    checks.current = answered(PREFLIGHT_FRESH_MS + 1);
    const onLaunch = vi.fn();
    open(AGENT, onLaunch);
    const button = await ready();
    expect(blockerList()).toHaveTextContent(NR_REVIEW.BLOCKER_EXPIRED);
    expect(blockerList()).toHaveTextContent(NR_REVIEW.BLOCKERS_ONE);
    expect(blockerList()).not.toHaveTextContent(NR_REVIEW.BLOCKER_STALE);
    expect(button).toBeEnabled();
    fireEvent.click(button);
    expect(preflight).toHaveBeenCalledTimes(1);
    expect(onLaunch).not.toHaveBeenCalled();
  });

  it("still says the setup changed when the answer is for another body", async () => {
    checks.current = answered(1000, { generation: 6 });
    open(AGENT);
    const button = await ready();
    expect(blockerList()).toHaveTextContent(NR_REVIEW.BLOCKER_STALE);
    expect(button).toBeDisabled();
  });

  it("says it is checking, with no blocker count, while the first check runs", async () => {
    checks.current = answered(0, { preflightRead: null, preflightResult: null, preflighting: true });
    open(AGENT);
    await ready();
    expect(blockerList()).toHaveTextContent(NR_REVIEW.CHECKING);
    expect(blockerList()).not.toHaveTextContent(NR_REVIEW.BLOCKERS_ONE);
    expect(blockerList()).not.toHaveTextContent(NR_REVIEW.BLOCKER_STALE);
  });
});
