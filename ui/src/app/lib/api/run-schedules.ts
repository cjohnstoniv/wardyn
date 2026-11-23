/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// /api/v1/run-schedules: a person's repeating schedules (internal/api/run_schedule_repeat.go). pkg/client has no
// methods for them yet. A refusal throws an HttpError carrying the server's sentence and reason.
import type { ScheduleEvery } from "../schedule";
import { asJson, asNoContent, wfetch } from "./core";

/** A schedule as the server answers it (runScheduleView). It never carries the stored request. */
export interface RunSchedule {
  id: string;
  name: string;
  every: ScheduleEvery;
  /** 0 is Sunday; weekly schedules only. */
  weekday?: number;
  time: string;
  time_zone: string;
  next_at: string;
  paused?: { at: string; reason: string };
  last?: { due_at: string; run_id?: string; outcome: string };
  /** The owner's principal, in the admin view only. */
  owner?: string;
}

export interface NewRunSchedule {
  name: string;
  every: ScheduleEvery;
  time: string;
  time_zone: string;
  /** A POST /runs body without start_at or start_time_zone. */
  request: Record<string, unknown>;
}

const path = (id: string, verb = "") => `/run-schedules/${encodeURIComponent(id)}${verb ? `/${verb}` : ""}`;

export const runSchedules = {
  /** The caller's own schedules, or everyone's in an operator's admin view. */
  list: async (admin = false): Promise<RunSchedule[]> =>
    (await asJson<RunSchedule[] | null>(await wfetch(admin ? "/run-schedules?view=admin" : "/run-schedules", { method: "GET" }))) ?? [],
  create: async (body: NewRunSchedule): Promise<RunSchedule> =>
    asJson<RunSchedule>(await wfetch("/run-schedules", { method: "POST", body: JSON.stringify(body), asPerson: true })),
  pause: async (id: string): Promise<RunSchedule> => asJson<RunSchedule>(await wfetch(path(id, "pause"), { method: "POST" })),
  resume: async (id: string): Promise<RunSchedule> =>
    asJson<RunSchedule>(await wfetch(path(id, "resume"), { method: "POST", asPerson: true })),
  remove: async (id: string): Promise<void> => asNoContent(await wfetch(path(id), { method: "DELETE" })),
};
