/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A drive or folder that is read-only by its own setting, not by this run's
// choice: a lock, "Read-only" and why.

import { Lock } from "lucide-react";
import { InfoTip } from "../../../../wardyn/info-tip";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";

export function LockedAccess({ id }: { id: string }) {
  return (
    <span data-testid={`${id}-locked`} className="inline-flex min-h-9 items-center gap-1.5 text-sm">
      <Lock className="size-3.5 text-muted-foreground" aria-hidden="true" />
      <span>{NR_FILES.ACCESS_READ}</span>
      <InfoTip label={NR_FILES.LOCKED_ARIA}>{NR_FILES.LOCKED_TIP}</InfoTip>
    </span>
  );
}
