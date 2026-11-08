/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe } from "vitest";
import { expectNoOwnCopy } from "../../../lib/test-fixtures";

// The Deciders section renders copy from wardyn/copy/deciders.ts and never authors its own.
describe("The Deciders section renders no copy of its own", () => {
  expectNoOwnCopy("src/app/components/screens/governance", ["unit-deciders.tsx"]);
});
