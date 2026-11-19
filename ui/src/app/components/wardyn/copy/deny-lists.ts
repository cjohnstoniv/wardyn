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
  /** The composed rows' hints: the box only adds to what the base denies. */
  COMPONENTS_HINT_COMPOSED:
    "Component ids people on this profile may not attach to a run. A run that attaches one is refused, and a queued run that does ends when it starts. Applies to stored components only. Adds to what the base denies. Entries the base denies stay denied.",
  PROVIDERS_HINT_COMPOSED:
    "Model provider uids people on this profile may not use. A run that would use one is refused, and a queued run that would ends when it starts. Adds to what the base denies. Entries the base denies stay denied.",
  PLACEHOLDER: "One id per line",
  STRIP_COMPONENTS: "Stored components",
  STRIP_PROVIDERS: "Your model providers",
  ADD: (name: string) => `Add ${name}`,
  /** The heading over the server's sentence when a deny list is refused. */
  REFUSED_LIST: "This list can't be saved",
  COMPONENTS_CHIP: (n: number) => `${n} denied ${n === 1 ? "component" : "components"}`,
  PROVIDERS_CHIP: (n: number) => `${n} denied model ${n === 1 ? "provider" : "providers"}`,
} as const;
