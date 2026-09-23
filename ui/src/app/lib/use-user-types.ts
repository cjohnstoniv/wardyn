/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { userTypes as userTypesApi } from "./api/user-types";
import type { UserType } from "./types";

// useUserTypes — the full user-types list the admin surfaces that name a type
// need: the Permissions / Governance / Drives subject pickers (UT-7a) and the
// People step's type picker. GET /user-types is securityOps, so nothing a
// user-tier caller renders may call this (the run page reads its "Ran as"
// name off GET /runs/{id} instead).
//
// Degrades to an EMPTY list on a failed fetch, never an error state — every
// caller here is additive to a screen that already has its own load/error
// handling for its OWN data; a user-types outage should shrink what a picker
// offers (or blank a name lookup) rather than block the screen it sits on.
export function useUserTypes(): { userTypes: UserType[]; loading: boolean; reload: () => void } {
  const [userTypes, setUserTypes] = React.useState<UserType[]>([]);
  const [loading, setLoading] = React.useState(true);

  const reload = React.useCallback(() => {
    setLoading(true);
    userTypesApi
      .listUserTypes()
      .then(setUserTypes)
      .catch(() => setUserTypes([]))
      .finally(() => setLoading(false));
  }, []);

  React.useEffect(reload, [reload]);

  return { userTypes, loading, reload };
}
