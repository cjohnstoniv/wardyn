/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { DesktopControls } from "../types/policy";
import { asJson, wfetch } from "./core";

/** GET /runs/{id}/desktop/files: the run's effective desktop rules, and the transfer folder's files
 *  only when download is allowed (mirrors Go's desktopFilesView). */
export interface DesktopFilesView {
  folder: string;
  rules: Required<DesktopControls>;
  files: { name: string; bytes: number }[];
}

export interface DesktopTransfer {
  name: string;
  bytes: number;
  sha256: string;
}

const base = (runId: string) => `/runs/${encodeURIComponent(runId)}/desktop/files`;

export const desktopFiles = {
  async list(runId: string, signal?: AbortSignal): Promise<DesktopFilesView> {
    return asJson<DesktopFilesView>(await wfetch(base(runId), { method: "GET", signal }));
  },
  // The body is the file itself; the server refuses one over its limit before anything is sent.
  async upload(runId: string, file: File): Promise<DesktopTransfer> {
    const res = await wfetch(`${base(runId)}/${encodeURIComponent(file.name)}`, {
      method: "PUT",
      body: file,
      headers: { "Content-Type": "application/octet-stream" },
    });
    return asJson<DesktopTransfer>(res);
  },
  async download(runId: string, name: string): Promise<{ blob: Blob; sha256: string }> {
    const res = await wfetch(`${base(runId)}/${encodeURIComponent(name)}`, { method: "GET", headers: { Accept: "application/octet-stream" } });
    if (!res.ok) await asJson(res);
    const blob = await res.blob();
    const digest = await crypto.subtle.digest("SHA-256", await blob.arrayBuffer());
    const sha256 = Array.from(new Uint8Array(digest), (b) => b.toString(16).padStart(2, "0")).join("");
    return { blob, sha256 };
  },
};
