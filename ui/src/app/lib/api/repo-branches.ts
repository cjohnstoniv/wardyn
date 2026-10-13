/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The branch picker's list: GET /workspaces/{id}/branches?repo=<repo>
// (internal/api/workspace_branches.go, RepoBranches). A listing that fails is
// still a 200 with listing "unavailable"; the Files step then shows the text
// field. A 404 (workspace not launchable), 422 (workspace_repo_unknown) or 429
// (branch_list_rate_limited) throws HttpError like any other call.
import { asJson, wfetch } from "./core";

export type BranchListing = "listed" | "unavailable";

/** Whose credential listed the branches. */
export type BranchCredential = "ado_signin" | "ado_own_token" | "github_app";

/** Why a listing is unavailable; every one falls back to the text field. */
export type BranchUnavailableReason =
  | "forge_not_supported"
  | "no_credential"
  | "signin_needed"
  | "forge_refused"
  | "forge_unreachable";

export interface RepoBranches {
  listing: BranchListing;
  /** Never null: the default branch first, then by name, at most 200. */
  branches: string[];
  default?: string;
  truncated: boolean;
  credential?: BranchCredential;
  reason?: BranchUnavailableReason;
}

export const repoBranches = {
  async list(workspaceId: string, repo: string): Promise<RepoBranches> {
    const res = await wfetch(
      `/workspaces/${encodeURIComponent(workspaceId)}/branches?repo=${encodeURIComponent(repo)}`,
      { method: "GET" },
    );
    return asJson<RepoBranches>(res);
  },
};
