/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Per-user run-cockpit widget layout (GET/PUT /api/v1/me/run-layout). The row
// is scoped SERVER-side by the caller's principal alone — there is no
// principal in the body and no run id anywhere: a layout belongs to the human,
// not to a run, which is why it is a server row rather than lib/storage.ts
// (see internal/api/ui_layout.go's package doc).
import { asJson, wfetch } from "./core";

// The server's closed preset set (runLayoutPresets). A third preset is a 400.
export type RunLayoutPreset = "live" | "finished";

// One placed widget. `widget` is validated server-side against a CLOSED id set
// (runLayoutWidgetIDs) — the console half of that contract is
// components/screens/run-detail/widget-registry.ts, and the two are kept in
// sync by hand.
export type RunLayoutWidget = {
  widget: string;
  x: number;
  y: number;
  w: number;
  h: number;
};

export type RunLayoutResponse = {
  preset: RunLayoutPreset;
  layout: RunLayoutWidget[];
  /** ABSENT means this principal has never saved this preset — that is a
   *  normal 200, not a failure (the server never 404s this route). */
  updated_at?: string;
};

export const runLayout = {
  // GET /api/v1/me/run-layout?preset=live|finished
  // No saved row => 200 with `layout: []` and no updated_at, the same shape a
  // deployment whose store cannot persist layouts returns. Callers treat an
  // empty layout as "fall through to the situational default", never as an
  // error.
  async getLayout(preset: RunLayoutPreset): Promise<RunLayoutResponse> {
    const res = await wfetch(`/me/run-layout?preset=${encodeURIComponent(preset)}`, {
      method: "GET",
    });
    return asJson<RunLayoutResponse>(res);
  },

  // PUT /api/v1/me/run-layout  { preset, layout }
  // Throws HttpError(501) when this deployment's store cannot persist a layout
  // (the memory store) — the caller degrades to in-session-only rather than
  // reporting a failure, since nothing the human did was wrong. 400 on an
  // unknown widget id or an out-of-range position. `layout: []` is the RESET:
  // it clears the saved row's contents so the next GET falls through to the
  // situational default (there is no DELETE route).
  async putLayout(preset: RunLayoutPreset, layout: RunLayoutWidget[]): Promise<RunLayoutResponse> {
    const res = await wfetch("/me/run-layout", {
      method: "PUT",
      body: JSON.stringify({ preset, layout }),
    });
    return asJson<RunLayoutResponse>(res);
  },
};
