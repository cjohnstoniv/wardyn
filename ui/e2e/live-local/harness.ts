/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Shared inputs for the live-local browser suites. Secrets arrive as FILE
 * PATHS only (WARDYN_LIVE_IDENTITIES_FILE); values are never printed. Assertions
 * compare categories ("wardyn", "entra", "other") rather than raw URLs, so a
 * failure message cannot carry a tenant id, a login hint or a code.
 */

import fs from "node:fs";
import type { Page } from "@playwright/test";

/** The variable names a suite needs, and which of them are unset. */
export function missing(gate: string, names: string[]): string | null {
  const unset = [gate, ...names].filter((n) =>
    n === gate ? process.env[n] !== "1" : !process.env[n],
  );
  return unset.length
    ? `live: needs ${gate}=1 and ${names.join(", ")}; unset: ${unset.join(", ")} (docs/LIVE-TESTS.md)`
    : null;
}

export interface Identity {
  storage_state?: string;
}

/** WARDYN_LIVE_IDENTITIES_FILE: `{ "admin": {...}, "member": {...}, "norole": {...} }`. */
export function identities(): Record<string, Identity> {
  const file = process.env.WARDYN_LIVE_IDENTITIES_FILE || "";
  if (!file) throw new Error("WARDYN_LIVE_IDENTITIES_FILE is unset");
  return JSON.parse(fs.readFileSync(file, "utf8")) as Record<string, Identity>;
}

/** Where a URL points, as a category only. */
export function where(
  url: string,
  wardynBase?: string,
): "wardyn" | "entra" | "other" {
  let host = "";
  try {
    host = new URL(url).host;
  } catch {
    return "other";
  }
  if (wardynBase && host === new URL(wardynBase).host) return "wardyn";
  if (/^login\.(microsoftonline|microsoft|windows)\.(com|net)$/.test(host))
    return "entra";
  return "other";
}

export function whereIs(page: Page, wardynBase: string) {
  return where(page.url(), wardynBase);
}
