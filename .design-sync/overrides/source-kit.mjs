/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { appendFileSync, realpathSync } from 'node:fs';
import { resolve } from 'node:path';
import { resolvePackage as upstream } from '../../.ds-sync/lib/source-kit.mjs';

export async function resolvePackage(context) {
  const source = await upstream(context);
  if (source.synthEntry) {
    // ESM keeps ambiguous star exports absent even through an outer bridge.
    const bridge = resolve(realpathSync(context.PKG_DIR), '../.design-sync/exports.ts');
    appendFileSync(source.entry, `export { Note, SectionCard } from ${JSON.stringify(bridge)};\n`);
  }
  return source;
}
