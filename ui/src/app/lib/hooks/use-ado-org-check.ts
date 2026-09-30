/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The admin's organisation-settings check on one Azure DevOps row: run it, keep
// the answer for this visit. The server keeps no last answer to read back, so
// nothing is drawn until the admin has pressed the button, and a failed check
// says what the server said (a row that is off or unsaved, a missing client
// secret, a browser session it needs) rather than a guess.
import * as React from "react";
import { toast } from "sonner";
import { adoPat } from "../api/ado-pat";
import { getErrorMessage } from "../format";
import type { ADOOrgCheck } from "../types/ado-pat";

export function useAdoOrgCheck(rowId: string): {
  result: ADOOrgCheck | null;
  checking: boolean;
  check: () => void;
} {
  const [result, setResult] = React.useState<ADOOrgCheck | null>(null);
  const [checking, setChecking] = React.useState(false);
  const mounted = React.useRef(true);
  React.useEffect(() => () => void (mounted.current = false), []);

  const check = React.useCallback(() => {
    setChecking(true);
    adoPat
      .orgCheck(rowId)
      .then((r) => mounted.current && setResult(r))
      .catch((e) => toast.error(getErrorMessage(e)))
      .finally(() => mounted.current && setChecking(false));
  }, [rowId]);

  return { result, checking, check };
}
