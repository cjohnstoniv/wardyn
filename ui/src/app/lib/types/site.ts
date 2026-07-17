/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Site config — the operator-wide baseline every run inherits (mirrors
// internal/types.go's SiteConfig EXACTLY, incl. json tags). GET/PUT
// /api/v1/site-config (admin-gated write). Secret VALUES are never
// included on the wire — only the ref NAMES the broker/proxy resolve at
// dispatch/injection time.
export interface ArtifactOverride {
  base_url: string;
  token_secret_ref?: string;
}

export interface SiteConfig {
  upstream_proxy_secret_ref?: string;
  artifact_overrides?: Record<string, ArtifactOverride>;
  scm_hosts?: string[];
}
