/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Runner pools and remote-hosted runners (internal/api/runner_pools.go, remote_runners.go): the pool
// list, a person's own user pools and their members, the administrators' organization pools, use
// policies and the organization default, and the remote-hosted runners this server is set up with. The
// server answers every route 501 runner_pools_unavailable while it has no pool storage, and the console
// shows that sentence, never an empty list. A covered write a second person must approve answers 202,
// which asJson throws as PendingChangeError.
import { asJson, asNoContent, throwIfPending, wfetch } from "./core";
import type {
  CreateMyRunnerPoolRequest,
  RemoteRunner,
  RemoteRunnerPeoplePolicyRequest,
  RemoteRunnerView,
  RunnerPool,
  RunnerPoolDefaults,
  RunnerPoolList,
  RunnerPoolMember,
  RunnerPoolMemberView,
  RunnerPoolSubject,
  RunnerPoolUsePolicy,
  UpdateRemoteRunnerRequest,
  UpdateRunnerPoolRequest,
} from "../types/runner-pools";

const JSON_HEADERS = { "Content-Type": "application/json" };
const seg = encodeURIComponent;

async function send<T>(path: string, init: RequestInit): Promise<T> {
  return asJson<T>(await wfetch(path, init));
}

const get = <T>(path: string) => send<T>(path, { method: "GET" });
const put = <T>(path: string, body?: unknown) =>
  send<T>(path, body === undefined ? { method: "PUT" } : { method: "PUT", headers: JSON_HEADERS, body: JSON.stringify(body) });

async function sendNoBody(path: string, method: string): Promise<void> {
  return asNoContent(await wfetch(path, { method }));
}

// A covered clear: held for a second person it answers 202, which must not pass for done.
async function clearCovered(path: string): Promise<void> {
  const res = await wfetch(path, { method: "DELETE" });
  await throwIfPending(res);
  return asNoContent(res);
}

const pool = (id: string) => `/runner-pools/${seg(id)}`;
const myPool = (id: string) => `/me/runner-pools/${seg(id)}`;
const memberPath = (poolId: string, runnerId: string) => `${myPool(poolId)}/runners/${seg(runnerId)}`;
const remoteMemberPath = (poolId: string, rid: string) => `${myPool(poolId)}/remote-runners/${seg(rid)}`;
const remote = (rid: string) => `/remote-runners/${seg(rid)}`;

export const runnerPools = {
  list: () => get<RunnerPoolList>("/runner-pools"),
  /** The pools the caller may use: for an administrator, without the oversight rows of other people's user pools. */
  listMine: () => get<RunnerPoolList>("/me/runner-pools"),
  get: (id: string) => get<RunnerPool>(pool(id)),
  /** A pool's runners with their limits and usage: a pool the caller may use, or any pool for an administrator. */
  members: (id: string) => get<RunnerPoolMemberView[]>(`${pool(id)}/members`),
  /** Organization pools (super admin). Creating one is the command line's. */
  update: (id: string, req: UpdateRunnerPoolRequest) => put<RunnerPool>(pool(id), req),
  remove: (id: string) => sendNoBody(pool(id), "DELETE"),
  usePolicy: (id: string) => get<RunnerPoolUsePolicy>(`${pool(id)}/use-policy`),
  setUsePolicy: (id: string, revision: number, subjects: RunnerPoolSubject[]) => put<RunnerPoolUsePolicy>(`${pool(id)}/use-policy`, { revision, subjects }),
  clearUsePolicy: (id: string) => clearCovered(`${pool(id)}/use-policy`),
  /** A person's own user pools. */
  createMine: (req: CreateMyRunnerPoolRequest) => send<RunnerPool>("/me/runner-pools", { method: "POST", headers: JSON_HEADERS, body: JSON.stringify(req) }),
  updateMine: (id: string, req: UpdateRunnerPoolRequest) => put<RunnerPool>(myPool(id), req),
  removeMine: (id: string) => sendNoBody(myPool(id), "DELETE"),
  /** Puts the caller's own claimed runner into one of their user pools; nobody adds another person's. */
  addMyRunner: (poolId: string, runnerId: string) => put<RunnerPoolMember>(memberPath(poolId, runnerId)),
  removeMyRunner: (poolId: string, runnerId: string) => sendNoBody(memberPath(poolId, runnerId), "DELETE"),
  /** A remote-hosted runner an admin made available to people's pools, into one of the caller's user pools. */
  addMyRemoteRunner: (poolId: string, rid: string) => put<RunnerPoolMember>(remoteMemberPath(poolId, rid)),
  removeMyRemoteRunner: (poolId: string, rid: string) => sendNoBody(remoteMemberPath(poolId, rid), "DELETE"),
  orgDefaults: () => get<RunnerPoolDefaults>("/runner-pool-defaults"),
  /** Replaces the organization's defaults (super admin): the caller sends back the barrier it read. */
  setOrgDefaults: (d: RunnerPoolDefaults) => put<RunnerPoolDefaults>("/runner-pool-defaults", d),
  myDefaults: () => get<RunnerPoolDefaults>("/me/runner-pool-defaults"),
  /** Replaces the caller's own defaults. A default only seeds a choice; it grants no pool. */
  setMyDefaults: (d: RunnerPoolDefaults) => put<RunnerPoolDefaults>("/me/runner-pool-defaults", d),
  clearMyDefaults: () => sendNoBody("/me/runner-pool-defaults", "DELETE"),
};

export const remoteRunners = {
  /** Every remote-hosted runner this server is set up with, and every pool each is in (administrators). */
  list: () => get<RemoteRunnerView[]>("/remote-runners"),
  get: (rid: string) => get<RemoteRunnerView>(remote(rid)),
  /** Its name or limits (super admin). */
  update: (rid: string, req: UpdateRemoteRunnerRequest) => put<RemoteRunnerView>(remote(rid), req),
  /** Who may add it to their own user pools (administrators). Off is the clear. */
  setPeoplePolicy: (rid: string, req: RemoteRunnerPeoplePolicyRequest) => put<RemoteRunner>(`${remote(rid)}/people-policy`, req),
  clearPeoplePolicy: (rid: string) => clearCovered(`${remote(rid)}/people-policy`),
  /** The remote-hosted runners the caller may add to their own user pools. */
  listMine: () => get<RemoteRunnerView[]>("/me/remote-runners"),
};
