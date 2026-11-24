/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The assistant routes (internal/api/assistant_settings.go, assistant_availability.go): the organisation's
// settings, which a super admin reads and replaces, and a person's own answer to whether they can use the
// assistant for one purpose. A refusal throws the HttpError of core.ts, so the console shows the server's own
// sentence and can match on `reason`.
import { apiURL } from "../base-path";
import type { GovernanceChange } from "../types";
import type { AssistantSettings } from "../types/site";
import { asJson, asNoContent, PendingChangeError, wfetch } from "./core";

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

// What a session works on (types.AssistantTarget): a new draft names no id.
export interface AssistantTarget {
  readonly kind: "run" | "unit" | "template" | "change";
  readonly id?: string;
}

// One block of the records the first message carries (types.AssistantSection).
export interface AssistantSection {
  readonly label: string;
  readonly text: string;
  readonly untrusted: boolean;
}

// POST /assistant/preview (types.AssistantPreview): the records, byte for byte, with the digest a start must carry.
// `letters` maps each Person letter to the name it stands for; it is for this browser only.
export interface AssistantPreview {
  readonly provider: string;
  readonly model: string;
  readonly sections: readonly AssistantSection[];
  readonly digest: string;
  readonly letters: Readonly<Record<string, string>>;
  readonly never_sent: readonly string[];
  readonly tools: readonly string[];
}

// POST /assistant/sessions (assistantSessionView in internal/api/assistant_session.go).
export interface AssistantSessionView {
  readonly id: string;
  readonly state: string;
  readonly run_id?: string;
  readonly provider: string;
  readonly model: string;
  readonly letters: Readonly<Record<string, string>>;
}

// One row of a session's transcript (assistantEventView); the payload is read by kind.
export interface AssistantEvent {
  readonly seq: number;
  readonly kind: string;
  readonly payload: Record<string, unknown>;
  readonly created_at: string;
}

// The long-poll answer: the events after the cursor, the session's state and its letters.
export interface AssistantEventsPage {
  readonly events: readonly AssistantEvent[];
  readonly state: string;
  readonly letters: Readonly<Record<string, string>>;
}

export interface AssistantSessionRequest {
  readonly purpose: AssistantPurpose;
  readonly target: AssistantTarget;
  readonly preview_digest: string;
}

/** The block with the ETag the response carried, which the next PUT sends back as If-Match. */
export interface AssistantSettingsSnapshot {
  settings: AssistantSettings;
  etag: string | null;
}

/** A 200 applied the block; a 202 held it for approval, and `held` is the change waiting. */
export type AssistantSaveResult = { applied: AssistantSettingsSnapshot } | { held: GovernanceChange };

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
      if (e instanceof PendingChangeError) return { held: e.change };
      throw e;
    }
  },

  // GET /api/v1/me/assistant?purpose=: any signed-in person.
  getMyAssistant: (purpose: AssistantPurpose): Promise<AssistantAvailability> =>
    wfetch(`/me/assistant?purpose=${purpose}`, { method: "GET" }).then((r) => asJson<AssistantAvailability>(r)),

  // POST /api/v1/assistant/preview: what the first message will carry. Starts nothing.
  postPreview: (purpose: AssistantPurpose, target: AssistantTarget): Promise<AssistantPreview> =>
    wfetch("/assistant/preview", { method: "POST", asPerson: true, body: JSON.stringify({ purpose, target }) }).then((r) =>
      asJson<AssistantPreview>(r),
    ),

  // POST /api/v1/assistant/sessions: Start. 409 assistant_preview_stale when what is sent changed since the preview.
  createSession: (body: AssistantSessionRequest): Promise<AssistantSessionView> =>
    wfetch("/assistant/sessions", { method: "POST", asPerson: true, body: JSON.stringify(body) }).then((r) => asJson<AssistantSessionView>(r)),

  // POST /api/v1/assistant/sessions/{id}/messages: the person's turn. The text is sent exactly as typed.
  postMessage: (id: string, text: string): Promise<{ seq: number }> =>
    wfetch(`/assistant/sessions/${encodeURIComponent(id)}/messages`, { method: "POST", asPerson: true, body: JSON.stringify({ text }) }).then((r) =>
      asJson<{ seq: number }>(r),
    ),

  // DELETE /api/v1/assistant/sessions/{id}: end it.
  endSession: (id: string): Promise<void> =>
    wfetch(`/assistant/sessions/${encodeURIComponent(id)}`, { method: "DELETE", asPerson: true }).then(asNoContent),

  // GET .../events?after=N as a server-sent event stream. An EventSource is not made by wfetch, so its URL is built here
  // with apiURL and stays same-origin: the session cookie rides it.
  streamEvents: (id: string, after: number): EventSource =>
    new EventSource(apiURL(`/assistant/sessions/${encodeURIComponent(id)}/events?after=${after}`)),

  // GET .../events?after=N as a long-poll: the fallback when a stream cannot be held. It answers within 25 seconds.
  pollEvents: (id: string, after: number): Promise<AssistantEventsPage> =>
    wfetch(`/assistant/sessions/${encodeURIComponent(id)}/events?after=${after}`, { method: "GET" }).then((r) => asJson<AssistantEventsPage>(r)),
};
