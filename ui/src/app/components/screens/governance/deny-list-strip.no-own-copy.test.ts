/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe } from "vitest";
import { expectNoOwnCopy } from "../../../lib/test-fixtures";

// The deny-list strips and the standalone rows render copy from copy/deny-lists.ts and never author their own.
describe("The deny-list strip renders no copy of its own", () => {
  expectNoOwnCopy("src/app/components/screens/governance", ["deny-list-strip.tsx"]);
});
