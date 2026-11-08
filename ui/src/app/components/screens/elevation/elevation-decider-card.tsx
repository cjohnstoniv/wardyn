/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// An elevation request as a card under Waiting on you, for a decider who cannot open Governance. Approve and Deny
// answer it directly: the request already carries everything a decider reads.

import * as React from "react";
import { Check, Clock, X } from "lucide-react";
import { toast } from "sonner";
import { requests } from "../../../lib/api/governance-requests";
import { APPROVALS } from "../../../lib/approvals-copy";
import { absoluteTime, getErrorMessage, relativeTime } from "../../../lib/format";
import type { GovernanceRequest } from "../../../lib/types/governance-requests";
import { Button } from "../../ui/button";
import { DECIDERS_INBOX } from "../../wardyn/copy/deciders-inbox";
import { ELEV } from "../../wardyn/copy/elevation";
import { RequestedByRow } from "../../wardyn/decider-lines";
import { Chip } from "../../wardyn/primitives";
import { decidersPhrase, lengthWords } from "./request-phrases";

export function ElevationDeciderCard({ request, onDone }: { request: GovernanceRequest; onDone: () => void }) {
  const [busy, setBusy] = React.useState(false);
  const person = request.proposed_by;
  const unit = request.unit_name ?? "";
  const length = lengthWords(request.duration ?? "");
  const decide = async (approve: boolean) => {
    setBusy(true);
    try {
      if (approve) await requests.approve(request.id);
      else await requests.reject(request.id, "");
      toast.success(approve ? APPROVALS.TOAST_APPROVED : APPROVALS.TOAST_DENIED);
    } catch (err) {
      toast.error(approve ? APPROVALS.TOAST_APPROVE_FAILED : APPROVALS.TOAST_DENY_FAILED, { description: getErrorMessage(err) });
    } finally {
      setBusy(false);
    }
    // Decided or refused, the next read says whether the card stays.
    onDone();
  };
  return (
    <div className="rounded-xl border border-warning/30 bg-warning/5 p-4">
      <div className="flex flex-wrap items-center gap-2">
        <Chip tone="success">{ELEV.F_KIND}</Chip>
        <span className="inline-flex items-center gap-1.5 text-sm font-semibold text-foreground">
          <Clock className="size-3.5 text-muted-foreground" aria-hidden="true" />
          {ELEV.F_TITLE(person, unit, length)}
        </span>
      </div>
      <RequestedByRow
        who={person}
        extra={
          request.note?.ticket ? (
            <span className="text-xs text-muted-foreground">
              {ELEV.TICKET_REQUIRED_LABEL} <span className="font-mono">{request.note.ticket}</span>
            </span>
          ) : undefined
        }
      />
      <p className="mt-1 text-xs text-muted-foreground">
        <span className="font-medium text-foreground">{DECIDERS_INBOX.DECIDED_BY_LABEL}</span> {decidersPhrase(request.deciders)}
      </p>
      <div className="mt-3 space-y-1.5 rounded-lg border border-border bg-background px-3 py-2 text-sm text-foreground">
        <p>
          <span className="font-semibold">{ELEV.F_REASON}</span> {request.note?.reason}
        </p>
        <p>
          <span className="font-semibold">{ELEV.F_WHAT_LABEL}</span> {ELEV.F_WHAT(person, unit, length)}
        </p>
        <p className="text-muted-foreground">
          <span className="font-semibold text-foreground">{ELEV.F_BLAST_LABEL}</span> {ELEV.F_BLAST(unit)}
        </p>
      </div>
      <div className="mt-3 flex flex-wrap items-center gap-2 border-t border-border pt-3">
        <Button size="sm" variant="info" disabled={busy} onClick={() => decide(true)}>
          <Check className="size-4" /> Approve
        </Button>
        <Button size="sm" variant="outline" disabled={busy} onClick={() => decide(false)}>
          <X className="size-4" /> Deny
        </Button>
        <span className="ml-auto text-xs text-muted-foreground" title={absoluteTime(request.proposed_at)}>
          {ELEV.F_REQUESTED(relativeTime(request.proposed_at))}
        </span>
      </div>
    </div>
  );
}
