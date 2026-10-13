/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Whether a string is held anywhere a rendered React tree keeps it: a fiber's props, or the value of one of
// its hooks (useState, useRef, useMemo and the rest). The DOM and `textContent` miss a secret a component
// keeps in state but does not draw; this finds it. Closures are not searched.
import type { ReactNode } from "react";

interface Fiber {
  child: Fiber | null;
  sibling: Fiber | null;
  memoizedProps: unknown;
  memoizedState: unknown;
  tag: number;
}

// Function, forwardRef and memo components keep hooks as a linked list on memoizedState.
const HOOK_TAGS = new Set([0, 11, 14, 15]);
const SKIP = new Set(["_owner", "_store", "stateNode", "return", "alternate", "ref", "$$typeof"]);

function holds(value: unknown, needle: string, seen: WeakSet<object>, depth: number): boolean {
  if (typeof value === "string") return value.includes(needle);
  if (value === null || typeof value !== "object" || depth > 8 || seen.has(value)) return false;
  if (typeof Node !== "undefined" && value instanceof Node) return false;
  seen.add(value);
  return Object.entries(value).some(([k, v]) => !SKIP.has(k) && holds(v, needle, seen, depth + 1));
}

/** True when `needle` is in the props or hook state of any component rendered under `container`. */
export function holdsText(container: Element, needle: string): boolean {
  const key = Object.keys(container).find((k) => k.startsWith("__reactContainer$"));
  if (!key) throw new Error("holdsText: the container has no React root");
  const seen = new WeakSet<object>();
  const walk = (f: Fiber | null): boolean => {
    for (let cur = f; cur; cur = cur.sibling) {
      if (holds(cur.memoizedProps as ReactNode, needle, seen, 0)) return true;
      if (HOOK_TAGS.has(cur.tag)) {
        for (let hook = cur.memoizedState as { memoizedState: unknown; next: unknown } | null; hook; hook = hook.next as typeof hook) {
          if (holds(hook.memoizedState, needle, seen, 0)) return true;
        }
      } else if (holds(cur.memoizedState, needle, seen, 0)) return true;
      if (walk(cur.child)) return true;
    }
    return false;
  };
  return walk((container as unknown as Record<string, Fiber>)[key].child);
}
