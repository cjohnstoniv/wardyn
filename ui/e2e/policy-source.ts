/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The policy source editor, as the specs reach it (#1921). It opens in YAML;
// "Spec (JSON)" is its label only once JSON is chosen. JSON text is valid YAML,
// so a spec written as JSON still fills the field and reads back the same.
import type { Locator } from "@playwright/test";
import { parse } from "yaml";

export const SPEC_LABEL = /^Spec \((YAML|JSON)\)/;

/** What the source in the editor reads as, whichever format it is written in. */
export async function readSpec<T = Record<string, unknown>>(field: Locator): Promise<T> {
  return parse(await field.inputValue()) as T;
}
