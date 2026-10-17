/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { health } from "../../lib/api/health";
import { releaseUnloadGuard } from "../../lib/use-unsaved-guard";
import { appURL, routerPath } from "../../lib/base-path";
import { useRequestIdentity } from "./operator-context";

/**
 * The Discard action of a 412 banner. A 412 can mean another tab switched the session to someone
 * else, so it re-reads /me first. A different person than this page loaded as reloads the page,
 * as useViewResync does, so the shell, its gates and the expected principal move together; the
 * same person, or no answer, reloads only the resource.
 */
export function useDiscardReload(load: () => void): () => void {
  const { principal, operatorResolved } = useRequestIdentity();
  return React.useCallback(() => {
    void health
      .whoami()
      .then((me) => {
        if (me?.principal && operatorResolved && me.principal !== principal) {
          // Discard was the answer: the browser's own leave prompt would ask a second time.
          releaseUnloadGuard(true);
          const { search, hash } = window.location;
          // Assigning the current URL with a fragment only moves within the page.
          if (hash) window.location.reload();
          else window.location.assign(appURL(`${routerPath()}${search}`));
          return;
        }
        load();
      })
      .catch(load);
  }, [load, principal, operatorResolved]);
}
