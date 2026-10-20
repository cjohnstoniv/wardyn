/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The policy editor's "Recording" section (#2968, M7 Knobs-B and States 1 and 3): the policy's
// level, the organisation's locked minimum (a lower level is struck out, the others stay open), and
// keystrokes as a fixed line, not a control. It reads and writes the one document the textarea shows.
import * as React from "react";
import { health } from "../../lib/api/health";
import { useRecordingDisabled } from "../../lib/hooks/use-recording-disabled";
import type { RecordingLevel, RunPolicySpec, SiteRecording } from "../../lib/types";
import { cn } from "../ui/utils";
import { RECORDING_LEVEL as C } from "./copy/recording-level";
import { RECORDING_DISABLED_DESC, RECORDING_DISABLED_TITLE } from "./copy/recording-disabled";
import { Chip, SectionLabel } from "./primitives";

const rank = (l: RecordingLevel | undefined) => (l ? C.LEVELS.findIndex((o) => o.value === l) + 1 : 0);

// The organisation's level, when this person may read it; a member who cannot sees no lock here, and
// the server still holds their run to it.
function useOrgRecording(): SiteRecording | undefined {
  const [org, setOrg] = React.useState<SiteRecording | undefined>();
  React.useEffect(() => {
    let alive = true;
    health
      .getSiteConfig()
      .then((sc) => alive && setOrg(sc.recording))
      .catch(() => {});
    return () => {
      alive = false;
    };
  }, []);
  return org;
}

export function RecordingSection({
  spec,
  onSpecChange,
}: {
  spec: RunPolicySpec;
  onSpecChange: (next: RunPolicySpec) => void;
}) {
  const storeOff = useRecordingDisabled() === true;
  const org = useOrgRecording();
  const floor: RecordingLevel | undefined = org?.locked ? (org.level ?? "terminal") : undefined;
  const lower = C.LEVELS.filter((o) => rank(o.value) < rank(floor));
  const chosen = C.LEVELS.find((o) => o.value === spec.recording);
  return (
    <section aria-labelledby="policy-recording-label" className="space-y-2" data-testid="policy-recording">
      <div className="flex items-center gap-2">
        <SectionLabel>
          <span id="policy-recording-label">{C.ROW}</span>
        </SectionLabel>
        {storeOff && <Chip tone="neutral">{C.DISABLED}</Chip>}
        {!storeOff && floor && lower.length > 0 && <Chip tone="warning">{C.LOCKED}</Chip>}
      </div>
      {storeOff && (
        <div role="note" className="rounded-lg border border-border p-3 text-body">
          <b className="font-semibold">{RECORDING_DISABLED_TITLE}</b>
          <p className="text-muted-foreground">{RECORDING_DISABLED_DESC}</p>
        </div>
      )}
      <div role="radiogroup" aria-labelledby="policy-recording-label" className="flex flex-wrap gap-1">
        {C.LEVELS.map((o) => {
          const below = rank(o.value) < rank(floor);
          return (
            <button
              key={o.value}
              type="button"
              role="radio"
              aria-checked={spec.recording === o.value}
              disabled={below}
              className={cn(
                "rounded-md border border-border px-3 py-1 text-body",
                spec.recording === o.value && "border-primary bg-primary-subtle",
                below && "line-through opacity-60",
              )}
              onClick={() => onSpecChange({ ...spec, recording: o.value })}
            >
              {o.label}
            </button>
          );
        })}
      </div>
      <p className="text-meta text-muted-foreground">{chosen ? chosen.description : C.UNSET}</p>
      {floor && lower.length > 0 && (
        <p className="text-meta text-muted-foreground" data-testid="policy-recording-floor">
          {C.MINIMUM_ABOVE(C.ORG, C.LEVELS.find((o) => o.value === floor)!.label, lower.map((o) => o.label))}
        </p>
      )}
      <div className="flex items-baseline gap-2 text-body" data-testid="policy-recording-keystrokes">
        <span className="font-medium">{C.KEYSTROKES}</span>
        <span>{C.KEYSTROKES_VALUE}</span>
        <span className="text-meta text-muted-foreground">{C.KEYSTROKES_NOTE}</span>
      </div>
      <p className="text-meta text-muted-foreground">{C.BELOW}</p>
    </section>
  );
}
