/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe } from "vitest";
import { expectNoOwnCopy } from "../../../lib/test-fixtures";

// The Approvals needed tab renders copy from approvals-needed-copy.ts and governance-copy.ts, never its own.
describe("The Approvals needed tab renders no copy of its own", () => {
  expectNoOwnCopy("src/app/components/screens/governance", ["approvals-needed.tsx"]);
});
