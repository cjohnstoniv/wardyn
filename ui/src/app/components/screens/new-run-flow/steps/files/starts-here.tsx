/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "Starts here": one radio group across every file the step lists, drives
// included. Choosing it only says where the run opens; it never includes
// anything, so an item that is not included cannot be chosen.

import * as React from "react";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";

export const STARTS_HERE_GROUP = "nr-starts-here";

export function StartsHere({ nameId, id, checked, included, onSelect }: {
  /** The id of the element that names the item; the radio is named "<item> Starts here". */
  nameId: string;
  id?: string;
  checked: boolean;
  included: boolean;
  onSelect: () => void;
}) {
  const labelId = React.useId();
  return (
    <label className="inline-flex items-center gap-2 text-sm">
      <input
        id={id}
        type="radio"
        name={STARTS_HERE_GROUP}
        className="accent-primary"
        checked={checked}
        disabled={!included}
        aria-labelledby={`${nameId} ${labelId}`}
        onChange={onSelect}
      />
      <span id={labelId}>{NR_FILES.STARTS_HERE}</span>
    </label>
  );
}
