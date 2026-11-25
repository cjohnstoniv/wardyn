/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe } from "vitest";
import { expectNoOwnCopy } from "../../../lib/test-fixtures";

// The change review sections and a change's own page render copy from change-review-copy.ts and the canon it reads,
// never their own.
describe("The change review sections render no copy of their own", () => {
  expectNoOwnCopy("src/app/components/screens/governance", [
    "change-review-sections.tsx",
    "change-review-page.tsx",
    "change-approvals-panel.tsx",
    "change-decision-footer.tsx",
    "change-review-slots.tsx",
  ]);
});
