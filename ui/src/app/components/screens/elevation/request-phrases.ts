/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The words the elevation screens use for who decides a request and how long it is for.

import { formatDuration } from "../../../lib/format";
import { goDurationMs } from "../../../lib/go-duration";
import type { GovernanceChangeApprover } from "../../../lib/types/governance-change";
import { joinOr } from "../../wardyn/decider-lines";

// request-phrases.test.ts and the elevation screens read it from here.
export { joinOr };

function deciderPhrase(d: GovernanceChangeApprover): string {
  if (d.kind === "security") return "a security admin";
  if (d.kind === "unit_admin") return `admins of ${d.unit_name || "this unit"}`;
  return `the ${d.name} group`;
}

// Who can decide a request, as the Decided by line says it.
export function decidersPhrase(d: GovernanceChangeApprover[]): string {
  return joinOr(d.map(deciderPhrase));
}

// A stored duration in a sentence ("4h" is "4 hours"). A value it cannot read comes back as it is.
export function lengthWords(duration: string): string {
  const ms = goDurationMs(duration);
  return Number.isNaN(ms) ? duration : formatDuration(ms, "words");
}
