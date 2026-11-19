/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// An image reference as the request screens show it: "<name>@sha256:<64 hex>" in pieces.

const AT = "@sha256:";

// "ghcr.io/acme/build-tools@sha256:4f2a…" → { name: "ghcr.io/acme/build-tools", digest: "sha256:4f2a…" }.
// A reference with no digest is all name.
export function splitImageRef(ref: string): { name: string; digest: string } {
  const at = ref.indexOf(AT);
  return at < 0 ? { name: ref, digest: "" } : { name: ref.slice(0, at), digest: ref.slice(at + 1) };
}

// "sha256:" and the first 8 hex, then an ellipsis.
export function shortDigest(digest: string): string {
  return digest ? `${digest.slice(0, "sha256:".length + 8)}…` : "";
}

// The Changes target's image half: the name's last path segment and the short digest.
export function changeTargetRef(ref: string): string {
  const { name, digest } = splitImageRef(ref);
  const last = name.split("/").pop() ?? name;
  return digest ? `${last}@${shortDigest(digest)}` : last;
}
