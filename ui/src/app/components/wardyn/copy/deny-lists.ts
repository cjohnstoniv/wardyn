/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/** The governance profile's deny lists (internal/types/governance.go DenyComponents, DenyModelProviders). */
export const DENY_LISTS = {
  COMPONENTS_LABEL: "Denied components",
  COMPONENTS_HINT:
    "Component ids people on this profile may not attach to a run. A run that attaches one is refused, and a queued run that does ends when it starts. Applies to stored components only; empty denies none.",
  PROVIDERS_LABEL: "Denied model providers",
  PROVIDERS_HINT:
    "Model provider uids people on this profile may not use. A run that would use one is refused, and a queued run that would ends when it starts. Empty denies none.",
  COMPONENTS_CHIP: (n: number) => `${n} denied ${n === 1 ? "component" : "components"}`,
  PROVIDERS_CHIP: (n: number) => `${n} denied model ${n === 1 ? "provider" : "providers"}`,
} as const;
