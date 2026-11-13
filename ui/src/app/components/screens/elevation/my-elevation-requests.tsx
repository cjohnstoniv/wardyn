/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "Your requests" on Approvals: the person's own elevation requests and the answer to each, with the action that
// asks for a new one. A plain member cannot open Governance, so this is where they follow a request.

import * as React from "react";
import { Clock, Package } from "lucide-react";
import { toast } from "sonner";
import { requests } from "../../../lib/api/governance-requests";
import { getErrorMessage, relativeTime } from "../../../lib/format";
import { timeLeft } from "../../../lib/time-left";
import type { GovernanceRequest, GovernanceRequestList } from "../../../lib/types/governance-requests";
import { Button } from "../../ui/button";
import { ELEV } from "../../wardyn/copy/elevation";
import { IMG } from "../../wardyn/copy/image-request";
import { Chip } from "../../wardyn/primitives";
import { ErrorState, TableSkeleton } from "../../wardyn/states";
import { weekdayClock } from "../run-detail/run-ends-row";
import { shortDigest, splitImageRef } from "./image-ref";
import { decidersPhrase, lengthWords } from "./request-phrases";
import { RequestDialog, type RequestKind } from "./request-dialog";

type Shown = { chip: string; tone: React.ComponentProps<typeof Chip>["tone"]; line: string };

const listed = (r: GovernanceRequest) => r.mine && (r.kind === "elevation" || r.kind === "image");

// A requester's own reject is a withdrawal: the view has no flag for it, so it is a rejection the proposer made.
const withdrawn = (r: GovernanceRequest) => r.state === "rejected" && r.decided_by === r.proposed_by;
// Who decided and when; a decider the server no longer names leaves just the time.
const decidedLine = (r: GovernanceRequest) =>
  [r.decided_by, relativeTime(r.decided_at ?? r.proposed_at)].filter(Boolean).join(" · ");

function shown(r: GovernanceRequest): Shown | null {
  const unit = r.unit_name ?? "";
  const image = r.kind === "image";
  if (r.state === "pending") {
    const when = relativeTime(r.proposed_at);
    return { chip: ELEV.STATE_WAITING, tone: "warning", line: image ? IMG.LINE_WAITING(when) : ELEV.LINE_WAITING(when, decidersPhrase(r.deciders)) };
  }
  if (r.state === "rejected") {
    const when = relativeTime(r.decided_at ?? r.proposed_at);
    if (withdrawn(r)) return { chip: IMG.STATE_WITHDRAWN, tone: "neutral", line: IMG.LINE_WITHDRAWN(when) };
    return { chip: ELEV.STATE_DENIED, tone: "danger", line: r.reason ? ELEV.LINE_DENIED(r.decided_by ?? "", r.reason, when) : decidedLine(r) };
  }
  if (r.state === "stale") return { chip: IMG.STATE_STALE, tone: "warning", line: IMG.LINE_STALE };
  if (r.state === "expired") return { chip: ELEV.STATE_EXPIRED, tone: "neutral", line: ELEV.LINE_EXPIRED(relativeTime(r.expires_at)) };
  if (r.state === "applied") {
    // An image grant is not read back and an admin can delete it, so the row says who approved and when, not that it
    // launches now. An elevation held with no end has no end to state, and it has not ended.
    if (image || r.no_end) return { chip: ELEV.STATE_APPROVED, tone: "success", line: decidedLine(r) };
    if (r.ends_at && Date.parse(r.ends_at) > Date.now()) {
      return {
        chip: ELEV.STATE_APPROVED,
        tone: "success",
        line: ELEV.LINE_APPROVED(weekdayClock(r.ends_at), timeLeft(Date.parse(r.ends_at) - Date.now()), unit),
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
  const ref = splitImageRef(r.image ?? "");
  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5 border-t border-border px-4 py-3">
      {r.kind === "image" ? (
        <span className="flex min-w-0 flex-wrap items-baseline gap-x-2">
          <span className="break-all font-mono text-sm font-medium text-foreground">{ref.name}</span>
          {ref.digest && (
            <span className="font-mono text-xs text-muted-foreground" title={ref.digest}>
              {shortDigest(ref.digest)}
            </span>
          )}
        </span>
      ) : (
        <span className="flex items-baseline gap-2">
          <span className="text-sm font-medium text-foreground">{r.unit_name}</span>
          <span className="text-xs text-muted-foreground">{lengthWords(r.duration ?? "")}</span>
        </span>
      )}
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
  const [kind, setKind] = React.useState<RequestKind>("elevation");
  if (list === null) return null;
  const ready = typeof list === "object";
  const rows = ready ? list.requests.filter(listed).sort((a, b) => Date.parse(b.proposed_at) - Date.parse(a.proposed_at)) : [];
  const offered = ready ? list.offered : [];
  const images = ready && list.image_offered;
  if (ready && offered.length === 0 && !images && rows.length === 0) return null;
  const ask = (k: RequestKind) => {
    setKind(k);
    setOpen(true);
  };
  const lead =
    offered.length > 0 && images ? IMG.LIST_LEAD_BOTH : offered.length > 0 ? ELEV.LIST_LEAD : images ? IMG.LIST_LEAD_IMAGES : null;
  return (
    <section className="mb-4 rounded-xl border border-border bg-card">
      <div className="flex flex-wrap items-center justify-between gap-x-3 gap-y-2 px-4 py-3">
        <div className="min-w-0">
          <h2 className="text-sm font-semibold text-foreground">{ELEV.LIST_TITLE}</h2>
          {lead && <p className="text-xs text-muted-foreground">{lead}</p>}
        </div>
        {ready && (
          <div className="flex flex-wrap gap-2">
            {offered.length > 0 && (
              <Button variant="outline" size="sm" onClick={() => ask("elevation")}>
                <Clock className="size-4" aria-hidden="true" />
                {ELEV.ACTION}
              </Button>
            )}
            {images && (
              <Button variant="outline" size="sm" onClick={() => ask("image")}>
                <Package className="size-4" aria-hidden="true" />
                {IMG.ACTION}
              </Button>
            )}
          </div>
        )}
      </div>
      {list === "loading" && <TableSkeleton rows={2} cols={3} />}
      {list === "error" && <ErrorState size="compact" title={IMG.LIST_FAILED_TITLE} message={IMG.LIST_FAILED_BODY} onRetry={onReload} />}
      {ready && rows.length === 0 && <p className="border-t border-border px-4 py-3 text-xs text-muted-foreground">{IMG.LIST_EMPTY}</p>}
      {rows.map((r) => (
        <Row key={r.id} r={r} onReload={onReload} />
      ))}
      {ready && (
        <RequestDialog open={open} onOpenChange={setOpen} initialKind={kind} offers={offered} imageOffered={images} onSent={onReload} />
      )}
    </section>
  );
}
