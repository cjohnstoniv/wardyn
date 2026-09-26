/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The base-image catalog (internal/api/base_images.go), behind the Images tab.
// Both routes are operatorOnly.
import type { BaseImageEntry } from "../types";
import { asJson, unwrapList, wfetch } from "./core";

export const baseImages = {
  // GET /api/v1/base-images -> {base_images: [...]}.
  async list(): Promise<BaseImageEntry[]> {
    const res = await wfetch("/base-images", { method: "GET" });
    return unwrapList<BaseImageEntry>((await asJson<{ base_images?: unknown }>(res)).base_images);
  },

  // POST /api/v1/base-images. "byo" is the kind that means the reference is
  // used as written; the server names the row from the reference's last path
  // segment and upserts on (kind, image), so adding a listed ref twice is one row.
  async add(image: string): Promise<BaseImageEntry> {
    const res = await wfetch("/base-images", { method: "POST", body: JSON.stringify({ kind: "byo", image }) });
    return asJson<BaseImageEntry>(res);
  },
};
