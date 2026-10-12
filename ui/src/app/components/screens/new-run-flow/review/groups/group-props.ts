/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type * as React from "react";
import type { PolicyPreviewResult } from "../../../../../lib/types/policy-preview";
import type { DraftAction } from "../../draft/draft-reducer";
import type { NewRunLocation } from "../../draft/new-run-url";
import type { NewRunDraft } from "../../draft/run-intent";
import type { DocRow } from "../review-model";
import type { ReviewPreflight } from "../review-seams";

/** What every group reads: the draft, the current server answers (null until they arrive), and the ways out. */
export interface GroupProps {
  draft: NewRunDraft;
  pre: ReviewPreflight | null;
  preview: PolicyPreviewResult | null;
  docRows: readonly DocRow[];
  go: (loc: NewRunLocation) => void;
  dispatch: React.Dispatch<DraftAction>;
}

export const stepLocation = (step: "run" | "harnesses" | "files" | "tools"): NewRunLocation => ({ screen: "step", step });
