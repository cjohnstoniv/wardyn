/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The confirm before a host leaves a workspace's Allowed or Denied list, and the toast after it.
export const WORKSPACE_HOSTS = {
  CANCEL: "Cancel",
  CONFIRM: "Remove host",
  ALLOWED_TITLE: (host: string, workspace: string) => `Remove ${host} from ${workspace}?`,
  ALLOWED_BODY: "Runs on this workspace can no longer reach this host unless another rule allows it.",
  DENIED_TITLE: (host: string, workspace: string) => `Lift the block on ${host} for ${workspace}?`,
  DENIED_BODY:
    "Every run on this workspace stops being blocked from this host. That does not approve it: the host is then judged by the other rules.",
  REMOVED_ALLOWED: (host: string) => `Removed ${host} from the allowed hosts`,
  REMOVED_DENIED: (host: string) => `Lifted the block on ${host}`,
} as const;
