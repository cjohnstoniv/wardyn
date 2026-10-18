/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Which lines of the internal-host-suffixes box get a coverage line (#2130). Only a line that can be a
// hostname does: a wildcard, scheme, port, path or IP address is refused on save (egress_baseline.go
// validBaselineName), so describing its reach would be wrong, e.g. "*.corp.example" as "(*.*.corp.example)".
// Same shape as hostrules.ValidApprovedHost (hostrules.go:17-23): a dot is required, so a bare word gets no line.
const HOSTNAME = /^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$/;
const IPV4 = /^\d+(\.\d+){3}$/;

/** The entries to describe, in order, as the server would store them (lowercased); invalid lines are left out. */
export function coveredSuffixes(lines: readonly string[]): string[] {
  return lines.map((l) => l.toLowerCase()).filter((l) => l.includes(".") && HOSTNAME.test(l) && !IPV4.test(l));
}
