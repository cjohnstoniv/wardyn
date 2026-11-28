/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { POLICY_TAB } from "../../screens/run-detail/policy-tab-copy";
import { Chip } from "../primitives";

// A value the reader may not see. Focusable, so the reason is reachable without a pointer.
export function HiddenChip({ tip = POLICY_TAB.hiddenTip }: { tip?: string }) {
  return (
    <span title={tip} tabIndex={0} role="note" aria-label={`${POLICY_TAB.hidden}. ${tip}`}>
      <Chip tone="neutral">{POLICY_TAB.hidden}</Chip>
    </span>
  );
}
