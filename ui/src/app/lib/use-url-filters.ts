/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { useInRouterContext, useSearchParams } from "react-router-dom";

/** Changes to the query string: a value of "" removes that key. */
export type FilterPatch = Record<string, string>;
export type SetFilters = (patch: FilterPatch, opts?: { push?: boolean }) => void;

function apply(prev: URLSearchParams, patch: FilterPatch): URLSearchParams {
  const next = new URLSearchParams(prev);
  for (const [k, v] of Object.entries(patch)) {
    if (v) next.set(k, v);
    else next.delete(k);
  }
  return next;
}

function useRouted(): [URLSearchParams, SetFilters] {
  const [params, setParams] = useSearchParams();
  const set = React.useCallback<SetFilters>(
    (patch, opts) => setParams((prev) => apply(prev, patch), { replace: !opts?.push }),
    [setParams],
  );
  return [params, set];
}

// A screen mounted with no router (a test, or a shell that has none) keeps the same filters in memory.
function useDetached(): [URLSearchParams, SetFilters] {
  const [params, setParams] = React.useState(() => new URLSearchParams());
  const set = React.useCallback<SetFilters>((patch) => setParams((prev) => apply(prev, patch)), []);
  return [params, set];
}

/**
 * A list screen's search text and facets, kept in the URL so Back from a row lands on the same filtered list.
 * A change replaces the history entry (typing does not stack entries) unless the caller asks to `push` one,
 * as a drill-in does so that Back removes it. One call sets any number of keys, so a clear is one navigation.
 */
export function useUrlFilters(): [URLSearchParams, SetFilters] {
  const routed = useInRouterContext();
  // `routed` cannot change while a tree stays mounted, so the same hook runs on every render.
  // eslint-disable-next-line react-hooks/rules-of-hooks
  return routed ? useRouted() : useDetached();
}
