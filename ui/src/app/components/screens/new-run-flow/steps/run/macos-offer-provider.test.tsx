/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The offer the New Run steps read comes from the chosen pool's `macos_vm` on GET /runner-pools (mock M-N1).
import * as React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useNavigate } from "react-router-dom";
import { SCENARIO_POOLS } from "../../../../../lib/new-run-fixtures/scenarios/pools";
import type { PoolMacOSVM, RunnerPoolList } from "../../../../../lib/types/runner-pools";
import type { SetupStatus } from "../../../../../lib/types/setup";
import { ModelAccessProvider } from "../../../../wardyn/model-access-context";
import type { RunIntent } from "../../draft/run-intent";
import { NewRunDraftProvider, useNewRunDispatch, useNewRunDraft } from "../../new-run-route";
import { useMacOSOffer } from "./macos-offer";
import { MacOSOfferProvider } from "./macos-offer-provider";

afterEach(cleanup);

const MEMBER = SCENARIO_POOLS.member;
const POOL = MEMBER.pools[0];
const list = (macos_vm?: PoolMacOSVM): RunnerPoolList => ({ ...MEMBER, pools: [{ ...POOL, macos_vm }, ...MEMBER.pools.slice(1)] });
const ON = { features: { macos_vms: true } } as SetupStatus;

// The wizard's own step change: the address moves while every provider above the steps stays mounted.
function NextStep() {
  const navigate = useNavigate();
  return <button onClick={() => void navigate({ search: "?step=harnesses" })}>next</button>;
}
const Machine = () => <p data-testid="machine">{useNewRunDraft().intent.machine_kind ?? ""}</p>;
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
    <MemoryRouter>
      <ModelAccessProvider status={status} onRefresh={() => {}}>
        <NewRunDraftProvider owner="alice">
          <Seeded intent={intent}>
            <MacOSOfferProvider load={load}>
              <Offer />
              <Machine />
              <NextStep />
            </MacOSOfferProvider>
          </Seeded>
        </NewRunDraftProvider>
      </ModelAccessProvider>
    </MemoryRouter>,
  );
}
const offer = () => JSON.parse(screen.getByTestId("offer").textContent ?? "null");

describe("MacOSOfferProvider", () => {
  it("offered: the chosen pool's guest, minimum and harnesses", async () => {
    show(vi.fn(async () => list({ offered: true, guest_os: "macOS 27.0.1 (26A434)", min_cpus: 4, min_memory_mib: 8192, harnesses: ["claude-code", "none"], runner: "mac-studio-07" })));
    await waitFor(() => expect(offer()).toMatchObject({ ok: true, guestOS: "macOS 27.0.1 (26A434)", minCPUs: 4, minMemoryMiB: 8192, runner: "mac-studio-07" }));
  });
  it("offered: the profile's name comes from the same list's allowance, for MAC-B05", async () => {
    show(vi.fn(async () => ({ ...list({ offered: true, harnesses: ["none"], standard_not_allowed: true }), allowance: { active: 0, unit_name: "Platform" } })));
    await waitFor(() => expect(offer()).toMatchObject({ ok: true, standardNotAllowed: true, profile: "Platform" }));
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
  it("asks again on a step change: a pool that was preparing offers macOS once it recovers, while the draft stays", async () => {
    const preparing = list({ offered: false, runner: "mac-studio-07", refusals: [{ runner: "mac-studio-07", why: "preparing", progress: 42, detail: "" }] });
    const ready = list({ offered: true, guest_os: "macOS 27.0.1 (26A434)", min_cpus: 4, min_memory_mib: 8192, harnesses: ["none"], runner: "mac-studio-07" });
    let calls = 0;
    const load = vi.fn(async () => (calls++ === 0 ? preparing : ready));
    show(load);
    await waitFor(() => expect(offer()).toEqual({ ok: false, refusal: { why: "preparing", runner: "mac-studio-07", percent: 42 } }));
    await userEvent.click(screen.getByRole("button", { name: "next" }));
    await waitFor(() => expect(offer()).toMatchObject({ ok: true, minCPUs: 4, minMemoryMiB: 8192 }));
    expect(load).toHaveBeenCalledTimes(2);
    expect(screen.getByTestId("machine").textContent).toBe("macos_vm");
  });
  it("keeps the offer in hand when the ask on a step change fails", async () => {
    const ready = list({ offered: true, guest_os: "macOS 27.0.1 (26A434)", min_cpus: 4, min_memory_mib: 8192, harnesses: ["none"], runner: "mac-studio-07" });
    let calls = 0;
    const load = vi.fn(async () => {
      if (calls++ > 0) throw new Error("runner pools unavailable");
      return ready;
    });
    show(load);
    await waitFor(() => expect(offer()).toMatchObject({ ok: true }));
    await userEvent.click(screen.getByRole("button", { name: "next" }));
    await waitFor(() => expect(load).toHaveBeenCalledTimes(2));
    expect(offer()).toMatchObject({ ok: true });
  });
  it("asks nothing while the organisation's macOS switch is off", () => {
    const load = vi.fn(async () => list({ offered: true }));
    show(load, null);
    expect(load).not.toHaveBeenCalled();
    expect(offer()).toBeNull();
  });
});
