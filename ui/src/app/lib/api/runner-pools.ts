/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Runner pools (internal/api/runner_pools.go): the caller's catalogue, their own
// runner membership and their own defaults. Typed stubs: the server answers
// every route 501 runner_pools_unavailable until its pool storage lands, and the
// console must show that sentence, never an empty catalogue. The admin routes
// (pool writes, executor membership, the organisation default, the use policy)
// are authored by the admin lane's own module.
import { asJson, asNoContent, wfetch } from "./core";
import type { RunnerPool, RunnerPoolDefaults, RunnerPoolList, RunnerPoolMember } from "../types/runner-pools";

const JSON_HEADERS = { "Content-Type": "application/json" };

async function send<T>(path: string, init: RequestInit): Promise<T> {
  return asJson<T>(await wfetch(path, init));
}

async function sendNoBody(path: string, method: string): Promise<void> {
  return asNoContent(await wfetch(path, { method }));
}

const memberPath = (poolId: string, runnerId: string) =>
  `/me/runner-pools/${encodeURIComponent(poolId)}/runners/${encodeURIComponent(runnerId)}`;

export const runnerPools = {
  list: () => send<RunnerPoolList>("/runner-pools", { method: "GET" }),
  get: (id: string) => send<RunnerPool>(`/runner-pools/${encodeURIComponent(id)}`, { method: "GET" }),
  /** Puts the caller's own claimed runner into a self-hosted pool; nobody adds another person's. */
  addMyRunner: (poolId: string, runnerId: string) => send<RunnerPoolMember>(memberPath(poolId, runnerId), { method: "PUT" }),
  removeMyRunner: (poolId: string, runnerId: string) => sendNoBody(memberPath(poolId, runnerId), "DELETE"),
  orgDefaults: () => send<RunnerPoolDefaults>("/runner-pool-defaults", { method: "GET" }),
  myDefaults: () => send<RunnerPoolDefaults>("/me/runner-pool-defaults", { method: "GET" }),
  /** Replaces the caller's own defaults. A default only seeds a choice; it grants no pool. */
  setMyDefaults: (d: RunnerPoolDefaults) =>
    send<RunnerPoolDefaults>("/me/runner-pool-defaults", { method: "PUT", headers: JSON_HEADERS, body: JSON.stringify(d) }),
  clearMyDefaults: () => sendNoBody("/me/runner-pool-defaults", "DELETE"),
};
