/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The macOS bases a New Run offers for the chosen pool (board J6): Standard, then the pool's other bases, each with
// what it is and, for one a person cannot pick, why. Only what the pool wire says: a base is pickable when it is ready
// on every Mac of the pool and the caller's profile lets them pick it (MAC-B05, the server's own answer per base, from the
// rule create holds them to); the server's MAC-B09 refusal still speaks at Launch for a draft that names one anyway.

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
  (offer.bases?.length ?? 0) > 0 || !!offer.standardNotAllowed || (!!chosen && chosen !== MAC_BASES.B04);

/**
 * Standard first, then the pool's other bases in the order it sent them. A base the draft names that the pool does
 * not list reads as not on every Mac, so the person sees why the server will refuse it and can pick another.
 */
export function baseChoices(offer: Extract<MacOSOffer, { ok: true }>, chosen: string | undefined, harnessNames: (ids: readonly string[]) => string): BaseChoice[] {
  const others = [...(offer.bases ?? [])];
  const notAllowed = MAC_BASES.B05(offer.profile ?? "");
  // Standard's prepare date is for display; whether a base is too old is the server's refusal, never judged here.
  const standardMeta = MAC_BASES.B03(offer.guestOS || undefined, offer.createdAt ? shortDate(offer.createdAt) : undefined, harnessNames(offer.harnesses) || undefined);
  const out: BaseChoice[] = [{ name: MAC_BASES.B04, meta: standardMeta, reason: offer.standardNotAllowed ? notAllowed : undefined }];
  if (chosen && chosen !== MAC_BASES.B04 && !others.some((b) => b.name === chosen)) others.push({ name: chosen, state: "missing" });
  for (const b of others) {
    // The organisation's rule first, as the board orders the reasons.
    const reason = b.not_allowed ? notAllowed : b.state === "ready" ? undefined : b.state === "rebuilding" ? MAC_BASES.B07(b.progress ?? 0) : MAC_BASES.B06;
    out.push({ name: b.name, meta: MAC_BASES.B03(b.guest_os, b.created_at ? shortDate(b.created_at) : undefined, harnessNames(b.harnesses ?? []) || undefined), reason });
  }
  return out;
}
