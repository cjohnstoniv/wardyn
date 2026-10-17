/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// OWNER-WORDING: the five CLI cards' names and one-line descriptions, and the
// cards' decorative icon marks, taken from the approved Tools and access prototype. They are in no canon table yet, so the
// lead moves them into copy/new-run/tools.ts and docs/design/new-run-canon.md
// when the owner confirms the wording; nothing else in this lane carries a
// string of its own. The GitHub CLI line is already canon (the owner's wording).

import type { CatalogueCLIKind } from "../../../../../lib/types/components";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";

export const CLI_CARD: Record<CatalogueCLIKind, { name: string; body: string; icon: string }> = {
  azure: { name: "Azure CLI", body: "Run az commands against your Azure subscriptions.", icon: "az" },
  github: { name: "GitHub CLI", body: NR_TOOLS.CLI_GITHUB_BODY, icon: "gh" },
  aws: { name: "AWS CLI", body: "aws commands with a role your organisation assigns.", icon: "aws" },
  gcloud: { name: "Google Cloud CLI", body: "gcloud with your Google Cloud identity.", icon: "gc" },
  kubectl: { name: "kubectl", body: "Reach a Kubernetes cluster your organisation registers.", icon: "k8" },
};

/** The icon mark on the other cards (the prototype's catalogue and door cards). */
export const CARD_ICON = { mcp: "MCP", api: "API", secret: "key", host: "net" } as const;

const HARNESS_MARK: Record<string, string> = { "claude-code": "cc", "codex-cli": "cx" };
/** The one mark every other harness shares (a custom or admin-declared one), so no harness id is cut to its first two letters. */
const HARNESS_MARK_OTHER = "ag";

/** The icon mark on a row of Already included (prototype rows "cc", "cx", "git", "dr", "$"). A harness the prototype has no mark for gets the shared "ag" mark. */
export function includedMark(row: { from: string; name: string }): string {
  if (row.from === "harness") return HARNESS_MARK[row.name] ?? HARNESS_MARK_OTHER;
  return row.from === "repository" ? "git" : row.from === "drive" ? "dr" : "$";
}

/** The icon mark on a row of Added to this run: the card it was added from. */
export function addedMark(row: { kind: string; name: string }): string {
  if (row.kind === "cli") return Object.values(CLI_CARD).find((c) => c.name === row.name)?.icon ?? CARD_ICON.api;
  return row.kind === "mcp" ? CARD_ICON.mcp : row.kind === "secret" ? CARD_ICON.secret : row.kind === "host" ? CARD_ICON.host : CARD_ICON.api;
}
