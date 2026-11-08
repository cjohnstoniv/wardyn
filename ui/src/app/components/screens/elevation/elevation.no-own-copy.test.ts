/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe } from "vitest";
import { expectNoOwnCopy } from "../../../lib/test-fixtures";

// The elevation dialog renders copy from wardyn/copy/elevation.ts and never authors any.
describe("elevation screens: canon", () => {
  expectNoOwnCopy("src/app/components/screens/elevation", ["elevation-request-dialog.tsx"]);
});
