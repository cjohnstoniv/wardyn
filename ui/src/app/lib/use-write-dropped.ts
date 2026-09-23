/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { ReauthContext } from "./reauth";

/** For the screen whose Save is `owner` (WfetchInit.save): true once the person
 *  is back and that save was refused in the lapse (#483); call clear() when the
 *  screen saves again. Its own module, apart from lib/reauth.ts, so it rides in
 *  the saving screens' lazy chunks rather than the entry. */
export function useWriteDropped(owner: string): [boolean, () => void] {
  const r = React.useContext(ReauthContext);
  const claim = r.claimWriteDropped;
  React.useEffect(() => claim(owner), [claim, owner]);
  return [r.writeDropped === owner && r.phase === "none", r.clearWriteDropped];
}
