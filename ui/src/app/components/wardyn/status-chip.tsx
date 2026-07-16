/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { Minus } from "lucide-react";
import { Chip } from "./primitives";
import { STATUS_LABEL, type StatusKind } from "./copy";

// Tone mapping for the unified status vocabulary (B6, copy.ts). "unavailable"
// always carries a concrete reason — surfaced as the chip's tooltip.
const STATUS_TONE: Record<StatusKind, "success" | "warning" | "danger" | "neutral"> = {
  ready: "success",
  connected: "success",
  "needs-setup": "warning",
  unavailable: "danger",
  incompatible: "danger",
  checking: "neutral",
  unverified: "neutral",
};

// The one extra kind beyond copy.ts's cross-screen StatusKind union — the
// Getting-started shell (setup-layout.tsx) tags optional steps with it. It lives
// here rather than widening the shared StatusKind vocabulary.
type ExtraStatusKind = "optional";

export function StatusChip({
  status,
  reason,
  label,
}: {
  status: StatusKind | ExtraStatusKind;
  reason?: string;
  // Override the default STATUS_LABEL text while keeping the status's tone/dot —
  // e.g. the compose setup checklist renders "ready" green but says "Configured",
  // never "Ready"/"Verified" (v1 is declared-present, not live-verified).
  label?: string;
}) {
  if (status === "optional") {
    return (
      <Chip tone="neutral" title={reason}>
        <Minus className="size-3" aria-hidden />
        {label ?? "Optional"}
        {/* AT copy of the native title (tier-illustration.tsx pattern) — `title`
            isn't reliably announced by screen readers. */}
        {reason && <span className="sr-only">{reason}</span>}
      </Chip>
    );
  }
  return (
    <Chip tone={STATUS_TONE[status]} dot title={reason}>
      {label ?? STATUS_LABEL[status]}
      {reason && <span className="sr-only">{reason}</span>}
    </Chip>
  );
}
