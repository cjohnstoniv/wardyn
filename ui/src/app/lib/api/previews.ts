/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A run's preview URLs (internal/api/run_previews.go): list, create or change, revoke, and the ticket that opens one.
import { asJson, asNoContent, wfetch } from "./core";
import type { PreviewWrite, RunPreview } from "../types/previews";

const base = (runId: string, port?: number) =>
  `/runs/${encodeURIComponent(runId)}/previews${port === undefined ? "" : `/${port}`}`;

export const previews = {
  // GET /api/v1/runs/{id}/previews — the run's live previews, for its owner or a security admin.
  async list(runId: string): Promise<RunPreview[]> {
    const res = await wfetch(base(runId), { method: "GET" });
    return (await asJson<{ previews: RunPreview[] }>(res)).previews ?? [];
  },

  // PUT /api/v1/runs/{id}/previews/{port} — creates the preview or changes it (the same visibility extends it).
  async put(runId: string, port: number, body: PreviewWrite): Promise<RunPreview> {
    const res = await wfetch(base(runId, port), { method: "PUT", asPerson: true, body: JSON.stringify(body) });
    return (await asJson<{ preview: RunPreview }>(res)).preview;
  },

  // DELETE /api/v1/runs/{id}/previews/{port} — revokes it; a port with no live preview answers the same 204.
  async revoke(runId: string, port: number): Promise<void> {
    await asNoContent(await wfetch(base(runId, port), { method: "DELETE" }));
  },

  // POST /api/v1/runs/{id}/previews/{port}/ticket — the single-use ticket that opens one in a browser (the attach ticket's
  // answer). A person the preview does not admit, and one that has ended, get the same 404 run_not_found.
  // `person`, when given: the ticket follows a read and names the person the open began under.
  async ticket(runId: string, port: number, person?: string): Promise<string> {
    const res = await wfetch(`${base(runId, port)}/ticket`, { method: "POST", asPerson: person });
    return (await asJson<{ ticket: string }>(res)).ticket;
  },
};
