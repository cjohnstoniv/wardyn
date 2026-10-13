/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// OWNER-WORDING: the five CLI cards' names and one-line descriptions, and the
// cards' decorative icon marks, taken from the approved Tools and access prototype. They are in no canon table yet, so the
// lead moves them into copy/new-run/tools.ts and docs/design/new-run-canon.md
// when the owner confirms the wording; nothing else in this lane carries a
// string of its own.

import type { CatalogueCLIKind } from "../../../../../lib/types/components";

export const CLI_CARD: Record<CatalogueCLIKind, { name: string; body: string; icon: string }> = {
  azure: { name: "Azure CLI", body: "Run az commands against your Azure subscriptions.", icon: "az" },
  github: { name: "GitHub CLI", body: "Issues, pull requests and Actions with gh.", icon: "gh" },
  aws: { name: "AWS CLI", body: "aws commands with a role your organisation assigns.", icon: "aws" },
  gcloud: { name: "Google Cloud CLI", body: "gcloud with your Google Cloud identity.", icon: "gc" },
  kubectl: { name: "kubectl", body: "Reach a Kubernetes cluster your organisation registers.", icon: "k8" },
};

/** The icon mark on the other cards (the prototype's catalogue and door cards). */
export const CARD_ICON = { mcp: "MCP", api: "API", secret: "key", host: "net" } as const;
