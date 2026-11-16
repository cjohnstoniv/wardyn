/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";

// The two lists a decided request can be drawn from: an approval (the Pending and Waiting on you lists) or a
// governance request (the elevation list).
export type HeldList = "approvals" | "elevation";
export type ListsLanded = Record<HeldList, boolean>;

// The longest a request stays held when no read of its list lands: a read that keeps failing frees the card, and the
// server refuses a second decision on a request already decided.
export const READ_HOLD_MS = 30_000;

type Hold = { list: HeldList; after: number; timer: ReturnType<typeof setTimeout> };

// useReadHold is the Approvals screen's set of requests with a decision out, from the click until a read of the
// request's list that began after the decision has landed. The card's own busy state is lost when a view switch
// remounts it, and is already clear when the unawaited read after a decision starts, so this screen-level set keeps
// every view's card for the request disabled through both. A read that fails, or that a newer read superseded, frees
// nothing: the decided request would be drawn with live buttons from the list shown before it.
//
// begin numbers a read of the lists; landedAt says which of them landed. holdUntilRead frees a request at once if a
// read numbered above after has landed for its list, or else when one does, or after READ_HOLD_MS.
export function useReadHold() {
  const [held, setHeld] = React.useState<ReadonlySet<string>>(() => new Set());
  const holds = React.useRef(new Map<string, Hold>());
  const begun = React.useRef(0);
  const landed = React.useRef<Record<HeldList, number>>({ approvals: 0, elevation: 0 });
  const mark = React.useCallback((id: string) => setHeld((cur) => new Set(cur).add(id)), []);
  const release = React.useCallback((id: string) => {
    const h = holds.current.get(id);
    if (h) clearTimeout(h.timer);
    holds.current.delete(id);
    setHeld((cur) => {
      if (!cur.has(id)) return cur;
      const next = new Set(cur);
      next.delete(id);
      return next;
    });
  }, []);
  const begin = React.useCallback(() => ++begun.current, []);
  const newestRead = React.useCallback(() => begun.current, []);
  const landedAt = React.useCallback(
    (read: number, lists: ListsLanded) => {
      for (const list of ["approvals", "elevation"] as const) {
        if (lists[list]) landed.current[list] = Math.max(landed.current[list], read);
      }
      for (const [id, h] of [...holds.current]) if (landed.current[h.list] > h.after) release(id);
    },
    [release],
  );
  const holdUntilRead = React.useCallback(
    (id: string, list: HeldList, after: number) => {
      if (landed.current[list] > after) {
        release(id);
        return;
      }
      const prev = holds.current.get(id);
      if (prev) clearTimeout(prev.timer);
      holds.current.set(id, { list, after, timer: setTimeout(() => release(id), READ_HOLD_MS) });
    },
    [release],
  );
  React.useEffect(() => {
    const map = holds.current;
    return () => {
      for (const h of map.values()) clearTimeout(h.timer);
      map.clear();
    };
  }, []);
  return { held, mark, release, begin, newestRead, landedAt, holdUntilRead };
}
