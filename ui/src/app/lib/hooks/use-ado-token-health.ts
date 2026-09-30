/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The admin's view of the organisation-settings check: the last answer, and a
// way to run it again. `enabled` is whether the row asks for it at all (a row
// that creates tokens); a disabled hook makes no request. A failed read leaves
// the answer absent: nothing here is drawn from a fact the server did not send.
import * as React from "react";
import { toast } from "sonner";
import { adoPat } from "../api/ado-pat";
import { getErrorMessage } from "../format";
import type { ADOTokenHealth } from "../types/ado-pat";

export function useAdoTokenHealth(enabled: boolean): {
  health: ADOTokenHealth | null;
  checking: boolean;
  check: () => void;
} {
  const [health, setHealth] = React.useState<ADOTokenHealth | null>(null);
  const [checking, setChecking] = React.useState(false);
  const mounted = React.useRef(true);
  React.useEffect(() => () => void (mounted.current = false), []);

  React.useEffect(() => {
    if (!enabled) return;
    let live = true;
    adoPat
      .health()
      .then((h) => live && setHealth(h))
      .catch(() => {});
    return () => {
      live = false;
    };
  }, [enabled]);

  const check = React.useCallback(() => {
    setChecking(true);
    adoPat
      .orgCheck()
      .then((h) => mounted.current && setHealth(h))
      .catch((e) => toast.error(getErrorMessage(e)))
      .finally(() => mounted.current && setChecking(false));
  }, []);

  return { health, checking, check };
}
