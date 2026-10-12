/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import type { TemplateSetupNeed } from "../../../../lib/types/templates";
import type { NewRunOrigin, RunIntent } from "../draft/run-intent";
import { templateSource } from "../steps/shape/shape-sources";
import { specifies } from "./blockers";

// Seam: the draft's template origin does not carry the template's needs_setup yet, so Review reads the pinned
// revision. Drop the read when the origin carries them.
const NONE: TemplateSetupNeed[] = [];

export interface TemplateNeeds {
  /** What the template left for the person to set that the draft still does not specify. */
  needs: TemplateSetupNeed[];
  /** A template's needs are not known yet, or could not be read. Launch stays shut: the server does not check them. */
  unread: boolean;
  /** Reads them again (Check again). */
  retry: () => void;
}

export function useTemplateNeeds(origin: NewRunOrigin, intent: RunIntent): TemplateNeeds {
  const [attempt, setAttempt] = React.useState(0);
  const [read, setRead] = React.useState<{ key: string; needs?: TemplateSetupNeed[] }>({ key: "" });
  const key = origin.kind === "template" ? `${origin.id}:${origin.revision}:${attempt}` : "";
  React.useEffect(() => {
    if (origin.kind !== "template") return;
    const controller = new AbortController();
    templateSource.read(origin.id, origin.revision, controller.signal).then(
      (t) => { if (!controller.signal.aborted) setRead({ key, needs: t.document.needs_setup ?? NONE }); },
      () => { if (!controller.signal.aborted) setRead({ key }); },
    );
    return () => controller.abort();
    // The origin's identity and the attempt are the key.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key]);
  const retry = React.useCallback(() => setAttempt((n) => n + 1), []);
  if (key === "") return { needs: NONE, unread: false, retry };
  const answered = read.key === key ? read.needs : undefined;
  return { needs: answered ? answered.filter((n) => !specifies(intent, n.field)) : NONE, unread: answered === undefined, retry };
}
