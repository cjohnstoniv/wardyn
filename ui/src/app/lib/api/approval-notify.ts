/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// GET /api/v1/approval-notify/status (notify-e4) — each configured approval
// notification channel's delivery health, for Settings' read-only card. Security
// tier on the server; the card is mounted for super admins only. Mirrors
// internal/api/approval_notify_status.go. destination_host is a hostname and
// never a URL; last_error is a failure class such as "http_status:503".
import { asJson, wfetch } from "./core";

export interface ApprovalNotifyChannelStatus {
  id: string;
  type: "webhook" | "teams" | "slack" | "smtp" | (string & {});
  destination_host: string;
  last_success_at?: string;
  last_error?: string;
  last_error_at?: string;
  failed_last_hour: number;
}

export interface ApprovalNotifyStatus {
  /** Empty when notifications are not configured. */
  channels: ApprovalNotifyChannelStatus[];
}

export const approvalNotify = {
  async getStatus(): Promise<ApprovalNotifyStatus> {
    const res = await wfetch("/approval-notify/status", { method: "GET" });
    return asJson<ApprovalNotifyStatus>(res);
  },
};
