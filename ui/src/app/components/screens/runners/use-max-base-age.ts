/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { health } from "../../../lib/api/health";

/**
 * The organisation's maximum macOS base age in days (site config `macos_vm.max_base_age_days`), read once and
 * only when `enabled` (some runner sends macOS VM caps). Absent while unread or unreadable: no maximum.
 */
export function useMaxBaseAgeDays(enabled: boolean): number | undefined {
  const [days, setDays] = React.useState<number>();
  React.useEffect(() => {
    if (!enabled) return;
    let alive = true;
    health
      .getSiteConfig()
      .then((sc) => alive && setDays(sc.macos_vm?.max_base_age_days))
      .catch(() => {});
    return () => {
      alive = false;
    };
  }, [enabled]);
  return days;
}
