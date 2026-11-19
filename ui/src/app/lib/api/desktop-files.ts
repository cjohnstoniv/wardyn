/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { ApprovalRequest } from "../types/approvals";
import type { DesktopControls } from "../types/policy";
import { asJson, wfetch } from "./core";

/** GET /runs/{id}/desktop/files: the run's effective desktop rules, and the transfer folder's files
 *  only when download is allowed or reviewed (mirrors Go's desktopFilesView). */
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
  // The body is the file itself; the server refuses one over its limit before anything is sent. `person`: a file
  // after the first names the person the upload began under (core.ts WfetchInit.asPerson).
  async upload(runId: string, file: File, person?: string): Promise<DesktopTransfer> {
    const res = await wfetch(`${base(runId)}/${encodeURIComponent(file.name)}`, {
      method: "PUT",
      asPerson: person ?? true,
      body: file,
      headers: { "Content-Type": "application/octet-stream" },
    });
    return asJson<DesktopTransfer>(res);
  },
  // Under desktop.download: review, a person asks for one file; the answer is the PENDING approval.
  async review(runId: string, name: string): Promise<ApprovalRequest> {
    return asJson<ApprovalRequest>(await wfetch(`${base(runId)}/${encodeURIComponent(name)}/review`, { method: "POST", asPerson: true }));
  },
  // `approvalId` names the approved review that releases the file, once.
  async download(runId: string, name: string, approvalId?: string): Promise<{ blob: Blob; sha256: string }> {
    const query = approvalId ? `?approval=${encodeURIComponent(approvalId)}` : "";
    const res = await wfetch(`${base(runId)}/${encodeURIComponent(name)}${query}`, { method: "GET", headers: { Accept: "application/octet-stream" } });
    if (!res.ok) await asJson(res);
    const blob = await res.blob();
    const digest = await crypto.subtle.digest("SHA-256", await blob.arrayBuffer());
    const sha256 = Array.from(new Uint8Array(digest), (b) => b.toString(16).padStart(2, "0")).join("");
    return { blob, sha256 };
  },
};
