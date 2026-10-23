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
  pasteIn: "Paste in",
  copyOut: "Copy out",
  allowed: "Allowed",
  blocked: "Blocked",
  // Not on the boards: the upload-blocked twin of the board's download-blocked state, a failed
  // transfer, and an empty allowed folder.
  uploadBlocked: "Blocked: uploading files",
  notSent: "Not sent",
  downloadFailed: "Download failed",
  emptyFolder: "Nothing in the transfer folder yet.",
} as const;
