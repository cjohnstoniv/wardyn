/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Where the AI composer lives and how a door names what it should work on. A door builds its link with
// composerHref; the composer screen reads the same query back with composerTarget, so the two cannot drift.
import * as React from "react";
import { assistant, type AssistantAvailability, type AssistantPurpose, type AssistantTarget } from "./api/assistant";

export const COMPOSER_PATH = "/compose";

const KINDS: readonly AssistantTarget["kind"][] = ["run", "unit", "template", "change"];

/** A change is explained; everything else is drafted. */
export function composerPurpose(target: AssistantTarget): AssistantPurpose {
  return target.kind === "change" ? "review" : "compose";
}

/** The link of a door: the composer route with its target in the query. A new draft names no id. */
export function composerHref(target: AssistantTarget): string {
  const q = new URLSearchParams({ kind: target.kind });
  if (target.id) q.set("id", target.id);
  return `${COMPOSER_PATH}?${q.toString()}`;
}

/** The target a composer URL names; a missing or unknown kind is a new run draft. */
export function composerTarget(search: string): AssistantTarget {
  const q = new URLSearchParams(search);
  const kind = KINDS.find((k) => k === q.get("kind")) ?? "run";
  const id = q.get("id");
  return id ? { kind, id } : { kind };
}

/**
 * A door's one read of whether the assistant is ready for a purpose, made once on mount. Until the server answers, and
 * when it cannot, the answer is null and the door stays closed.
 */
export function useAssistantDoor(purpose: AssistantPurpose): AssistantAvailability | null {
  const [answer, setAnswer] = React.useState<AssistantAvailability | null>(null);
  React.useEffect(() => {
    let live = true;
    assistant
      .getMyAssistant(purpose)
      .then((a) => live && setAnswer(a))
      .catch(() => live && setAnswer(null));
    return () => {
      live = false;
    };
  }, [purpose]);
  return answer;
}
