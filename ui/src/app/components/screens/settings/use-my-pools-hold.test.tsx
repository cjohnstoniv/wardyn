/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// My pools keeps today's states through TanStack Query: a failure stays on screen while a reload is pending, a failed
// reload keeps the pools, a disabled card reads nothing, and a change of identity re-reads under the same key.
import * as React from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { HttpError } from "../../../lib/api/core";
import type { RunnerPool, RunnerPoolCatalogueEntry } from "../../../lib/types/runner-pools";

const listMock = vi.fn();
const getMock = vi.fn();
const membersMock = vi.fn();
const remoteMineMock = vi.fn();
vi.mock("../../../lib/api/runner-pools", () => ({
  runnerPools: {
    list: (...a: unknown[]) => listMock(...a),
    get: (...a: unknown[]) => getMock(...a),
    members: (...a: unknown[]) => membersMock(...a),
  },
  remoteRunners: { listMine: (...a: unknown[]) => remoteMineMock(...a) },
}));

import { OperatorProvider } from "../../wardyn/operator-context";
import { useMyPools } from "./use-my-pools";

const entry = (id: string, owner: string) => ({ id, name: id, scope: "user", owner }) as unknown as RunnerPoolCatalogueEntry;
const BY_ID: Record<string, RunnerPool> = {
  "p-a": { id: "p-a", name: "p-a", scope: "user", owner: "alice" } as unknown as RunnerPool,
  "p-b": { id: "p-b", name: "p-b", scope: "user", owner: "bob" } as unknown as RunnerPool,
};
const BOTH = { pools: [entry("p-a", "alice"), entry("p-b", "bob")] };

let identity = { principal: "alice", securityOperator: false };
const wrapper = ({ children }: { children: React.ReactNode }) => (
  <OperatorProvider operator={false} operatorResolved principal={identity.principal} securityOperator={identity.securityOperator}>
    {children}
  </OperatorProvider>
);
const render = (enabled = true) => renderHook(() => useMyPools(enabled), { wrapper });

const settle = () => act(async () => void (await new Promise((resolve) => setTimeout(resolve, 20))));
const hung = <T,>() => {
  let resolve: (v: T) => void = () => {};
  const promise = new Promise<T>((r) => (resolve = r));
  return { promise, resolve };
};

beforeEach(() => {
  identity = { principal: "alice", securityOperator: false };
  for (const m of [listMock, getMock, membersMock, remoteMineMock]) m.mockReset();
  getMock.mockImplementation(async (id: string) => BY_ID[id]);
  membersMock.mockResolvedValue([]);
  remoteMineMock.mockResolvedValue([]);
});

describe("useMyPools failure hold", () => {
  it("a held 501 stays unavailable while a reload is pending", async () => {
    listMock.mockRejectedValueOnce(new HttpError(501, "no pool storage"));
    const { result } = render();
    await settle();
    expect(result.current.status).toBe("unavailable");
    listMock.mockReturnValueOnce(new Promise(() => {}));
    act(() => result.current.reload());
    await settle();
    expect(listMock).toHaveBeenCalledTimes(2);
    expect(result.current.status).toBe("unavailable");
  });

  it("a held error stays error while a reload is pending", async () => {
    listMock.mockRejectedValueOnce(new HttpError(500, "boom"));
    const { result } = render();
    await settle();
    expect(result.current.status).toBe("error");
    listMock.mockReturnValueOnce(new Promise(() => {}));
    act(() => result.current.reload());
    await settle();
    expect(result.current.status).toBe("error");
  });

  it("a failed reload with pools shown reads error and keeps the pools", async () => {
    listMock.mockResolvedValueOnce(BOTH).mockRejectedValueOnce(new HttpError(500, "boom"));
    const { result } = render();
    await settle();
    expect(result.current.status).toBe("ready");
    expect(result.current.pools).toHaveLength(2);
    act(() => result.current.reload());
    await settle();
    expect(result.current.status).toBe("error");
    expect(result.current.pools).toHaveLength(2);
  });

  it("enabled false reads nothing and stays loading", async () => {
    listMock.mockResolvedValue(BOTH);
    const { result } = render(false);
    await settle();
    expect(listMock).not.toHaveBeenCalled();
    expect(result.current.status).toBe("loading");
  });

  it("a change of identity keeps the pools ready while its read is pending, then shows its answer", async () => {
    listMock.mockResolvedValue(BOTH);
    const { result, rerender } = render();
    await settle();
    expect(result.current.pools).toHaveLength(2);
    const answer = hung<typeof BOTH>();
    listMock.mockReturnValueOnce(answer.promise);
    identity = { principal: "alice", securityOperator: true };
    rerender();
    await settle();
    expect(listMock).toHaveBeenCalledTimes(2);
    expect(result.current.status).toBe("ready");
    expect(result.current.pools).toHaveLength(2);
    await act(async () => answer.resolve(BOTH));
    await settle();
    expect(result.current.status).toBe("ready");
    expect(result.current.pools.map((p) => p.pool.id)).toEqual(["p-a"]);
  });

  it("a read for the old identity still in flight when the identity changes is not the answer", async () => {
    const first = hung<typeof BOTH>();
    listMock.mockReturnValueOnce(first.promise).mockResolvedValueOnce(BOTH);
    const { result, rerender } = render();
    await settle();
    identity = { principal: "alice", securityOperator: true };
    rerender();
    await settle();
    expect(result.current.pools.map((p) => p.pool.id)).toEqual(["p-a"]);
    await act(async () => first.resolve(BOTH));
    await settle();
    expect(result.current.pools.map((p) => p.pool.id)).toEqual(["p-a"]);
  });

  it("the new identity's failed read shows error and keeps the pools", async () => {
    listMock.mockResolvedValueOnce(BOTH).mockRejectedValueOnce(new HttpError(500, "boom"));
    const { result, rerender } = render();
    await settle();
    identity = { principal: "alice", securityOperator: true };
    rerender();
    await settle();
    expect(result.current.status).toBe("error");
    expect(result.current.pools).toHaveLength(2);
  });
});
