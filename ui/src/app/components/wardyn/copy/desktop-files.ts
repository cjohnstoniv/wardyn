/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run's Files panel and clipboard strip (mock M7-Files, M7-Phone, approved 2026-10-20). Every
// string here is canon from those boards except the ones marked "not on the boards".
export const DESKTOP_FILES = {
  title: "Files",
  toTab: "To the desktop",
  fromTab: "From the desktop",
  folderLabel: "Transfer folder",
  folderWhere: "in the desktop",
  drop: "Drop files here or choose files",
  dropSub: "They land in the transfer folder.",
  choose: "Choose files",
  sending: "Sending",
  delivered: "Delivered",
  tooLarge: "Too large",
  tooLargeSub: "Over the size limit for this desktop. Nothing was sent.",
  hashLine: (sha: string, at: string) => `sha256 ${sha.slice(0, 4)}…${sha.slice(-4)} · ${at}`,
  everyFile: "Every file is logged by direction, size and hash. Never the content.",
  downloadBlocked: "Blocked: downloading files",
  refused: "Refused by policy",
  blockedEmpty: "Files the agent saves in the transfer folder stay in the desktop. Nothing is listed here.",
  refusedLogged: "Refused attempts are logged by direction and size too.",
  download: "Download",
  downloaded: (at: string, sha: string) => `Downloaded ${at} · sha256 ${sha.slice(0, 4)}…${sha.slice(-4)}`,
  downloadsAllowed: "Downloads were allowed by your team. Each one is logged by size and hash.",
  // Row 29: a VM desktop has no clipboard at all, so the Allowed/Blocked strip would claim a relay that is not there.
  clipboardVM: "Copy and paste: not available on VM desktops yet",
  pasteIn: "Paste in",
  copyOut: "Copy out",
  allowed: "Allowed",
  blocked: "Blocked",
  // Not on the boards: the upload-blocked twin of the board's download-blocked state, a failed
  // transfer, and an empty allowed folder.
  uploadBlocked: "Blocked: uploading files",
  uploadFailed: "Upload failed",
  downloadFailed: "Download failed",
  emptyFolder: "Nothing in the transfer folder yet.",
  // Downloads held for a person's yes (desktop.download: review).
  requestDownload: "Request download",
  asking: "Asking…",
  downloadApproved: "Download approved",
  stateWaiting: "Waiting for approval",
  stateApproved: "Approved",
  stateDenied: "Denied",
  stateExpired: "Expired",
  askedLine: (at: string, sha: string) => `Asked ${at} · sha256 ${sha.slice(0, 4)}…${sha.slice(-4)}`,
  approvedLine: (by: string, at: string) => `Approved by ${by} · ${at}`,
  deniedLine: (by: string, reason: string, at: string) => (reason ? `${by}: “${reason}” · ${at}` : `${by} · ${at}`),
  expiredLine: (at: string) => `Nobody decided it in time · ${at}`,
  reviewFooter:
    "Downloads from this run wait for a person's yes, and each yes releases the file once. Every download is logged by size and hash.",
  // A spent approval whose file did not come to this browser: the decision time is not when anything arrived.
  approvalUsed: "Approval used; request another download",
  notAsked: "Not asked",
  requestsUnreadable: "Couldn't check this run's download requests.",
  retry: "Try again",
  loadFailed: "Couldn't load this desktop's files. Check the connection and try again.",
} as const;
