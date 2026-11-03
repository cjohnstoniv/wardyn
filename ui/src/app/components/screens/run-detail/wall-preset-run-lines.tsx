/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run page's wall preset sentences (boards WP-3), as plain components over the words they need. They are not
// mounted yet: the run read's wall_preset, the policy view's wall_preset record and the queue ref's versions arrive
// with the launch-side change, and these take their inputs as props so nothing here names a field of those shapes.
import { WALL_PRESETS as W } from "../../wardyn/copy/wall-presets";

/** The policy tab's line under the source line. A person preset seen by anyone but its owner is named by no one. */
export function WallPresetRunLine({ name, audience, version }: { name?: string; audience?: string; version?: number }) {
  const text = name && audience && version !== undefined ? W.RUN_LINE(name, audience, version) : W.RUN_LINE_PERSONAL;
  return <p data-testid="wall-preset-run-line" className="text-xs text-muted-foreground">{text}</p>;
}

/** The run activity line under a deny-all refusal, when the run's preset is what sets it. */
export function WallPresetRefusalLine({ name }: { name: string }) {
  return <p data-testid="wall-preset-refusal-line" className="text-xs text-muted-foreground">{W.PUSH_REFUSED_BY(name)}</p>;
}

/** The muted line under a queued run's end sentence when its preset changed before it could start. */
export function WallPresetQueuedChanged({ name, queuedVersion, nowVersion }: { name: string; queuedVersion: number; nowVersion: number }) {
  if (queuedVersion === nowVersion) return null;
  return <p data-testid="wall-preset-queued-changed" className="text-xs text-muted-foreground">{W.QUEUED_CHANGED(name, queuedVersion, nowVersion)}</p>;
}

/** An approval row's sentence when the run's wall preset denies the host: the owner's own run, or somebody else's. */
export function wallPresetApprovalSentence(o: { name: string; host: string; ownRun: boolean; owner: string }) {
  return o.ownRun ? [W.APPROVAL_OWN(o.name), o.host, W.APPROVAL_OWN_AFTER] : [W.APPROVAL_OTHER(o.owner, o.name), o.host, W.APPROVAL_OTHER_AFTER];
}
