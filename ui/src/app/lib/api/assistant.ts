/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The assistant routes (internal/api/assistant_settings.go, assistant_availability.go): the organisation's
// settings, which a super admin reads and replaces, and a person's own answer to whether they can use the
// assistant for one purpose. A refusal throws the HttpError of core.ts, so the console shows the server's own
// sentence and can match on `reason`.
import type { AssistantSettings } from "../types/site";
import { asJson, PendingChangeError, wfetch } from "./core";

export type AssistantPurpose = "compose" | "review";

// The states of GET /me/assistant (types.Assistant* in internal/types/assistant.go, plus at_run_limit).
export type AssistantState = "ready" | "off" | "no_provider" | "unit_denies" | "not_connected" | "at_run_limit";

// availabilityAnswer in internal/api/assistant_availability.go, the json tags verbatim.
export interface AssistantAvailability {
  readonly state: AssistantState;
  // The wire reason of a state that is not ready.
  readonly reason?: string;
  // The provider that serves the person or, when none is connected, the one to connect.
  readonly provider_id?: string;
  readonly provider_name?: string;
  // Set when ready.
  readonly provider_kind?: string;
  readonly model?: string;
  readonly never_sent_kinds?: readonly string[];
}

/** The block with the ETag the response carried, which the next PUT sends back as If-Match. */
export interface AssistantSettingsSnapshot {
  settings: AssistantSettings;
  etag: string | null;
}

/** A 200 applied the block; a 202 held it for approval, under the change `changeId`. */
export type AssistantSaveResult = { applied: AssistantSettingsSnapshot } | { held: true; changeId: string };

export const assistant = {
  // GET /api/v1/assistant/settings (super admin).
  async getAssistantSettings(): Promise<AssistantSettingsSnapshot> {
    const res = await wfetch("/assistant/settings", { method: "GET" });
    return { settings: await asJson<AssistantSettings>(res), etag: res.headers.get("ETag") };
  },

  // PUT /api/v1/assistant/settings (super admin): the whole block, replaced. 200 -> applied, 202 -> held, 400
  // -> HttpError carrying the server's sentence, 412 -> the same, for a stale `etag`, 428 -> no `etag` sent.
  async putAssistantSettings(body: AssistantSettings, etag: string | null): Promise<AssistantSaveResult> {
    try {
      const res = await wfetch("/assistant/settings", {
        method: "PUT",
        asPerson: true,
        headers: etag ? { "If-Match": etag } : {},
        body: JSON.stringify(body),
      });
      return { applied: { settings: await asJson<AssistantSettings>(res), etag: res.headers.get("ETag") } };
    } catch (e) {
      if (e instanceof PendingChangeError) return { held: true, changeId: e.change.id };
      throw e;
    }
  },

  // GET /api/v1/me/assistant?purpose=: any signed-in person.
  getMyAssistant: (purpose: AssistantPurpose): Promise<AssistantAvailability> =>
    wfetch(`/me/assistant?purpose=${purpose}`, { method: "GET" }).then((r) => asJson<AssistantAvailability>(r)),
};
