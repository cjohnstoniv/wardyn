/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { directory, MIN_QUERY_LEN } from "./api/directory";
import { useSecurityOperator } from "../components/wardyn/operator-context";

// useOwnerName — a run owner's display name for a sentence that would otherwise print their sign-in
// subject (#3785). The directory is where the console reads display names (the People step's picker),
// and GET /access/directory/search is securityOps, so only a security admin or a super admin asks.
// Anyone else, a deployment with no directory, a failed read and a subject the directory does not
// know all keep the subject itself: the name is an upgrade, never a gate.
export function useOwnerName(owner: string | undefined, enabled = true): string {
  const allowed = useSecurityOperator();
  const [found, setFound] = React.useState<{ owner: string; name: string } | null>(null);
  React.useEffect(() => {
    if (!enabled || !allowed || !owner || owner.length < MIN_QUERY_LEN) return;
    let live = true;
    directory.search(owner, "user").then(
      (entries) => {
        const name = entries?.find((e) => e.claim_value === owner)?.display_name;
        if (live && name) setFound({ owner, name });
      },
      () => {},
    );
    return () => { live = false; };
  }, [owner, enabled, allowed]);
  return found && found.owner === owner ? found.name : (owner ?? "");
}
