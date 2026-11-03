/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What GET /me/machines says to New Run: the person's kept machines and whether they may keep another. The Run step
// reads it to decide whether to draw the machine section, and Review reads it to name a machine it resumes. It is read
// once while the site's macOS switch is on; any failure (a caller who is no person) reads as no machines, and the create
// checks again whatever this says.

import * as React from "react";
import { myMachines } from "../../../../../lib/api/me-machines";
import { macOSVMsEnabled } from "../../../../../lib/types/machine-kind";
import type { MyMachines } from "../../../../../lib/types";
import { useShellSetupStatus } from "../../../../wardyn/model-access-context";

export type KeptMachinesRead = { status: "loading" } | { status: "absent" } | { status: "ready"; data: MyMachines };

export const KeptMachinesContext = React.createContext<KeptMachinesRead>({ status: "absent" });
export const useKeptMachines = (): KeptMachinesRead => React.useContext(KeptMachinesContext);

export function KeptMachinesProvider({ load = myMachines, children }: { load?: () => Promise<MyMachines>; children: React.ReactNode }) {
  const { status } = useShellSetupStatus();
  const on = macOSVMsEnabled(status);
  const [read, setRead] = React.useState<KeptMachinesRead>({ status: "loading" });
  React.useEffect(() => {
    if (!on) {
      setRead({ status: "absent" });
      return;
    }
    let live = true;
    load()
      .then((data) => live && setRead({ status: "ready", data }))
      .catch(() => live && setRead({ status: "absent" }));
    return () => {
      live = false;
    };
  }, [on, load]);
  return <KeptMachinesContext.Provider value={read}>{children}</KeptMachinesContext.Provider>;
}
