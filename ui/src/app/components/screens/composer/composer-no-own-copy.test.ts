/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe } from "vitest";
import { expectNoOwnCopy } from "../../../lib/test-fixtures";

// A component in this directory may render copy, never author it. Everything a reader sees comes from copy/assistant.ts
// or the modules it reuses, so a quoted or bare prose string in these sources is a break, not a style question.
describe("Composer components render no copy of their own", () => {
  // The vitest project root (ui/) is the resolve base.
  expectNoOwnCopy("src/app/components/screens/composer", [
    "composer-screen.tsx",
    "composer-chat.tsx",
    "composer-stream.ts",
    "composer-preview.tsx",
    "composer-starting.tsx",
    "composer-trail.tsx",
    "composer-draft-panel.tsx",
    "composer-phone.tsx",
  ]);
});
