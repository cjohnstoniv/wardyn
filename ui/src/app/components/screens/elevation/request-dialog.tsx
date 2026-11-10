/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// One request dialog for both kinds. When a person is offered both, each dialog carries the same choice, and
// picking the other kind closes this one and opens that one (what was typed does not carry over). With one kind
// offered there is no choice.
import * as React from "react";
import type { ElevationOffer, GovernanceRequest } from "../../../lib/types/governance-requests";
import { IMG } from "../../wardyn/copy/image-request";
import { ElevationRequestDialog } from "./elevation-request-dialog";
import { ImageRequestDialog } from "./image-request-dialog";

export type RequestKind = "elevation" | "image";

const KINDS: { value: RequestKind; label: string }[] = [
  { value: "elevation", label: IMG.KIND_ELEVATION },
  { value: "image", label: IMG.KIND_IMAGE },
];

export function KindChoice({ kind, onKind }: { kind: RequestKind; onKind: (k: RequestKind) => void }) {
  const name = React.useId();
  return (
    <div className="space-y-2">
      <p className="text-sm font-medium leading-none text-foreground">{IMG.KIND_LABEL}</p>
      <div role="radiogroup" aria-label={IMG.KIND_LABEL} className="inline-flex overflow-hidden rounded-md border border-border text-xs">
        {KINDS.map((k) => (
          <label key={k.value} className="cursor-pointer">
            <input type="radio" className="peer sr-only" name={name} checked={kind === k.value} onChange={() => onKind(k.value)} />
            <span className="block px-3 py-1.5 text-muted-foreground peer-checked:bg-primary peer-checked:text-primary-foreground peer-focus-visible:ring-2 peer-focus-visible:ring-inset peer-focus-visible:ring-ring">
              {k.label}
            </span>
          </label>
        ))}
      </div>
    </div>
  );
}

export function RequestDialog({
  open,
  onOpenChange,
  initialKind,
  offers,
  imageOffered,
  onSent,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  initialKind: RequestKind;
  offers: ElevationOffer[];
  imageOffered: boolean;
  onSent?: (r: GovernanceRequest) => void;
}) {
  // The kind picked inside the dialog; until one is, the kind that was asked for.
  const [picked, setPicked] = React.useState<RequestKind | null>(null);
  const kind = picked ?? initialKind;
  // Every opening starts on the kind that was asked for.
  React.useEffect(() => {
    if (!open) setPicked(null);
  }, [open]);
  const both = offers.length > 0 && imageOffered;
  const choice = both ? <KindChoice kind={kind} onKind={setPicked} /> : undefined;
  return (
    <>
      <ElevationRequestDialog open={open && kind === "elevation"} onOpenChange={onOpenChange} offers={offers} kindChoice={choice} onSent={onSent} />
      <ImageRequestDialog open={open && kind === "image"} onOpenChange={onOpenChange} kindChoice={choice} onSent={onSent} />
    </>
  );
}
