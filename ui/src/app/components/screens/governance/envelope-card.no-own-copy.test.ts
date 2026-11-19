/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe } from "vitest";
import { expectNoOwnCopy } from "../../../lib/test-fixtures";

// The envelope card renders copy from wardyn/copy/envelope.ts and never authors it.
describe("The envelope card renders no copy of its own", () => {
  expectNoOwnCopy("src/app/components/screens/governance", ["envelope-card.tsx"]);
});
