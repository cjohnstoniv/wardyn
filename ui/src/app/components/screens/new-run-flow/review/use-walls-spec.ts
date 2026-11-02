/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { walls as wallsApi, type PersonalWallsSpec } from "../../../../lib/api/walls";

/** The person's own walls, to name a host they leave out; a caller with none (or no way to read them) gets undefined. */
export function useWallsSpec(): PersonalWallsSpec | undefined {
  const [walls, setWalls] = React.useState<PersonalWallsSpec>();
  React.useEffect(() => {
    let alive = true;
    wallsApi.get().then((w) => alive && setWalls(w.spec), () => undefined);
    return () => { alive = false; };
  }, []);
  return walls;
}
