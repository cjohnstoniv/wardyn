/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Drives: the person's one drive from /me.user_drive, as today's wizard offers
// it. An Environment run opens with it attached when it can be.

import * as React from "react";
import { useUserDrive } from "../../../../wardyn/operator-context";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import { MEMBER } from "../../../../../lib/governance-copy";
import { DRIVE_MEMBER } from "../../../../../lib/user-drives-copy";
import { useNewRunDispatch, useNewRunDraft } from "../../new-run-route";
import { LegacyDriveRow } from "./drive-row";
import { legacyMounted, mountLegacy, nothingChosen, setLegacyReadOnly, setStartsAtLegacy, startsAtLegacy } from "./drives-section.model";

/** The id a held Continue focuses when the step cannot be completed (starts_here_required). */
export const STARTS_FOCUS_ID = "nr-starts-here";

const UNAVAILABLE_TEXT: Record<string, string | undefined> = {
  groups_snapshot_stale: MEMBER.DENIED_STALE_GROUPS,
  governance_unavailable: DRIVE_MEMBER.NR_GOVERNANCE_UNAVAILABLE,
  unmountable: DRIVE_MEMBER.NR_UNAVAILABLE,
  unavailable: DRIVE_MEMBER.NR_UNAVAILABLE,
};

/** Why the drive is not offered: the door outranks everything, then a /me that could not answer, then a pause. */
function useLegacy() {
  const { drive, deniedByProfile, unavailable } = useUserDrive();
  const reason = deniedByProfile
    ? DRIVE_MEMBER.NR_DENIED(deniedByProfile)
    : (unavailable ? UNAVAILABLE_TEXT[unavailable] : undefined) ?? (drive?.paused ? DRIVE_MEMBER.NR_PAUSED : undefined);
  return { drive, reason };
}

/** Whether the drive's Starts here radio is on the page and can take the failed-Continue focus. */
export function useDriveStartsTarget(): boolean {
  const { drive, reason } = useLegacy();
  const mounted = legacyMounted(useNewRunDraft().intent);
  return !!drive && !reason && mounted;
}

/** `focus`: this row's radio carries #nr-starts-here (no repository is included to carry it). */
export function DrivesSection({ focus = false }: { focus?: boolean }) {
  const draft = useNewRunDraft();
  const dispatch = useNewRunDispatch();
  const { drive, reason } = useLegacy();
  const intent = draft.intent;
  const edit = (apply: (i: typeof intent) => typeof intent) => dispatch({ type: "edit", apply });

  // Environment opens with the person's own drive when it can attach, once, and only while nothing is chosen.
  const preselected = React.useRef(false);
  const canAttach = !!drive && !reason;
  React.useEffect(() => {
    if (preselected.current || !canAttach || intent.shape !== "environment" || !nothingChosen(intent)) return;
    preselected.current = true;
    edit((i) => setStartsAtLegacy(mountLegacy(i, true)));
    // `edit` is a closure over a stable dispatch.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [canAttach, intent]);

  if (!reason && !drive) return null;
  return (
    <section aria-labelledby="nr-drives-title" className="flex flex-col gap-2 rounded-lg border border-border p-4">
      <div>
        <h2 id="nr-drives-title" className="font-semibold">{NR_FILES.USER_DRIVES_TITLE}</h2>
        <p className="text-sm text-muted-foreground">{NR_FILES.DRIVES_BODY}</p>
      </div>
      {reason || !drive ? (
        <p data-testid="nr-drive-reason" className="text-sm text-muted-foreground">{reason}</p>
      ) : (
        <LegacyDriveRow
          drive={drive}
          mounted={legacyMounted(intent)}
          readOnly={intent.drive?.read_only === true}
          onMount={(on) => edit((i) => mountLegacy(i, on))}
          onReadOnly={(on) => edit((i) => setLegacyReadOnly(i, on))}
          starts={{ checked: startsAtLegacy(intent), onSelect: () => edit(setStartsAtLegacy), id: focus ? STARTS_FOCUS_ID : undefined }}
        />
      )}
    </section>
  );
}
