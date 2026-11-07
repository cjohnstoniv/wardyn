/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The reason with the setting's name shown as code; the sentence reads the same.
export function withSignerVar(reason: string, name: string) {
  const [before, after] = reason.split(name);
  return after === undefined ? (
    reason
  ) : (
    <>
      {before}
      <code className="font-mono">{name}</code>
      {after}
    </>
  );
}
