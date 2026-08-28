/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";

// useDeferredBusy — rulebook §7 (in-flight feedback): bind `disabled` the
// instant an action fires, but don't flash a spinner for anything that
// resolves fast. `disabled` mirrors `busy` with no delay; `showSpinner` only
// flips on after `delayMs` of CONTINUOUS busy, and off immediately the
// moment `busy` ends.
export function useDeferredBusy(
  busy: boolean,
  delayMs = 200,
): { disabled: boolean; showSpinner: boolean } {
  const [showSpinner, setShowSpinner] = React.useState(false);

  React.useEffect(() => {
    if (!busy) {
      setShowSpinner(false);
      return;
    }
    const id = setTimeout(() => setShowSpinner(true), delayMs);
    return () => clearTimeout(id);
  }, [busy, delayMs]);

  return { disabled: busy, showSpinner };
}
