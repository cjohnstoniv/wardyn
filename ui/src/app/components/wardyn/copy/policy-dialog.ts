/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The New/Edit policy dialog's lede (policies.tsx): what a policy is for, not
// how the console or the server handles the spec.
export const POLICY_DIALOG = {
  LEDE: "A named set of network, credential and lifetime rules a run can use. Wardyn checks the spec when you save and shows any problem here.",
} as const;

// The Policies page's caption over the caller's own ceiling (policies.tsx): the page shows the
// caller's ceiling, so it speaks to them, not about every member.
export const POLICIES_PAGE = {
  CEILING_CAPTION: "Your ceiling policy — used when a run names no policy, and the limit on any inline policy you write",
} as const;
