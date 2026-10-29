/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Workloads (internal/api/workloads.go) and the namespace bindings the security tier keeps
// (workload_bindings.go). A workload is applied from the command line or a custom resource, never here: the
// console reads, restarts and deletes. A binding is standing authority over a service identity, so creating
// one is a signed-in person's alone (asPerson).
import type { CreateWorkloadBindingRequest, Workload, WorkloadApplyResult, WorkloadBinding } from "../types/workloads";
import { asJson, asNoContent, unwrapList, wfetch, withLimit } from "./core";

const seg = encodeURIComponent;

// A workload is keyed by owner and name: an admin reading another owner's names it with ?owner=.
const workloadPath = (name: string, owner?: string) => `/workloads/${seg(name)}${owner ? `?owner=${seg(owner)}` : ""}`;

export const workloads = {
  /** The caller's own workloads and those of the service identities they sponsor; the security tier's reads every owner's. */
  async list(): Promise<Workload[]> {
    return unwrapList<Workload>(await asJson<unknown>(await wfetch(withLimit("/workloads"), { method: "GET" })));
  },

  get: async (name: string, owner?: string): Promise<Workload> => asJson<Workload>(await wfetch(workloadPath(name, owner), { method: "GET" })),

  /** A fresh run of the same spec, then the old one ends; a refused one leaves the old one running. */
  async restart(name: string, owner?: string): Promise<WorkloadApplyResult> {
    return asJson<WorkloadApplyResult>(await wfetch(`/workloads/${seg(name)}/restart${owner ? `?owner=${seg(owner)}` : ""}`, { method: "POST" }));
  },

  /** The row goes, then its run is ended. A custom resource's workload is deleted in Git; the server refuses it here for anyone below the security tier. */
  async remove(name: string, owner?: string): Promise<void> {
    await asNoContent(await wfetch(workloadPath(name, owner), { method: "DELETE" }));
  },
};

export const workloadBindings = {
  async list(): Promise<WorkloadBinding[]> {
    return unwrapList<WorkloadBinding>(await asJson<unknown>(await wfetch(withLimit("/workload-bindings"), { method: "GET" })));
  },

  create: async (req: CreateWorkloadBindingRequest): Promise<WorkloadBinding> =>
    asJson<WorkloadBinding>(await wfetch("/workload-bindings", { method: "POST", asPerson: true, body: JSON.stringify(req) })),

  async remove(id: string): Promise<void> {
    await asNoContent(await wfetch(`/workload-bindings/${seg(id)}`, { method: "DELETE" }));
  },
};
