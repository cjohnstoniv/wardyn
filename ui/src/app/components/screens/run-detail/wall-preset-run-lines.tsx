/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run page's wall preset sentences (boards WP-3), as plain components over the words they need: the policy tab's
// line and the approval row's sentence.
import { WALL_PRESETS as W } from "../../wardyn/copy/wall-presets";

/** The policy tab's line under the source line. A person preset seen by anyone but its owner is named by no one. */
export function WallPresetRunLine({ name, audience, version }: { name?: string; audience?: string; version?: number }) {
  const text = name && audience && version !== undefined ? W.RUN_LINE(name, audience, version) : W.RUN_LINE_PERSONAL;
  return <p data-testid="wall-preset-run-line" className="text-xs text-muted-foreground">{text}</p>;
}

/** An approval row's sentence when the run's wall preset denies the host: the owner's own run, or somebody else's. */
export function wallPresetApprovalSentence(o: { name: string; host: string; ownRun: boolean; owner: string }) {
  return o.ownRun ? [W.APPROVAL_OWN(o.name), o.host, W.APPROVAL_OWN_AFTER] : [W.APPROVAL_OTHER(o.owner, o.name), o.host, W.APPROVAL_OTHER_AFTER];
}
