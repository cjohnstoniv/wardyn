/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { userTypes as userTypesApi } from "./api/user-types";
import type { UserType } from "./types";
import { useSecurityOperator } from "../components/wardyn/operator-context";

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
export function useUserTypes(enabled = true): { userTypes: UserType[]; loading: boolean; reload: () => void } {
  const [userTypes, setUserTypes] = React.useState<UserType[]>([]);
  const [loading, setLoading] = React.useState(true);

  const reload = React.useCallback(() => {
    if (!enabled) return;
    setLoading(true);
    userTypesApi
      .listUserTypes()
      .then(setUserTypes)
      .catch(() => setUserTypes([]))
      .finally(() => setLoading(false));
  }, [enabled]);

  React.useEffect(reload, [reload]);

  return { userTypes, loading, reload };
}

// A user type's display name by id, for a row that stores the id (a grant, an
// assignment, a drive allocation). Falls back to the id itself while the list
// loads, after a failed read, or for a type that no longer exists.
// Asks only for a security admin or a super admin: GET /user-types is
// securityOps, and any other caller would only earn an authz.denied row.
export function useUserTypeName(): (id: string) => string {
  const { userTypes } = useUserTypes(useSecurityOperator());
  return React.useCallback((id: string) => userTypes.find((t) => t.id === id)?.name ?? id, [userTypes]);
}
