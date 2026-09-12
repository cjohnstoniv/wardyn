/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Agent providers (0.7.2) — the two operatorOnly routes behind the Agents tab
// on /providers. Mirrors internal/api/agent_providers.go +
// internal/types/agent_provider.go; every route is under /api/v1 via wfetch.
//
// The `providers.ts` twin: same GET/PUT-with-ETag shape as
// lib/api/providers.ts's workspace-providers client, over a different
// singleton document. The wire types are lib/types/site.ts's
// AgentProviders/AgentProvider (C3's mirror, shared with
// SiteConfig.agent_providers) — imported, not re-declared here.
import type { AgentProvider, AgentProviders } from "../types/site";
import { asJson, wfetch } from "./core";

export type { AgentProvider, AgentProviders };

// GET's response, with the ETag the console keeps and sends back as If-Match
// on every PUT — never a silent overwrite. A never-configured install answers
// the zero-value document with 200, which is legacy open mode, not an error.
export interface AgentProvidersSnapshot {
  providers: AgentProviders;
  etag: string | null;
}

export const agentProviders = {
  // GET /api/v1/agent-providers — operatorOnly, like GET /workspace-providers.
  async getAgentProviders(): Promise<AgentProvidersSnapshot> {
    const res = await wfetch("/agent-providers");
    const body = await asJson<AgentProviders>(res);
    return { providers: body, etag: res.headers.get("ETag") };
  },

  // PUT /api/v1/agent-providers — replaces the WHOLE document; there is no
  // delete route, removing a row is a PUT without it. `etag` is the last GET's
  // (or the last successful PUT's) — sent as If-Match so a stale write is
  // refused 412 rather than silently clobbering someone else's save. Absent
  // (null) behaves as last-writer-wins, exactly like PUT /workspace-providers.
  async putAgentProviders(next: AgentProviders, etag: string | null): Promise<AgentProvidersSnapshot> {
    const headers: Record<string, string> = {};
    if (etag) headers["If-Match"] = etag;
    const res = await wfetch("/agent-providers", {
      method: "PUT",
      headers,
      body: JSON.stringify(next),
    });
    const body = await asJson<AgentProviders>(res);
    return { providers: body, etag: res.headers.get("ETag") };
  },
};
