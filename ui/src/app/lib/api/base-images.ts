/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The base-image catalog (internal/api/base_images.go), behind the Images tab.
// An admin reads and writes every kind; a security admin lists, adds and checks
// again prewrapped images only (design final §3).
import type { BaseImageEntry } from "../types";
import { asJson, unwrapList, wfetch } from "./core";

export interface BaseImageCatalog {
  rows: BaseImageEntry[];
  // Whether this control plane has an image builder to add Wardyn's tools to the
  // other kinds. An older daemon sends nothing, and it had only those kinds.
  imageBuilder: boolean;
  // Why this control plane cannot check a prewrapped image; "" when it can. An older daemon sends nothing.
  prewrappedUnavailable: string;
}

export const baseImages = {
  // GET /api/v1/base-images -> {base_images: [...], image_builder, prewrapped_check_unavailable}.
  async list(): Promise<BaseImageCatalog> {
    const res = await wfetch("/base-images", { method: "GET" });
    const body = await asJson<{ base_images?: unknown; image_builder?: boolean; prewrapped_check_unavailable?: string }>(res);
    return {
      rows: unwrapList<BaseImageEntry>(body.base_images),
      imageBuilder: body.image_builder ?? true,
      prewrappedUnavailable: body.prewrapped_check_unavailable ?? "",
    };
  },

  // POST /api/v1/base-images. "byo" is the kind that means the reference is
  // used as written; the server names the row from the reference's last path
  // segment and upserts on (kind, image), so adding a listed ref twice is one row.
  // "prewrapped" also starts its check on the cluster: the row comes back checking.
  async add(image: string, kind: "byo" | "prewrapped" = "byo"): Promise<BaseImageEntry> {
    const res = await wfetch("/base-images", { method: "POST", asPerson: true, body: JSON.stringify({ kind, image }) });
    return asJson<BaseImageEntry>(res);
  },

  // POST /api/v1/base-images/{id}/check: a refused or interrupted prewrapped row only.
  async checkAgain(id: string): Promise<BaseImageEntry> {
    const res = await wfetch(`/base-images/${encodeURIComponent(id)}/check`, { method: "POST", asPerson: true });
    return asJson<BaseImageEntry>(res);
  },
};
