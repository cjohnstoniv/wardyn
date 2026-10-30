/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Grid3x3 } from "lucide-react";
import { Button } from "../../../../ui/button";
import { Chip } from "../../../../wardyn/primitives";
import { NR_REVIEW } from "../../../../wardyn/copy/new-run/review";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { NR_WALLS } from "../../../../wardyn/copy/personal-walls";
import { wallsLeaveOut, wallsLeaveOutKnobs, type WallsHostKnob } from "../../lenses/tools-lens";
import { ReviewGroup } from "../review-group";
import { useWallsSpec } from "../use-walls-spec";
import { AccessCredentials } from "./access-credentials";
import { stepLocation, type GroupProps } from "./group-props";
import { JumpButton } from "./jump-button";

/** `onLeftOut` is told which of the walls' host lists the drawn leave-out lines come from, so Review keeps the walls' warning for any list no line speaks for. */
export function AccessGroup({ draft, pre, preview, docRows, go, onLeftOut }: GroupProps & { onLeftOut?: (knobs: readonly WallsHostKnob[]) => void }) {
  const access = pre?.access ?? preview?.access;
  const hosts = access?.hosts ?? [];
  const source = preview?.source.name;
  // The person's own walls, to name a host they leave out; a caller with none (or no way to read them) sees no such line.
  const walls = useWallsSpec();
  const leftOut = wallsLeaveOut(walls, pre?.access ?? preview?.access, !!access);
  const knobKey = wallsLeaveOutKnobs(walls, pre?.access ?? preview?.access, !!access).join();
  React.useEffect(() => onLeftOut?.(knobKey ? (knobKey.split(",") as WallsHostKnob[]) : []), [knobKey, onLeftOut]);
  // A run that reaches only its listed sites says so (T4-b); one that may ask for more keeps the plain count.
  const closed = !!access && !access.unlisted.allow_all && access.unlisted.mode === "always_deny";
  const count = hosts.length === 1 ? NR_REVIEW.POLICY_HOST_ONE : NR_REVIEW.POLICY_HOSTS(hosts.length);
  const closedCount = hosts.length === 1 ? NR_WALLS.REVIEW_SITE_ONE : NR_WALLS.REVIEW_SITES(hosts.length);
  return (
    <ReviewGroup
      id="access"
      label={NR_REVIEW.GROUP_ACCESS}
      value={access && (closed ? closedCount : count)}
      provenance={source ? NR_SHARED.JOIN(NR_REVIEW.PROV_SOURCE_POLICY, source) : NR_REVIEW.PROV_SOURCE_POLICY}
      docRows={docRows}
      editor={(close) => (
        <>
          <p className="text-sm text-muted-foreground">{NR_REVIEW.ACCESS_NOTE}</p>
          {hosts.map((h) => (
            <div key={h.host} className="flex items-center justify-between gap-2 text-sm">
              <code className="font-mono text-xs">{h.host}</code>
              {h.source.name && <Chip>{h.source.name}</Chip>}
            </div>
          ))}
          <AccessCredentials access={access} intent={draft.intent} />
          <JumpButton label={NR_REVIEW.EDIT_TOOLS_ACCESS} to={stepLocation("tools")} go={go} />
          <Button type="button" variant="ghost" className="w-fit" onClick={close}>{NR_REVIEW.DONE}</Button>
        </>
      )}
    >
      {leftOut.map((host) => (
        <p key={host} data-testid="walls-leave-out" className="flex items-center gap-2 rounded-md border border-border bg-muted px-3 py-2 text-xs text-foreground">
          <Grid3x3 aria-hidden="true" className="size-3.5 shrink-0 text-muted-foreground" />
          <span className="min-w-0 [overflow-wrap:anywhere]">
            {NR_WALLS.LEAVE_OUT_BEFORE}
            <span className="font-mono">{host}</span>
            {NR_WALLS.LEAVE_OUT_AFTER}
          </span>
        </p>
      ))}
    </ReviewGroup>
  );
}
