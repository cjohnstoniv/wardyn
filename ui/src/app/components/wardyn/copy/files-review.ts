/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The words of three approval cards: one reviewed file download (wardyn/file-download-card.tsx), one held kubectl
// write (wardyn/kube-call-card.tsx) and one held host request (wardyn/host-call-card.tsx). The strings they share with the MCP call card (the Once chip, Approve once,
// Deny, the call fingerprint, the deny note) stay in copy/approvals.ts and copy/deciders-inbox.ts.

export const FILE_DOWNLOAD_CARD = {
  TITLE: (name: string) => `Download ${name}`,
  FIELD_FILE: "File",
  FIELD_SIZE: "Size",
  FIELD_SHA: "SHA-256",
  FIELD_ASKED_BY: "Asked by",
  // Who asked, once erasure rewrote the asker (internal/types/file_download.go FileDownloadAskerErased).
  ERASED_ASKER: "a person since erased",
  SIZE: (size: string, n: string) => `${size} (${n} bytes)`,
  WHAT: (asker: string, name: string) => `${asker} downloads ${name} from this run, once, with exactly the bytes named here.`,
  BLAST:
    "The file leaves the run for that person's browser. You see its name, size and hash, never its content. If the file's bytes change before the download, nothing is sent.",
  UNDER_BANNER: "Approving lets this one file out once. Asking again needs another yes.",
  ASKER_NOTE:
    "You asked for this download, so someone else must approve it. Once it is approved, download it from the run's Files panel.",
  // An erased asker could be anyone, so the server lets no person decide the download (desktop_review.go askedForDownload).
  ERASED_NOTE: "The person who asked for this download was erased and could be anyone, so no person can approve or deny it.",
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

// HC-1 (rc.18 mock verdicts): the held host-rule request. Approving covers the method and path on the host for the
// rest of the run, so the card says that, not "once". A row whose proxy accepts once (requested_scope.accepts_once)
// reads the _EITHER lines, and under a deciders rule whose max_scope is below run, the _ONE lines.
export const HOST_CALL_CARD = {
  TITLE: "Approve requests to one host",
  FIELD_HOST: "Host",
  FIELD_METHOD: "Method",
  FIELD_PATH: "Path",
  WHAT: (method: string, path: string, host: string) => `allowing ${method} requests to ${path} on ${host} for the rest of this run.`,
  WHAT_EITHER: (method: string, path: string, host: string) =>
    `allowing ${method} requests to ${path} on ${host}, for one request or for the rest of this run.`,
  WHAT_ONE: (method: string, path: string, host: string) => `allowing one ${method} request to ${path} on ${host}.`,
  BLAST: "This covers any query or body sent with this host, method and path. Other policy rules still apply.",
  APPROVE: "Approve for this run",
  // Offered only on a row whose proxy accepts once (requested_scope.accepts_once).
  APPROVE_ONCE: "Just this request",
  ONCE_NOTE: "Just this request lets one request through, the one waiting or else the next one sent. The same host, method and path then asks again.",
  DENY_NOTE: "Refuses this method and path on this host for the rest of the run.",
  // A row whose proxy accepts once, under a unit deciders rule whose max_scope is below run.
  CAP_NOTE: "This unit's deciders rule allows one request at most, so Approve for this run is off and Deny refuses only the waiting request.",
  HELD_NOTE: "If the request is still waiting, approving lets it proceed when all other rules allow it.",
  HELD_EXPIRED: "No longer waiting. This approval covers retries with the same host, method and path for the rest of the run. Other policy rules still apply.",
  HELD_EXPIRED_EITHER:
    "No longer waiting. Approve for this run covers retries with the same host, method and path for the rest of the run; Just this request covers the next one only. Other policy rules still apply.",
  HELD_EXPIRED_ONE: "No longer waiting. Approving covers only the next request with the same host, method and path. Other policy rules still apply.",
  LIST_TITLE: (method: string, path: string, host: string) => `${method} ${path} on ${host}`,
} as const;
