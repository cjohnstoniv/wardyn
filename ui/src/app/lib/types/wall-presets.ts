/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The wall preset wire contract (types only). Every interface mirrors one Go struct (internal/types/wall_preset.go,
// pkg/client/wall_presets.go) and wall-presets.wire.test.ts pins its json tags to that source. A server whose store
// keeps no wall presets answers 501 wall_presets_unavailable.
import type { PersonalWallsSpec } from "../api/walls";
import type { TemplatePublish } from "./templates";

/** Who a preset is for: built in, one person, a group or the organisation. */
export type WallPresetScope = "builtin" | "person" | "group" | "org";

/** Mirrors Go's types.WallPreset. */
export interface WallPreset {
  /** A uuid, or the built-in's name: open, read-only or sealed. */
  id: string;
  scope: WallPresetScope;
  group_id?: string;
  name: string;
  description?: string;
  spec: PersonalWallsSpec;
  version: number;
  updated_at?: string;
  /** Org and group scope only; a person preset never sends it. */
  updated_by?: string;
}

/** Mirrors Go's client.WallPresetList: the built-ins first, then every preset the caller may read. */
export interface WallPresetList {
  presets: WallPreset[];
  publish: TemplatePublish;
}

/** Mirrors Go's client.WallPresetSaveRequest: a POST names the scope, a PUT the version it read. */
export interface WallPresetSaveRequest {
  scope?: WallPresetScope;
  group_id?: string;
  name: string;
  description?: string;
  spec: PersonalWallsSpec;
  expected_version?: number;
}

/**
 * Mirrors Go's client.RunWallPreset: the run read's wall_preset (GET /runs/{id}), never the spec. A personal preset
 * read by anyone but the run's owner carries its scope alone: no id, name or version.
 */
export interface RunWallPreset {
  /** What the preset became while the run waited, once that ended it queue_policy_changed. */
  changed?: WallPresetChange;
  id?: string;
  name?: string;
  scope: WallPresetScope;
  group_id?: string;
  /** A group preset's group label, when the group has one. */
  group_label?: string;
  version?: number;
}

/** Mirrors Go's types.WallPresetChange: a wall preset as a queued run's start found it, at a version or gone. */
export interface WallPresetChange {
  version?: number;
  gone?: boolean;
}
