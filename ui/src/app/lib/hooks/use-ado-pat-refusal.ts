/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The newest launch the organisation's token-creation policy refused on one
// Azure DevOps row in the last seven days (#1449), for the admin's banner. It
// is read when the row opens and says nothing when the read fails: the banner is
// an addition to the organisation check, which still reports on its own.
import * as React from "react";
import { adoPat } from "../api/ado-pat";
import type { ADOPATRefusal } from "../types/ado-pat";

export function useAdoPatRefusal(rowId: string, enabled: boolean): ADOPATRefusal | null {
  const [refusal, setRefusal] = React.useState<ADOPATRefusal | null>(null);
  React.useEffect(() => {
    if (!enabled) return;
    let live = true;
    adoPat
      .refusal(rowId)
      .then((r) => live && setRefusal(r))
      .catch(() => live && setRefusal(null));
    return () => {
      live = false;
    };
  }, [rowId, enabled]);
  return enabled ? refusal : null;
}
