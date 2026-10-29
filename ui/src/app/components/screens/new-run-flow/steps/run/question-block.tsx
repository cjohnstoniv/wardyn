/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { ReactNode } from "react";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { StepSection } from "../../../../wardyn/step-section";

/** One question of the Run step as the guided steps' StepSection: the question as the title, "Required" (or the marker given) as its marker. */
export function QuestionBlock({ id, labelId, label, marker = NR_SHARED.REQUIRED, children }: { id?: string; labelId?: string; label: string; marker?: string; children: ReactNode }) {
  return (
    <div id={id}>
      <StepSection title={label} titleId={labelId} titleAs="div" marker={marker}>
        {children}
      </StepSection>
    </div>
  );
}
