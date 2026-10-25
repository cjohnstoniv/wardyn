/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The offer the New Run steps read comes from the chosen pool's `macos_vm` on GET /runner-pools (mock M-N1).
import * as React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { SCENARIO_POOLS } from "../../../../../lib/new-run-fixtures/scenarios/pools";
import type { PoolMacOSVM, RunnerPoolList } from "../../../../../lib/types/runner-pools";
import type { SetupStatus } from "../../../../../lib/types/setup";
import { ModelAccessProvider } from "../../../../wardyn/model-access-context";
import type { RunIntent } from "../../draft/run-intent";
import { NewRunDraftProvider, useNewRunDispatch } from "../../new-run-route";
import { useMacOSOffer } from "./macos-offer";
import { MacOSOfferProvider } from "./macos-offer-provider";

afterEach(cleanup);

const MEMBER = SCENARIO_POOLS.member;
const POOL = MEMBER.pools[0];
const list = (macos_vm?: PoolMacOSVM): RunnerPoolList => ({ ...MEMBER, pools: [{ ...POOL, macos_vm }, ...MEMBER.pools.slice(1)] });
const ON = { features: { macos_vms: true } } as SetupStatus;

const Offer = () => <p data-testid="offer">{JSON.stringify(useMacOSOffer() ?? null)}</p>;
function Seeded({ intent, children }: { intent: RunIntent; children: React.ReactNode }) {
  const dispatch = useNewRunDispatch();
  const [ready, setReady] = React.useState(false);
  React.useEffect(() => {
    dispatch({ type: "edit", apply: () => intent });
    setReady(true);
  }, [dispatch, intent]);
  return ready ? children : null;
}
const INTENT: RunIntent = { shape: "agent_task", experience: "interactive", machine_kind: "macos_vm", runner_pool_id: POOL.id };

function show(load: () => Promise<RunnerPoolList>, status: SetupStatus | null = ON, intent: RunIntent = INTENT) {
  render(
    <ModelAccessProvider status={status} onRefresh={() => {}}>
      <NewRunDraftProvider owner="alice">
        <Seeded intent={intent}>
          <MacOSOfferProvider load={load}>
            <Offer />
          </MacOSOfferProvider>
        </Seeded>
      </NewRunDraftProvider>
    </ModelAccessProvider>,
  );
}
const offer = () => JSON.parse(screen.getByTestId("offer").textContent ?? "null");

describe("MacOSOfferProvider", () => {
  it("offered: the chosen pool's guest, minimum and harnesses", async () => {
    show(vi.fn(async () => list({ offered: true, guest_os: "macOS 27.0.1 (26A434)", min_cpus: 4, min_memory_mib: 8192, harnesses: ["claude-code", "none"], runner: "mac-studio-07" })));
    await waitFor(() => expect(offer()).toMatchObject({ ok: true, guestOS: "macOS 27.0.1 (26A434)", minCPUs: 4, minMemoryMiB: 8192, runner: "mac-studio-07" }));
  });
  it("refused: the pool's refusal", async () => {
    show(vi.fn(async () => list({ offered: false, runner: "mac-studio-07", refusals: [{ runner: "mac-studio-07", why: "preparing", progress: 42, detail: "" }] })));
    await waitFor(() => expect(offer()).toEqual({ ok: false, refusal: { why: "preparing", runner: "mac-studio-07", percent: 42 } }));
  });
  it("no offer when the pool says nothing, so today's behaviour stands", async () => {
    const load = vi.fn(async () => list(undefined));
    show(load);
    await waitFor(() => expect(load).toHaveBeenCalled());
    expect(offer()).toBeNull();
  });
  it("no offer when no pool is chosen", async () => {
    const load = vi.fn(async () => ({ ...list({ offered: true }), default: undefined }));
    show(load, ON, { ...INTENT, runner_pool_id: undefined });
    await waitFor(() => expect(load).toHaveBeenCalled());
    expect(offer()).toBeNull();
  });
  it("asks nothing while the organisation's macOS switch is off", () => {
    const load = vi.fn(async () => list({ offered: true }));
    show(load, null);
    expect(load).not.toHaveBeenCalled();
    expect(offer()).toBeNull();
  });
});
