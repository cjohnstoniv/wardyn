/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Approval notifications Settings card (notify-e4, packet M6 S3, approved
// 2026-10-03). Read-only: the channels are set by WARDYN_APPROVAL_NOTIFY. The
// two sentences that name a wire literal are split around it, because wire
// values render mono and nothing else does.
export const APPROVAL_NOTIFY = {
  TITLE: "Approval notifications",
  SUMMARY_OK: (n: number) => `${n} channels · all delivering`,
  SUMMARY_FAILED: (n: number, f: number) => `${n} channels · ${f} failed in the last hour`,
  SUMMARY_OFF: "Not set up",
  ENV_NAME: "WARDYN_APPROVAL_NOTIFY",
  LEDE: ["Where pending approvals are announced. Read-only: set by ", "."],
  OFF_BODY: ["Set ", " to announce pending approvals to a webhook, Teams, Slack or email."],
  COLS: {
    CHANNEL: "Channel",
    TYPE: "Type",
    DESTINATION: "Destination",
    DELIVERED: "Last delivered",
    ERROR: "Last error",
    FAILED: "Failed (1h)",
  },
  TYPE: { webhook: "Webhook", teams: "Teams", slack: "Slack", smtp: "Email" } as Record<string, string>,
  NEVER: "Never",
  NONE: "None",
  AUDIT_ACTION: "approval.notify.failed",
  AUDIT_NOTE: ["Each failed delivery is in the audit trail as ", ". The approval still waits in the console."],
} as const;
