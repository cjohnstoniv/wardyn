/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Workspace providers (0.7.2) — the two operatorOnly routes behind the
// /providers screen. Mirrors internal/api/workspace_providers.go +
// internal/types/workspace_provider.go; every route is under /api/v1 via
// wfetch.
//
// The wire types are lib/types/site.ts's WorkspaceProviders/GitProvider/
// StorageProviders/EphemeralProvider/UserDriveProvider (A1's mirror, shared
// with SiteConfig.workspace_providers) — imported, not re-declared here, for
// the reason lib/api/drives.ts's header states for its own hand-maintained
// mirrors: one shape, one place it can drift.
import type {
  EphemeralProvider,
  GitLane,
  GitProvider,
  GitProviderKind,
  StorageProviders,
  UserDriveProvider,
  WorkspaceProviders,
} from "../types/site";
import { asJson, wfetch } from "./core";

export type { EphemeralProvider, GitLane, GitProvider, GitProviderKind, StorageProviders, UserDriveProvider, WorkspaceProviders };

// GET's response, with the ETag the console keeps and sends back as If-Match
// on every PUT (§9.4) — never a silent overwrite. A block with no rows and no
// storage policy is legacy open mode, not an error.
export interface WorkspaceProvidersSnapshot {
  providers: WorkspaceProviders;
  etag: string | null;
}

// PUT's response: the persisted block, its fresh ETag, and the one advisory
// signal the write produces — how many already-onboarded repo locators this
// block now refuses (workspace_providers.go's workspaceProvidersPutResponse).
// Always present, including as 0: "0" is the reassurance an admin narrowing a
// base URL is looking for.
export interface WorkspaceProvidersSaveResult {
  providers: WorkspaceProviders;
  etag: string | null;
  sourcesNoLongerAdmitted: number;
}

export const providers = {
  // GET /api/v1/workspace-providers — operatorOnly, like GET /site-config: a
  // base URL names corporate topology.
  async getWorkspaceProviders(): Promise<WorkspaceProvidersSnapshot> {
    const res = await wfetch("/workspace-providers");
    const body = await asJson<WorkspaceProviders>(res);
    return { providers: body, etag: res.headers.get("ETag") };
  },

  // PUT /api/v1/workspace-providers — replaces the WHOLE document; there is no
  // delete route, removing a row is a PUT without it. `etag` is the last GET's
  // (or the last successful PUT's) — sent as If-Match so a stale write is
  // refused 412 rather than silently clobbering someone else's save (rendered
  // by the caller as the saved-elsewhere state, never retried automatically).
  // Absent (null) behaves as last-writer-wins, exactly like PUT /site-config.
  async putWorkspaceProviders(next: WorkspaceProviders, etag: string | null): Promise<WorkspaceProvidersSaveResult> {
    const headers: Record<string, string> = {};
    if (etag) headers["If-Match"] = etag;
    const res = await wfetch("/workspace-providers", {
      method: "PUT",
      headers,
      body: JSON.stringify(next),
    });
    // A 412 (stale If-Match) surfaces here as an HttpError carrying the
    // server's own "providers changed since you loaded them — reload and
    // retry" body — the caller renders that as the saved-elsewhere state,
    // never retries automatically.
    const body = await asJson<WorkspaceProviders & { sources_no_longer_admitted: number }>(res);
    const { sources_no_longer_admitted, ...rest } = body;
    return {
      providers: rest,
      etag: res.headers.get("ETag"),
      sourcesNoLongerAdmitted: sources_no_longer_admitted,
    };
  },
};
