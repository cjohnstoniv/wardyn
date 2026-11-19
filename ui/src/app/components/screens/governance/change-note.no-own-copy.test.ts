/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe } from "vitest";
import { expectNoOwnCopy } from "../../../lib/test-fixtures";

// The change-note surfaces render copy from governance-copy.ts and wardyn/copy/change-note.ts, never their own.
describe("The change note components render no copy of their own", () => {
  expectNoOwnCopy("src/app/components/screens/governance", ["changes.tsx", "change-note-form.tsx", "submitted-note.tsx"]);
});
