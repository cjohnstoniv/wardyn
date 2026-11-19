/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What GET /me/machines says to New Run: the person's kept machines and whether they may keep another. The Run step
// reads it to decide whether to draw the machine section, and Review reads it to name a machine it resumes. It is read
// once the setup status is known and the site's macOS switch is on. A read that failed is its own state, never "no
// machines": a keep or resume the person chose stays in the draft, and Launch is held until the read is retried
// (kept-machine-choice.ts). Only the server's answer that the caller is no person reads as absent.

import * as React from "react";
import { HttpError } from "../../../../../lib/api/core";
import { KEPT_MACHINE_REASON } from "../../../../../lib/kept-machine-refusals";
import { myMachines } from "../../../../../lib/api/me-machines";
import { macOSVMsEnabled } from "../../../../../lib/types/machine-kind";
import type { MyMachines } from "../../../../../lib/types";
import { useShellSetupStatus } from "../../../../wardyn/model-access-context";

export type KeptMachinesRead =
  | { status: "loading" }
  | { status: "absent" }
  | { status: "failed"; retry: () => void }
  | { status: "ready"; data: MyMachines };

export const KeptMachinesContext = React.createContext<KeptMachinesRead>({ status: "absent" });
export const useKeptMachines = (): KeptMachinesRead => React.useContext(KeptMachinesContext);

/**
 * The one answer that says this caller has no list: 403 kept_machine_not_your_mac, which handleListMyMachines gives a
 * caller that is not a person (a device, a delegated or service caller, the admin token, local mode; keptMachineOwner,
 * internal/api/kept_machines.go). Any other failure, 401 included, is unknown.
 */
const proves = (e: unknown): boolean => e instanceof HttpError && e.status === 403 && e.reason === KEPT_MACHINE_REASON.NOT_YOUR_MAC;

export function KeptMachinesProvider({ load = myMachines, children }: { load?: () => Promise<MyMachines>; children: React.ReactNode }) {
  const { status } = useShellSetupStatus();
  const known = status !== null;
  const on = macOSVMsEnabled(status);
  const [attempt, setAttempt] = React.useState(0);
  const [read, setRead] = React.useState<KeptMachinesRead>({ status: "loading" });
  const retry = React.useCallback(() => {
    setRead({ status: "loading" });
    setAttempt((n) => n + 1);
  }, []);
  React.useEffect(() => {
    if (!known) {
      setRead({ status: "loading" });
      return;
    }
    if (!on) {
      setRead({ status: "absent" });
      return;
    }
    let live = true;
    load()
      .then((data) => live && setRead({ status: "ready", data }))
      .catch((e: unknown) => live && setRead(proves(e) ? { status: "absent" } : { status: "failed", retry }));
    return () => {
      live = false;
    };
  }, [known, on, load, attempt, retry]);
  return <KeptMachinesContext.Provider value={read}>{children}</KeptMachinesContext.Provider>;
}
