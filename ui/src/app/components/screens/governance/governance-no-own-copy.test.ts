/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe } from "vitest";
import { expectNoOwnCopy } from "../../../lib/test-fixtures";

// THE canon pin: a component in this directory may render copy, never author
// it. Anything a reader sees comes from governance-copy.ts (frozen §7) or the
// modules §7.1 defers to — so a quoted or bare prose string in this source is a
// canon break, not a style question.
describe("Governance components render no copy of their own", () => {
  // The vitest project root (ui/) is the resolve base: vite rewrites
  // import.meta.url to a root-relative URL.
  expectNoOwnCopy("src/app/components/screens/governance", [
    "governance-screen.tsx",
    "profile-editor.tsx",
    "profile-rubric.tsx",
    "profile-overlay.tsx",
    "assignments.tsx",
    "display.tsx",
    "runner-credentials.tsx",
    "egress-baseline.tsx",
  ]);
});
