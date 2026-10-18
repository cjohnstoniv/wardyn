/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// F5-F3. Removing an allowed host is PUT
// .../approved-egress plus, for an operator-authored requirements row, PUT
// .../requirements. A row that is in NEITHER — a host the workspace's own scan
// seeded — has no write to make: the pair fired, nothing changed, and the row
// came back with its old provenance.
export const EGRESS = {
  // The Allowed hosts card's subtitle. Rows come from approvals, recordings,
  // scans and composed sources, so it claims none of those as the only origin.
  ALLOWED_HOSTS_SUBTITLE: "Every run against this workspace may reach these. Each row says where it came from.",
  SCAN_SEEDED_REASON: "Detected by this workspace's scan, not approved here — the next scan puts it back.",
  // The other half of the same mismatch: an operator-authored requirement that
  // reaches this workspace through the EFFECTIVE fold rather than its own
  // overlay. The requirements PUT here replaces only the overlay, so clearing
  // it has to happen where it was written. Distinct from the scan sentence
  // above, which would contradict the row's own "required by this workspace".
  INHERITED_REASON: "Required by a source this workspace composes, not by an approval here — clear it where it was written.",
} as const;

