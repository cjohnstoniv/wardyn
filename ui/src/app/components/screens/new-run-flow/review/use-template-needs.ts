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
// revision once. Drop the read when the origin carries them.
const NONE: TemplateSetupNeed[] = [];

/** What the template left for the person to set that the draft still does not specify. */
export function useTemplateNeeds(origin: NewRunOrigin, intent: RunIntent): TemplateSetupNeed[] {
  const [read, setRead] = React.useState<{ key: string; needs: TemplateSetupNeed[] }>({ key: "", needs: NONE });
  const key = origin.kind === "template" ? `${origin.id}:${origin.revision}` : "";
  React.useEffect(() => {
    if (origin.kind !== "template") return;
    const controller = new AbortController();
    templateSource.read(origin.id, origin.revision, controller.signal).then(
      (t) => { if (!controller.signal.aborted) setRead({ key, needs: t.document.needs_setup ?? NONE }); },
      () => {},
    );
    return () => controller.abort();
    // The origin's identity is its key.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key]);
  return read.key === key ? read.needs.filter((n) => !specifies(intent, n.field)) : NONE;
}
