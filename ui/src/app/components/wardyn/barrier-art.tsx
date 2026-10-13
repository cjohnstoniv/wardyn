/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The barrier illustration. Imported as modules so Vite emits hashed files: only
// the 128 and 256 px art is shipped, because a smaller file falls under Vite's
// inline limit and would become a data: URI, which the CSP blocks. Below 56 px
// use BarrierMark's SVG instead.
import fence128 from "./barrier-art/barrier-fence-128.png";
import fence256 from "./barrier-art/barrier-fence-256.png";
import wall128 from "./barrier-art/barrier-wall-128.png";
import wall256 from "./barrier-art/barrier-wall-256.png";
import vault128 from "./barrier-art/barrier-vault-128.png";
import vault256 from "./barrier-art/barrier-vault-256.png";
import type { ConfinementClass } from "../../lib/types";

const ART: Record<ConfinementClass, [string, string]> = {
  CC1: [fence128, fence256],
  CC2: [wall128, wall256],
  CC3: [vault128, vault256],
};

/** Decorative: the column header next to it names the barrier. */
export function BarrierArt({ cc, size }: { cc: ConfinementClass; size: 56 | 88 }) {
  const [x1, x2] = ART[cc];
  return (
    <img
      src={x1}
      srcSet={`${x1} 128w, ${x2} 256w`}
      sizes={`${size}px`}
      width={size}
      height={size}
      alt=""
      decoding="async"
      className="shrink-0"
    />
  );
}
