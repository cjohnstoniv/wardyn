/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Shared client-side vocabulary for the AI composer.
//
//  - stageLabel maps an INTERNAL pipeline-stage key (the server emits keys only)
//    to a small, user-facing phrase. One source of truth for the progress copy, so
//    renaming/reordering the backend pipeline never changes what the user reads.
//  - hostLabel gives a friendly egress-host label that ALWAYS also shows the raw
//    host, so a friendly name is never the sole label and a host can never be
//    silently dropped. Deny-by-default egress and the honest allow_all_egress
//    phrasing are unchanged — this is purely cosmetic.

const STAGE_COPY: Record<string, string> = {
  validate: "Understanding your request",
  clarify: "Understanding your request",
  detect: "Inspecting your workspace",
  propose: "Planning a sandbox",
  ground: "Applying your security policy",
  clamp: "Applying your security policy",
  check: "Checking for risks",
  grade: "Checking for risks",
  setup: "Checking your setup",
  assemble: "Preparing review",
};

// stageLabel maps an internal stage key to human copy; an unknown key falls back to
// the raw key (never blank) so a newly-added pipeline stage still shows something.
export function stageLabel(stage?: string): string {
  if (!stage) return "Composing your sandbox…";
  return STAGE_COPY[stage] ?? stage;
}

// A small set of well-known egress hosts, mirroring the grader's baseline
// (internal/composer/risk.go). Intentionally tiny: the raw host is ALWAYS shown, so
// an unknown host is never dropped and this never becomes a maintenance treadmill.
const WELL_KNOWN_HOST: Record<string, string> = {
  "github.com": "GitHub",
  "api.github.com": "GitHub",
  "codeload.github.com": "GitHub",
  "objects.githubusercontent.com": "GitHub",
  "raw.githubusercontent.com": "GitHub",
  "api.anthropic.com": "Anthropic",
  "api.openai.com": "OpenAI",
  "registry.npmjs.org": "npm",
  "pypi.org": "PyPI",
  "files.pythonhosted.org": "PyPI",
  "proxy.golang.org": "Go modules",
  "sum.golang.org": "Go checksum db",
  "index.docker.io": "Docker Hub",
  "ghcr.io": "GitHub Container Registry",
};

// hostLabel returns a friendly label that ALWAYS contains the raw host: known hosts
// render "GitHub (api.github.com)", unknown hosts render the bare host. It is
// structurally impossible to drop or hide a host.
export function hostLabel(host: string): string {
  const name = WELL_KNOWN_HOST[host];
  return name ? `${name} (${host})` : host;
}
