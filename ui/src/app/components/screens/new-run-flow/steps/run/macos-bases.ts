/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The macOS bases a New Run offers for the chosen pool (board J6): Standard, then the pool's other bases, each with
// what it is and, for one a person cannot pick, why. Only what the pool wire says: a base is pickable when it is ready
// on every Mac of the pool; the organisation's own rule (MAC-B05) has no wire signal before create, so the server's
// MAC-B09 refusal speaks for it at Launch.

import { shortDate } from "../../../../../lib/format";
import { MAC_BASES } from "../../../../wardyn/copy/mac-bases";
import type { MacOSOffer } from "./macos-offer";

export interface BaseChoice {
  name: string;
  /** MAC-B03: the build, when it was prepared and its harnesses, whichever the wire sent. */
  meta: string;
  /** MAC-B06 or B07 when the base cannot be picked. */
  reason?: string;
}

/** Whether the picker shows: the pool has other bases, or the draft already names one the pool's list lacks. */
export const hasBaseChoice = (offer: Extract<MacOSOffer, { ok: true }>, chosen: string | undefined): boolean =>
  (offer.bases?.length ?? 0) > 0 || (!!chosen && chosen !== MAC_BASES.B04);

/**
 * Standard first, then the pool's other bases in the order it sent them. A base the draft names that the pool does
 * not list reads as not on every Mac, so the person sees why the server will refuse it and can pick another.
 */
export function baseChoices(offer: Extract<MacOSOffer, { ok: true }>, chosen: string | undefined, harnessNames: (ids: readonly string[]) => string): BaseChoice[] {
  const others = [...(offer.bases ?? [])];
  const out: BaseChoice[] = [{ name: MAC_BASES.B04, meta: MAC_BASES.B03(offer.guestOS || undefined, undefined, harnessNames(offer.harnesses) || undefined) }];
  if (chosen && chosen !== MAC_BASES.B04 && !others.some((b) => b.name === chosen)) others.push({ name: chosen, state: "missing" });
  for (const b of others) {
    const reason = b.state === "ready" ? undefined : b.state === "rebuilding" ? MAC_BASES.B07(b.progress ?? 0) : MAC_BASES.B06;
    out.push({ name: b.name, meta: MAC_BASES.B03(b.guest_os, b.created_at ? shortDate(b.created_at) : undefined, harnessNames(b.harnesses ?? []) || undefined), reason });
  }
  return out;
}
