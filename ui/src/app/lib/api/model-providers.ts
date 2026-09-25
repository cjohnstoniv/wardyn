/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Model providers (0.8, MP-3) — the operatorOnly GET/PUT /model-providers
// document behind Settings → Model providers. Mirrors
// internal/api/model_providers_api.go + internal/types/model_provider.go;
// every route is under /api/v1 via wfetch.
//
// The `agent-providers.ts` twin: same GET/PUT-with-ETag shape as its roster
// client, over a different singleton document. The wire types are
// lib/types/site.ts's ModelProviders/ModelProvider (shared with
// SiteConfig.model_providers) — imported, not re-declared here.
import type { ModelProvider, ModelProviders, ModelProvidersRead } from "../types/site";
import { asJson, wfetch } from "./core";

export type { ModelProvider, ModelProviders };

// GET's response, with the ETag the console keeps and sends back as If-Match
// on every PUT — never a silent overwrite. A never-configured install answers
// the zero-value document with 200, which is today's legacy-open-mode
// behaviour, not an error.
export interface ModelProvidersSnapshot {
  providers: ModelProviders;
  etag: string | null;
}

// GET's snapshot adds `connected`: per provider id, how many people hold a
// credential of their own for it (MODEL_PROVIDERS.CONNECTED(n), #970). Split
// off `providers` so that document stays safe to PUT back. A PUT's answer
// carries no count — re-read GET after a save that may have purged some.
export interface ModelProvidersList extends ModelProvidersSnapshot {
  connected: Record<string, number>;
}

export const modelProviders = {
  // GET /api/v1/model-providers — operatorOnly, like GET /agent-providers.
  async getModelProviders(): Promise<ModelProvidersList> {
    const res = await wfetch("/model-providers");
    const { connected_people, ...providers } = await asJson<ModelProvidersRead>(res);
    return { providers, connected: connected_people ?? {}, etag: res.headers.get("ETag") };
  },

  // PUT /api/v1/model-providers — replaces the WHOLE document; there is no
  // delete route, removing a row is a PUT without it. `etag` is the last GET's
  // (or the last successful PUT's) — sent as If-Match so a stale write is
  // refused 412 rather than silently clobbering someone else's save. Absent
  // (null) behaves as last-writer-wins, exactly like PUT /agent-providers.
  // A save may also be refused (400) for reasons the body's `error` names
  // verbatim: a still-default provider being removed, a Claude subscription
  // whose sign-in image hasn't resolved yet, or a plain validation failure.
  async putModelProviders(next: ModelProviders, etag: string | null): Promise<ModelProvidersSnapshot> {
    const headers: Record<string, string> = {};
    if (etag) headers["If-Match"] = etag;
    const res = await wfetch("/model-providers", {
      method: "PUT",
      headers,
      body: JSON.stringify(next),
    });
    const body = await asJson<ModelProviders>(res);
    return { providers: body, etag: res.headers.get("ETag") };
  },
};
