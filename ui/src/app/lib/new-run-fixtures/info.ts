/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Info tab: run metadata only (Title, Description) for now. It adds nothing
// to the request contract.
import { preview, APPLIED_ORG, type FixtureBody } from "./base";

export const INFO_FIXTURE: FixtureBody = {
  route: "info/title-description",
  note: "Info: Title and Description only; the description will seed the AI composer.",
  preview: preview({ resources_applied: APPLIED_ORG }),
};
