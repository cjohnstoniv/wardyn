/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// One of Review's six groups: what the run gets, who put it there, and a
// Change disclosure. A document-only field is a read-only row with its chip.

import * as React from "react";
import { Check, CircleAlert } from "lucide-react";
import { Button } from "../../../ui/button";
import { Chip } from "../../../wardyn/primitives";
import { NR_REVIEW } from "../../../wardyn/copy/new-run/review";
import type { ReviewGroupId } from "../draft/field-homes";
import type { DocRow } from "./review-model";

/** The groups a current blocker's fix lives under (groupsBlockedBy); each shows the blocker mark, not the check. */
export const BlockedGroups = React.createContext<ReadonlySet<ReviewGroupId>>(new Set());

export interface ReviewGroupProps {
  id: ReviewGroupId;
  label: string;
  value?: React.ReactNode;
  /** A mark shown under the value (the barrier). */
  mark?: React.ReactNode;
  detail?: React.ReactNode;
  /** Who put the value there. */
  provenance?: string;
  /** More read-only content under the value. */
  children?: React.ReactNode;
  docRows?: readonly DocRow[];
  /** The Change disclosure's body. Without one the group has no Change. */
  editor?: (close: () => void) => React.ReactNode;
}

export function ReviewGroup({ id, label, value, mark, detail, provenance, children, docRows = [], editor }: ReviewGroupProps) {
  const [open, setOpen] = React.useState(false);
  const regionId = `nr-edit-${id}`;
  const blocked = React.useContext(BlockedGroups).has(id);
  return (
    <div data-review-group={id} data-blocked={blocked || undefined} className="grid grid-cols-[28px_minmax(0,1fr)_auto] items-start gap-2.5 border-t border-border py-4 pr-4 pl-5 first:border-t-0">
      {blocked ? (
        <span className="grid size-[22px] place-items-center rounded-full border border-danger/25 bg-danger-subtle text-danger">
          <CircleAlert aria-hidden="true" className="size-3" />
          <span className="sr-only">{NR_REVIEW.BADGE_ATTENTION}</span>
        </span>
      ) : (
        <span aria-hidden="true" className="grid size-[22px] place-items-center rounded-full border border-success/35 bg-success-subtle text-success">
          <Check className="size-3" />
        </span>
      )}
      <div className="flex min-w-0 flex-col gap-1">
        <h3 className="label-eyebrow">{label}</h3>
        {value !== undefined && <strong className="font-medium break-words text-foreground">{value}</strong>}
        {mark && <div className="my-1">{mark}</div>}
        {detail && <p className="text-sm text-muted-foreground">{detail}</p>}
        {children}
        {docRows.map((r) => (
          <div key={r.path} className="flex flex-wrap items-center gap-2 text-sm">
            <code className="font-mono text-xs">{r.path}</code>
            <span className="min-w-0 break-all">{r.value}</span>
            <Chip>{NR_REVIEW.DOC_ONLY_CHIP}</Chip>
          </div>
        ))}
        {provenance && <small className="text-primary">{provenance}</small>}
      </div>
      {editor ? (
        <Button
          type="button"
          variant="ghost"
          aria-expanded={open}
          aria-controls={open ? regionId : undefined}
          aria-label={`${open ? NR_REVIEW.CLOSE : NR_REVIEW.CHANGE} ${label}`}
          onClick={() => setOpen((o) => !o)}
        >
          {open ? NR_REVIEW.CLOSE : NR_REVIEW.CHANGE}
        </Button>
      ) : <span />}
      {editor && open && (
        <div id={regionId} role="group" aria-label={label} className="col-span-full flex flex-col gap-3 rounded-lg bg-surface-2/60 p-3.5">
          {editor(() => setOpen(false))}
        </div>
      )}
    </div>
  );
}
