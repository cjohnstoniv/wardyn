/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run page's wall preset sentences (boards WP-3), as plain components over the words they need: the policy tab's
// line, the activity line under a deny-all refusal, the queued run's version line and the approval row's sentence.
import type { RunWallPreset } from "../../../lib/types/wall-presets";
import { WALL_PRESETS as W } from "../../wardyn/copy/wall-presets";

/** The policy tab's line under the source line. A person preset seen by anyone but its owner is named by no one. */
export function WallPresetRunLine({ name, audience, version }: { name?: string; audience?: string; version?: number }) {
  const text = name && audience && version !== undefined ? W.RUN_LINE(name, audience, version) : W.RUN_LINE_PERSONAL;
  return <p data-testid="wall-preset-run-line" className="text-xs text-muted-foreground">{text}</p>;
}

/** The run activity line under a deny-all refusal, when the run's preset is what sets it. */
export function WallPresetRefusalLine({ name, className = "" }: { name: string; className?: string }) {
  return <span data-testid="wall-preset-refusal-line" className={`text-xs text-muted-foreground ${className}`}>{W.PUSH_REFUSED_BY(name)}</span>;
}

/**
 * The muted line under a queued run's end sentence when its wall preset changed or went before the run could start.
 * The run read's wall_preset carries the version the run was queued under and, once that ended it, what the preset
 * became (changed). A personal preset read by anyone but its owner has no name or version, so it says nothing.
 */
export function WallPresetQueuedChanged({ preset, className = "" }: { preset: RunWallPreset | undefined; className?: string }) {
  const { name, version, changed } = preset ?? {};
  if (!name || version === undefined || !changed) return null;
  const text = changed.gone ? W.QUEUED_GONE(name, version) : changed.version !== undefined && changed.version !== version ? W.QUEUED_CHANGED(name, version, changed.version) : undefined;
  return text ? <p data-testid="wall-preset-queued-changed" className={`text-xs text-muted-foreground ${className}`}>{text}</p> : null;
}

/** An approval row's sentence when the run's wall preset denies the host: the owner's own run, or somebody else's. */
export function wallPresetApprovalSentence(o: { name: string; host: string; ownRun: boolean; owner: string }) {
  return o.ownRun ? [W.APPROVAL_OWN(o.name), o.host, W.APPROVAL_OWN_AFTER] : [W.APPROVAL_OTHER(o.owner, o.name), o.host, W.APPROVAL_OTHER_AFTER];
}
