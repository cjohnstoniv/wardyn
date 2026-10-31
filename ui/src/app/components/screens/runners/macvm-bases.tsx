/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A Mac's bases on its runner drawer (board J6): Standard, the one prepare builds, then each other base derived from
// it with the operator's extra items, and while Standard changes the ones being rebuilt from it. Only what the
// runner's caps say; a Mac with no other base shows nothing here.
import { harnessDisplayNames } from "../../../lib/model-access";
import type { MacVMCaps, MacVMDerivedBase } from "../../../lib/types/runner-macvm";
import { DESKTOP } from "../../wardyn/copy/desktop";
import { MAC_BASES } from "../../wardyn/copy/mac-bases";
import { MACVM_R } from "../../wardyn/copy/runner-macvm";
import { useShellSetupStatus } from "../../wardyn/model-access-context";
import { Chip } from "../../wardyn/primitives";
import { Hint } from "./resource-table";

function StateChip({ base }: { base: MacVMDerivedBase }) {
  if (base.state === "ready") return <Chip tone="success" dot>{DESKTOP.ready}</Chip>;
  // The wire's percent rides on the chip, as the preparing bar's does for Standard.
  if (base.state === "rebuilding") return <Chip tone="info" dot pulse title={`${base.progress ?? 0}%`}>{MAC_BASES.B12}</Chip>;
  if (base.state === "failed") return <Chip tone="danger">{MACVM_R.R15}</Chip>;
  return null;
}

export function MacVMBases({ caps }: { caps: MacVMCaps }) {
  const { status } = useShellSetupStatus();
  const others = caps.bases ?? [];
  if (!others.length) return null;
  const names = (caps.harnesses ?? []).map((h) => harnessDisplayNames(status, [h])).join(", ");
  const standard = [caps.base.guest_os, names].filter(Boolean).join(" · ");
  return (
    <div data-macvm-bases className="mt-2">
      <h4 className="text-meta font-medium text-foreground">{MAC_BASES.B10}</h4>
      <ul className="mt-1 divide-y divide-border rounded-md border border-border">
        <li className="flex items-center justify-between gap-3 px-3 py-1.5">
          <span>
            <span className="font-medium">{MAC_BASES.B04}</span>
            {standard && <span className="text-meta text-muted-foreground"> · {standard}</span>}
          </span>
          <Chip tone="success" dot>{DESKTOP.ready}</Chip>
        </li>
        {others.map((b) => (
          <li key={b.name} className="flex items-center justify-between gap-3 px-3 py-1.5">
            <span>
              <span className="font-medium">{b.name}</span>
              <span className="text-meta text-muted-foreground"> · {MAC_BASES.B11(b.items)}</span>
            </span>
            <StateChip base={b} />
          </li>
        ))}
      </ul>
      <Hint>{MAC_BASES.B13(caps.max)}</Hint>
    </div>
  );
}
