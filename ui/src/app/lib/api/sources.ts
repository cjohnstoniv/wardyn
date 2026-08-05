/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The tier-1 SOURCE LIBRARY + tier-2 IMAGE CATALOG endpoints. A source is a
// repo/dir configured once (its own contract + scan) and attached to many
// workspaces; a catalog image is a shared registry/custom/BYO base. Both
// creates are idempotent by identity server-side — re-adding an existing
// entry answers with the existing row rather than a duplicate.
import type { BaseImageEntry, Source, WorkspaceRequirementsMap } from "../types";
import { asJson, errText, HttpError, wfetch } from "./core";

export const sourcesApi = {
  // GET /api/v1/sources — the whole library.
  async listSources(): Promise<Source[]> {
    const res = await wfetch("/sources", { method: "GET" });
    const body = await asJson<{ sources?: Source[] }>(res);
    return body.sources ?? [];
  },

  // POST /api/v1/sources — upsert by canonical identity (kind, locator, ref).
  async createSource(input: {
    kind: "local_dir" | "repo";
    locator: string;
    ref?: string;
    name?: string;
    requirements?: WorkspaceRequirementsMap;
  }): Promise<Source> {
    const res = await wfetch("/sources", { method: "POST", body: JSON.stringify(input) });
    return asJson<Source>(res);
  },

  // PUT /api/v1/sources/{id}/requirements — full replacement of the source's
  // OWN contract (the same grammar as a workspace's, minus integration: keys,
  // which are tier-3-only).
  async setSourceRequirements(id: string, requirements: WorkspaceRequirementsMap): Promise<Source> {
    const res = await wfetch(`/sources/${encodeURIComponent(id)}/requirements`, {
      method: "PUT",
      body: JSON.stringify({ requirements }),
    });
    return asJson<Source>(res);
  },

  // POST /api/v1/sources/{id}/scan — dir inline (200 + profile), repo as a
  // governed run (202 + scan_run_id). Shape varies; callers refetch the list.
  async scanSource(id: string): Promise<unknown> {
    const res = await wfetch(`/sources/${encodeURIComponent(id)}/scan`, { method: "POST" });
    return asJson<unknown>(res);
  },

  // DELETE /api/v1/sources/{id}[?force=1] — in use answers 409 NAMING the
  // attaching workspaces; force detaches them (their runs then fail loudly at
  // the mount gate rather than silently losing code).
  async deleteSource(id: string, force = false): Promise<void> {
    const res = await wfetch(`/sources/${encodeURIComponent(id)}${force ? "?force=1" : ""}`, {
      method: "DELETE",
    });
    if (!res.ok) throw new HttpError(res.status, await errText(res));
  },
};

export const baseImagesApi = {
  // GET /api/v1/base-images — the catalog.
  async listBaseImages(): Promise<BaseImageEntry[]> {
    const res = await wfetch("/base-images", { method: "GET" });
    const body = await asJson<{ base_images?: BaseImageEntry[] }>(res);
    return body.base_images ?? [];
  },

  // POST /api/v1/base-images — upsert by identity (kind, image, steps).
  async createBaseImage(input: {
    kind: "registry" | "custom" | "byo";
    name?: string;
    image: string;
    steps?: string[];
  }): Promise<BaseImageEntry> {
    const res = await wfetch("/base-images", { method: "POST", body: JSON.stringify(input) });
    return asJson<BaseImageEntry>(res);
  },

  // DELETE /api/v1/base-images/{id}[?force=1] — in use answers 409 naming the
  // workspaces; force detaches them (they fall back to the derived
  // recommended build — a working state, stated as such).
  async deleteBaseImage(id: string, force = false): Promise<void> {
    const res = await wfetch(`/base-images/${encodeURIComponent(id)}${force ? "?force=1" : ""}`, {
      method: "DELETE",
    });
    if (!res.ok) throw new HttpError(res.status, await errText(res));
  },
};
