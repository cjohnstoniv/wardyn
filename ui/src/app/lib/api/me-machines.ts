/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// GET /api/v1/me/machines and DELETE /api/v1/me/machines/{id}: the caller's own kept macOS VM machines. Mirrors
// pkg/client/machines.go (wire-parity.test.ts pins the keys). The record only, never the machine's content.
import type { KeptMachine, MyMachines } from "../types/kept-machine";
import { asJson, errText, HttpError, wfetch } from "./core";

/** Reads the caller's kept machines and whether they may keep another. */
export async function myMachines(): Promise<MyMachines> {
  const body = await asJson<MyMachines>(await wfetch("/me/machines", { method: "GET" }));
  return { ...body, machines: body.machines ?? [] };
}

/**
 * Deletes one of the caller's kept machines from its Mac. 204 is a confirmed delete, so null; 202 is a delete waiting
 * for its Mac to come back, and answers the machine, now being deleted. A refusal (409 while a run holds it, 503 when the
 * Mac's answer is unknown or it refused) throws an HttpError carrying the server's sentence and reason.
 */
export async function deleteMyMachine(id: string): Promise<KeptMachine | null> {
  const res = await wfetch(`/me/machines/${encodeURIComponent(id)}`, { method: "DELETE", asPerson: true });
  if (!res.ok) throw new HttpError(res.status, await errText(res));
  return res.status === 204 ? null : asJson<KeptMachine>(res);
}
