/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe } from "vitest";
import { expectNoOwnCopy } from "../../../lib/test-fixtures";

// The elevation screens render copy from wardyn/copy/elevation.ts and never author any.
describe("elevation screens: canon", () => {
  expectNoOwnCopy("src/app/components/screens/elevation", ["elevation-request-dialog.tsx", "my-elevation-requests.tsx", "elevation-decider-card.tsx"]);
});
