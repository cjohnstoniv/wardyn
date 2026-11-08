/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "Your requests" on Approvals: the person's own elevation requests and the answer to each, with the action that
// asks for a new one. A plain member cannot open Governance, so this is where they follow a request.

import * as React from "react";
import { Clock } from "lucide-react";
import { toast } from "sonner";
import { requests } from "../../../lib/api/governance-requests";
import { formatDuration, getErrorMessage, relativeTime } from "../../../lib/format";
import type { GovernanceRequest, GovernanceRequestList } from "../../../lib/types/governance-requests";
import { Button } from "../../ui/button";
import { ELEV } from "../../wardyn/copy/elevation";
import { Chip } from "../../wardyn/primitives";
import { ErrorState, TableSkeleton } from "../../wardyn/states";
import { weekdayClock } from "../run-detail/run-ends-row";
import { ElevationRequestDialog } from "./elevation-request-dialog";
import { decidersPhrase, lengthWords } from "./request-phrases";

type Shown = { chip: string; tone: React.ComponentProps<typeof Chip>["tone"]; line: string };

// A requester's own reject is a withdrawal, and the list draws no state for one. A stale row has no state to draw.
const listed = (r: GovernanceRequest) =>
  r.mine && r.kind === "elevation" && r.state !== "stale" && !(r.state === "rejected" && r.decided_by === r.proposed_by);

function shown(r: GovernanceRequest): Shown | null {
  const unit = r.unit_name ?? "";
  if (r.state === "pending") return { chip: ELEV.STATE_WAITING, tone: "warning", line: ELEV.LINE_WAITING(relativeTime(r.proposed_at), decidersPhrase(r.deciders)) };
  if (r.state === "rejected") {
    const by = r.decided_by ?? "";
    const when = relativeTime(r.decided_at ?? r.proposed_at);
    return { chip: ELEV.STATE_DENIED, tone: "danger", line: r.reason ? ELEV.LINE_DENIED(by, r.reason, when) : `${by} · ${when}` };
  }
  if (r.state === "expired") return { chip: ELEV.STATE_EXPIRED, tone: "neutral", line: ELEV.LINE_EXPIRED(relativeTime(r.expires_at)) };
  if (r.state === "applied") {
    if (r.ends_at && Date.parse(r.ends_at) > Date.now()) {
      return {
        chip: ELEV.STATE_APPROVED,
        tone: "success",
        line: ELEV.LINE_APPROVED(weekdayClock(r.ends_at), formatDuration(Date.parse(r.ends_at) - Date.now()), unit),
      };
    }
    return { chip: ELEV.STATE_ENDED, tone: "neutral", line: ELEV.LINE_ENDED(relativeTime(r.ends_at ?? r.decided_at ?? r.proposed_at)) };
  }
  return null;
}

function Row({ r, onReload }: { r: GovernanceRequest; onReload: () => void }) {
  const [busy, setBusy] = React.useState(false);
  const s = shown(r);
  if (!s) return null;
  const withdraw = async () => {
    setBusy(true);
    try {
      await requests.reject(r.id, "");
      toast.success(ELEV.TOAST_WITHDRAWN);
      onReload();
    } catch (err) {
      toast.error(ELEV.WITHDRAW_FAILED, { description: getErrorMessage(err) });
      setBusy(false);
    }
  };
  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5 border-t border-border px-4 py-3">
      <span className="flex items-baseline gap-2">
        <span className="text-sm font-medium text-foreground">{r.unit_name}</span>
        <span className="text-xs text-muted-foreground">{lengthWords(r.duration ?? "")}</span>
      </span>
      <Chip tone={s.tone} dot>
        {s.chip}
      </Chip>
      <span className="min-w-0 text-xs text-muted-foreground">{s.line}</span>
      {r.state === "pending" && (
        <Button variant="outline" size="sm" className="ml-auto" disabled={busy} onClick={withdraw}>
          {ELEV.WITHDRAW}
        </Button>
      )}
    </div>
  );
}

export function MyElevationRequests({
  list,
  onReload,
}: {
  list: GovernanceRequestList | null | "loading" | "error";
  onReload: () => void;
}) {
  const [open, setOpen] = React.useState(false);
  if (list === null) return null;
  const rows =
    typeof list === "object"
      ? list.requests.filter(listed).sort((a, b) => Date.parse(b.proposed_at) - Date.parse(a.proposed_at))
      : [];
  const offered = typeof list === "object" ? list.offered : [];
  if (typeof list === "object" && offered.length === 0 && rows.length === 0) return null;
  return (
    <section className="mb-4 rounded-xl border border-border bg-card">
      <div className="flex flex-wrap items-center justify-between gap-x-3 gap-y-2 px-4 py-3">
        <div className="min-w-0">
          <h2 className="text-sm font-semibold text-foreground">{ELEV.LIST_TITLE}</h2>
          <p className="text-xs text-muted-foreground">{ELEV.LIST_LEAD}</p>
        </div>
        {offered.length > 0 && (
          <Button variant="outline" size="sm" onClick={() => setOpen(true)}>
            <Clock className="size-4" aria-hidden="true" />
            {ELEV.ACTION}
          </Button>
        )}
      </div>
      {list === "loading" && <TableSkeleton rows={2} cols={3} />}
      {list === "error" && <ErrorState size="compact" onRetry={onReload} />}
      {rows.map((r) => (
        <Row key={r.id} r={r} onReload={onReload} />
      ))}
      {typeof list === "object" && (
        <ElevationRequestDialog open={open} onOpenChange={setOpen} offers={list.offered} onSent={onReload} />
      )}
    </section>
  );
}
