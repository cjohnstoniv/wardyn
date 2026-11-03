/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { CircleAlert, Shield } from "lucide-react";
import type { NewRunLocation } from "../draft/new-run-url";
import { WALL_PRESETS as W } from "../../../wardyn/copy/wall-presets";

/**
 * Review's wall preset line, beside the person's own walls line: "Wall preset: Sealed (built in)" and a Change link
 * back to the Tools step. A preset that is no longer available is drawn as the error it is. Nothing for Open.
 */
export function WallPresetLine({ name, audience, unavailable, go }: { name: string; audience: string; unavailable: boolean; go: (loc: NewRunLocation) => void }) {
  const Icon = unavailable ? CircleAlert : Shield;
  return (
    <p data-testid="wall-preset-line" className={`flex items-start gap-2 rounded-md border px-3 py-2 text-xs text-foreground ${unavailable ? "border-danger/40 bg-danger-subtle" : "border-border bg-muted"}`}>
      <Icon aria-hidden="true" className={`mt-0.5 size-3.5 shrink-0 ${unavailable ? "text-danger" : "text-muted-foreground"}`} />
      <span className="min-w-0 [overflow-wrap:anywhere]">
        {audience ? W.REVIEW_LINE(name, audience) : name}{" "}
        <button type="button" className="font-semibold text-primary hover:underline" onClick={() => go({ screen: "step", step: "tools" })}>{W.CHANGE}</button>
      </span>
    </p>
  );
}
