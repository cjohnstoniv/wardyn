/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Preview URLs: a URL on a port a running run serves (internal/api/run_previews.go).

/** Go: types.PreviewVisibility. Who may open a preview; each is wider than the one before it. */
export type PreviewVisibility = "private" | "org" | "public";

/** Go: types.RunPreview. The wire never carries the link hash. */
export interface RunPreview {
  id: string;
  run_id: string;
  port: number;
  visibility: PreviewVisibility;
  created_by: string;
  created_at: string;
  updated_at: string;
  expires_at: string;
  /** Set once the preview is revoked; the list answers live rows only. */
  revoked_at?: string;
}

/** The PUT body. `ttl_seconds` is 3600 to 604800. */
export interface PreviewWrite {
  visibility: PreviewVisibility;
  ttl_seconds: number;
}

/** The reason a 404 from the ticket route carries once a preview has ended, was revoked or does not admit the caller. */
export const PREVIEW_ENDED_REASON = "run_not_found";
