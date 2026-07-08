/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// TierMatrix — the pricing-table "compare all three barriers" view: tiers are
// columns, protections are rows, cells are the 3-state Protected / Caveat / Not-
// protected trio. A "more detail on demand" surface (TierMatrixDialog), never the
// onboarding intro — the 3-card pickers stay the primary chooser.
//
// HONESTY/spec constraint: the wire codes (CC1/CC2/CC3) and the substrate
// mechanism (runc/gVisor/Kata) live ONLY in tooltips and sr-only text — the
// visible copy uses the friendly Fence/Wall/Vault labels. This dialog opens
// inside the New Run dialog, where wizard.spec.ts asserts no visible "gVisor" or
// wire code. ("Needs KVM hardware" is the one user-approved visible substrate note.)
import * as React from "react";
import { CircleCheck, CircleAlert, CircleX, Info } from "lucide-react";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "../ui/dialog";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "../ui/table";
import { cn } from "../ui/utils";
import { ConfinementChip } from "./primitives";
import {
  CC_MATRIX_ROWS,
  CC_MATRIX_WHERE,
  CC_META,
  CC_ORDER,
  CONFINEMENT_CONSTANT_NOTE,
  type CCMark,
} from "./cc-meta";
import { RESIDUAL_PREFIX } from "./copy";
import type { ConfinementClass } from "../../lib/types";

// The exact CircleCheck/CircleAlert/CircleX + success/warning/danger trio the
// audit-outcome badge uses (primitives.tsx), so "protected / caveat / not" reads
// identically everywhere. sr defaults are for yes/no; caveat overrides with the
// verbatim residual-risk sentence below.
const MARK: Record<CCMark, { Icon: React.ElementType; cls: string; sr: string }> = {
  yes: { Icon: CircleCheck, cls: "text-success", sr: "Protected" },
  caveat: { Icon: CircleAlert, cls: "text-warning", sr: "Partly protected" },
  no: { Icon: CircleX, cls: "text-danger", sr: "Not protected" },
};

function MatrixCell({ mark, cc }: { mark: CCMark; cc: ConfinementClass }) {
  const m = MARK[mark];
  // A caveat isn't a bare "partly" — it names the residual risk, reusing the
  // verbatim RESIDUAL_PREFIX + doesntProtect wording (no new residual copy). Kept
  // in title + sr-only so it never leaks into visible tier-mechanism text.
  const label = mark === "caveat" ? `${RESIDUAL_PREFIX} ${CC_META[cc].doesntProtect}` : m.sr;
  return (
    <TableCell className="text-center">
      <span title={label} className={cn("inline-flex justify-center", m.cls)}>
        <m.Icon className="size-4" aria-hidden />
        <span className="sr-only">{label}</span>
      </span>
    </TableCell>
  );
}

export function TierMatrix() {
  return (
    <div className="space-y-3">
      <Table>
        <TableHeader>
          <TableRow>
            {/* Empty corner over the protection-name column. */}
            <TableHead className="w-1/3" />
            {CC_ORDER.map((cc) => (
              <TableHead key={cc} className="text-center">
                <span className="inline-flex justify-center">
                  <ConfinementChip value={cc} />
                </span>
              </TableHead>
            ))}
          </TableRow>
        </TableHeader>
        <TableBody>
          {CC_MATRIX_ROWS.map((row) => (
            <TableRow key={row.label}>
              <TableCell className="font-medium whitespace-normal text-foreground">
                {row.label}
              </TableCell>
              {CC_ORDER.map((cc) => (
                <MatrixCell key={cc} mark={row.cells[cc]} cc={cc} />
              ))}
            </TableRow>
          ))}
          {/* Separate "where it runs" group — plain text; border-top sets it off. */}
          <TableRow>
            <TableCell className="border-t-2 font-medium text-foreground">
              {CC_MATRIX_WHERE.label}
            </TableCell>
            {CC_ORDER.map((cc) => (
              <TableCell key={cc} className="border-t-2 text-center text-muted-foreground">
                {CC_MATRIX_WHERE.cells[cc]}
              </TableCell>
            ))}
          </TableRow>
        </TableBody>
      </Table>
      {/* Applies to every tier — the barrier only sets isolation strength. */}
      <p className="flex items-start gap-2 text-xs leading-relaxed text-muted-foreground">
        <Info className="mt-0.5 size-3.5 shrink-0 text-primary" />
        {CONFINEMENT_CONSTANT_NOTE}
      </p>
    </div>
  );
}

// Controlled dialog wrapper (mirrors SetupGuideDialog) — the caller owns the
// trigger and open state; a later step wires the "Compare all three" entry points.
export function TierMatrixDialog({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>Compare the three barriers</DialogTitle>
          <DialogDescription>
            How strongly each tier walls the agent off from your machine.
          </DialogDescription>
        </DialogHeader>
        <TierMatrix />
      </DialogContent>
    </Dialog>
  );
}
