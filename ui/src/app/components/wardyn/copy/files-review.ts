/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The words of two approval cards: one reviewed file download (wardyn/file-download-card.tsx) and one held kubectl
// write (wardyn/kube-call-card.tsx). The strings they share with the MCP call card (the Once chip, Approve once,
// Deny, the call fingerprint, the deny note) stay in copy/approvals.ts and copy/deciders-inbox.ts.

export const FILE_DOWNLOAD_CARD = {
  TITLE: (name: string) => `Download ${name}`,
  FIELD_FILE: "File",
  FIELD_SIZE: "Size",
  FIELD_SHA: "SHA-256",
  FIELD_ASKED_BY: "Asked by",
  SIZE: (size: string, n: string) => `${size} (${n} bytes)`,
  WHAT: (asker: string, name: string) => `${asker} downloads ${name} from this run, once, with exactly the bytes named here.`,
  BLAST:
    "The file leaves the run for that person's browser. You see its name, size and hash, never its content. If the file's bytes change before the download, nothing is sent.",
  UNDER_BANNER: "Approving lets this one file out once. Asking again needs another yes.",
  ASKER_NOTE:
    "You asked for this download, so someone else must approve it. Once it is approved, download it from the run's Files panel.",
  LIST_TITLE: (name: string) => `Download ${name}`,
} as const;

export const KUBE_CALL_CARD = {
  TITLE: "Approve one kubectl change",
  FIELD_CLUSTER: "Cluster",
  FIELD_NAMESPACE: "Namespace",
  FIELD_METHOD: "Method",
  FIELD_PATH: "Path",
  FIELD_QUERY: "Query",
  FIELD_MEDIA: "Content type",
  FIELD_BODY: "Body",
  COPY_BODY: "Copy body",
  NO_BODY: "No body",
  WHAT: (method: string, cluster: string, ns: string) => `the run sends this ${method} to ${cluster} in ${ns}, exactly as shown.`,
  BLAST:
    "the cluster does what the request says, under the run's own service account. A request with any other path, query or body is not covered.",
  HELD_NOTE: "kubectl is waiting for a short time. Approving sends this request now, or the next time the run sends the same request.",
  HELD_EXPIRED: "No longer waiting. Approving lets the next identical request through, once.",
  NO_TEXT: "This server did not send the request's exact body, so Approve is off. Deny stays available.",
  LIST_TITLE: (method: string, ns: string) => `kubectl ${method} in ${ns}`,
} as const;
