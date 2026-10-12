/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What Start from reads: the template catalogue and the creator's launch
// intents. A failed read throws the server's own sentence (HttpError).

import { asJson, wfetch } from "../../../../../lib/api/core";
import type { LaunchIntent, LaunchIntentList } from "../../../../../lib/new-run-fixtures/scenarios/templates";
import type { Template, TemplateList } from "../../../../../lib/types/templates";

export const templateSource = {
  async list(signal?: AbortSignal): Promise<TemplateList> {
    return asJson<TemplateList>(await wfetch("/templates", { method: "GET", signal }));
  },
  /** A pinned revision, or the current one when the link names none. */
  async read(id: string, revision: number | undefined, signal?: AbortSignal): Promise<Template> {
    const base = `/templates/${encodeURIComponent(id)}`;
    const path = revision === undefined ? base : `${base}/revisions/${revision}`;
    return asJson<Template>(await wfetch(path, { method: "GET", signal }));
  },
};

// Seam: DS2-S4 (launch intents) is not merged. These are its routes as DS-2
// draws them; the wire types are stand-ins until lib/types/launch-intents.ts
// lands, then this import moves there and nothing else changes.
export const launchIntentSource = {
  async list(signal?: AbortSignal): Promise<LaunchIntentList> {
    return asJson<LaunchIntentList>(await wfetch("/me/launch-intents", { method: "GET", signal }));
  },
  async read(runId: string, signal?: AbortSignal): Promise<LaunchIntent> {
    return asJson<LaunchIntent>(await wfetch(`/runs/${encodeURIComponent(runId)}/launch-intent`, { method: "GET", signal }));
  },
};
