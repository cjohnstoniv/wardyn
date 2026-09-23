/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { userTypes as userTypesApi } from "./api/user-types";
import type { UserType } from "./types";

// useUserTypes — the full user-types list every surface that names a type
// needs: the Permissions / Governance / Drives subject pickers (UT-7a), the
// People step's type picker, and the run header's id -> display-name lookup.
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

// byId — the id -> row lookup every display site needs (a name off a bare id
// on a grant, a run, or an assignment). undefined for an id no longer in the
// list, which a caller renders as "the id itself" or "nothing", never a guess.
export function userTypeById(list: UserType[], id: string): UserType | undefined {
  return list.find((t) => t.id === id);
}
