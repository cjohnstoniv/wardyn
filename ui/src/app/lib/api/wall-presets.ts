/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Wall presets: named walls a person picks at launch, at /wall-presets (internal/api/wall_presets.go). The
// built-ins come first in the list and are read-only. A write is the signed-in person's own (asPerson), and the
// run templates' authority decides where it may be published (the list's `publish`). A preset the caller cannot
// read answers as a missing one: 404 wall_preset_not_found.
import type { WallPreset, WallPresetList, WallPresetSaveRequest } from "../types/wall-presets";
import { asJson, asNoContent, HttpError, wfetch } from "./core";

/** The wire reasons (internal/api/reasons.go). The console matches on these, never on the text. */
export const WALL_PRESET_REASON = {
  INVALID: "wall_preset_invalid",
  UNKNOWN: "wall_preset_unknown",
  NAME_TAKEN: "wall_preset_name_taken",
  VERSION_CONFLICT: "wall_preset_version_conflict",
  UNAVAILABLE: "wall_presets_unavailable",
  HOST: "wall_preset_host",
} as const;

/** A store that keeps no wall presets: the card is absent, as Your walls is for no walls. */
export const wallPresetsAbsent = (e: unknown): boolean => e instanceof HttpError && e.reason === WALL_PRESET_REASON.UNAVAILABLE;

export const wallPresets = {
  // GET /api/v1/wall-presets: {presets, publish}.
  async list(): Promise<WallPresetList> {
    return asJson<WallPresetList>(await wfetch("/wall-presets", { method: "GET" }));
  },

  // POST /api/v1/wall-presets -> 201: version 1.
  async create(req: WallPresetSaveRequest): Promise<WallPreset> {
    return asJson<WallPreset>(await wfetch("/wall-presets", { method: "POST", asPerson: true, body: JSON.stringify(req) }));
  },

  // PUT /api/v1/wall-presets/{id}: the scope never changes; expected_version is the version the caller read.
  async update(id: string, req: WallPresetSaveRequest): Promise<WallPreset> {
    return asJson<WallPreset>(await wfetch(`/wall-presets/${encodeURIComponent(id)}`, { method: "PUT", asPerson: true, body: JSON.stringify(req) }));
  },

  // DELETE /api/v1/wall-presets/{id} -> 204. Runs already launched with it keep it.
  async remove(id: string): Promise<void> {
    await asNoContent(await wfetch(`/wall-presets/${encodeURIComponent(id)}`, { method: "DELETE", asPerson: true }));
  },
};
