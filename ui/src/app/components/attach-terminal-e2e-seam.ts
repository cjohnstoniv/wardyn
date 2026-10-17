/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/**
 * Test-only text-read seam. The GPU renderer draws to a canvas, so specs can no
 * longer read terminal text from the DOM; under the e2e build mode only, each
 * terminal registers itself here and the specs read its buffer. The production
 * build folds the mode check to a constant and drops everything below, which CI
 * proves by grepping the built output for the registry name.
 */
import type { Terminal } from "@xterm/xterm";

/** Returns the unregister function. A no-op outside the e2e build mode. */
export function exposeTerminalForE2E(term: Terminal): () => void {
  if (import.meta.env.MODE !== "e2e") return () => {};
  const w = window as unknown as { __wardynTerm?: { terms: Set<Terminal> } };
  const reg = (w.__wardynTerm ??= { terms: new Set() });
  reg.terms.add(term);
  return () => reg.terms.delete(term);
}
