/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Reads the chosen pool's `macos_vm` off GET /runner-pools into the macOS offer the OS row, the harness list and
// Review's Limits read (mock M-N1). It sits above every step because the Harnesses step and Review read the offer
// too. The list is asked for only while the organisation's macOS switch is on, so a console without it asks nothing.

import * as React from "react";
import { macOSVMsEnabled } from "../../../../../lib/types/machine-kind";
import { useShellSetupStatus } from "../../../../wardyn/model-access-context";
import { useNewRunDraft } from "../../new-run-route";
import { MacOSOfferContext, macOSOfferOf } from "./macos-offer";
import { runTypeOf, selectedPoolId, usablePools } from "./pool-model";
import { loadPools, usePools, type LoadPools } from "./pool-source";

export function MacOSOfferProvider({ load = loadPools, children }: { load?: LoadPools; children: React.ReactNode }) {
  const { intent } = useNewRunDraft();
  const { status } = useShellSetupStatus();
  const pools = usePools(macOSVMsEnabled(status) ? load : undefined, runTypeOf(intent));
  const offer = React.useMemo(() => {
    if (pools.status !== "ready") return undefined;
    const id = selectedPoolId(intent, pools.list);
    return macOSOfferOf(usablePools(pools.list).find((p) => p.id === id)?.macos_vm);
  }, [pools, intent]);
  return <MacOSOfferContext.Provider value={offer}>{children}</MacOSOfferContext.Provider>;
}
