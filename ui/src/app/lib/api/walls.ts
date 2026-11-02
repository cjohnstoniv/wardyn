/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Your walls: a person's own limits on every run they start, read and written at /me/walls
// (internal/api/personal_walls.go; the shape is internal/types/personal_walls.go). One private
// document per person, replaced whole on a write. Every value only narrows what the organisation
// allows, so the types admit no wider one. The admin token, local mode and a service identity are no
// person: GET answers 400 walls_need_person, and a store that keeps no personal settings 501
// walls_unavailable; the card is absent for both.
import type { ConfinementClass, DesktopControls, FirstUseMode, GrantSpec } from "../types";
import { asJson, asNoContent, HttpError, wfetch } from "./core";

/** A credential kept out of every run: a grant of this kind (scope-less) or of this kind and scope. */
export type WithheldGrant = Pick<GrantSpec, "kind"> & Partial<GrantSpec>;

/** Mirrors Go's types.PersonalWallsSpec. An absent knob is no limit. */
export interface PersonalWallsSpec {
  /** "Only these sites"; [] is only what the run's own setup needs. */
  allowed_domains?: string[];
  denied_domains?: string[];
  /** false only. */
  allow_all_egress?: false;
  first_use_approval?: FirstUseMode;
  min_confinement_class?: ConfinementClass;
  /** "blocked" only. */
  desktop?: DesktopControls;
  withheld_grants?: WithheldGrant[];
}

/** Mirrors Go's types.PersonalWalls. */
export interface PersonalWalls {
  spec: PersonalWallsSpec;
  updated_at?: string;
}

/** The two refusals that mean this caller has no walls to show: no person, or a store that keeps none. */
export const wallsAbsent = (e: unknown): boolean =>
  e instanceof HttpError && (e.reason === "walls_need_person" || e.reason === "walls_unavailable");

export const walls = {
  // GET /api/v1/me/walls: {spec, updated_at?}, {spec: {}} when none are set.
  async get(): Promise<PersonalWalls> {
    return asJson<PersonalWalls>(await wfetch("/me/walls", { method: "GET" }));
  },

  // PUT /api/v1/me/walls: replaces the whole document; an empty spec is no walls. 400 walls_invalid
  // names the first path that is wrong.
  async put(spec: PersonalWallsSpec): Promise<PersonalWalls> {
    return asJson<PersonalWalls>(await wfetch("/me/walls", { method: "PUT", asPerson: true, body: JSON.stringify({ spec }) }));
  },

  // DELETE /api/v1/me/walls -> 204: the stored document becomes {}.
  async clear(): Promise<void> {
    await asNoContent(await wfetch("/me/walls", { method: "DELETE", asPerson: true }));
  },
};
