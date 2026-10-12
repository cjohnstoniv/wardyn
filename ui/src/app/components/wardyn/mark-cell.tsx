/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The check / caveat / cross cell of the barrier comparison matrices. A caveat
// is never a bare "partly": its native title names the tier's residual risk,
// behind `caveatPrefix` (the setup matrix says "Doesn't stop:", the New Run
// comparison table says "Except:").
import { Check, X, AlertTriangle } from "lucide-react";
import { CC_META, type CCMark } from "./cc-meta";
import { RESIDUAL_PREFIX } from "./copy";
import type { ConfinementClass } from "../../lib/types";

export function MarkCell({
  mark,
  cc,
  caveatPrefix = RESIDUAL_PREFIX,
}: {
  mark: CCMark;
  cc: ConfinementClass;
  caveatPrefix?: string;
}) {
  if (mark === "yes") return <Check className="mx-auto size-5 text-success" aria-label="Yes" />;
  if (mark === "no") return <X className="mx-auto size-5 text-danger" aria-label="No" />;
  return (
    <span
      title={`${caveatPrefix} ${CC_META[cc].doesntProtect}`}
      aria-label="Yes, with caveat"
      className="mx-auto inline-flex text-warning"
    >
      <AlertTriangle className="size-5" aria-hidden />
    </span>
  );
}
