/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { setExpectedPrincipal } from "./core";
import { health } from "./health";

/**
 * Re-reads /me and names its person on later drafts. A 412 can mean the session now belongs to
 * someone else (another tab switched it): reloading only the resource would keep naming the old
 * person, so every save would be refused again.
 */
export async function reconfirmPrincipal(): Promise<void> {
  const me = await health.whoami();
  if (me?.principal) setExpectedPrincipal(me.principal);
}
