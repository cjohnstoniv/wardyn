/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Runs row's sign-in read, as a component that draws nothing. Its own file
// so the row can load it lazily: imported eagerly, the read loop sits in the
// entry chunk for every viewer, and almost no row is a sign-in run
// (bundle-split.test.ts).
import * as React from "react";
import { useRunSignIn } from "./use-run-sign-in";

export default function RowSignInProbe({
  runId,
  createdAt,
  onChange,
}: {
  runId: string;
  createdAt: string;
  /** Called with whether a sign-in is waiting; a failed read reports false. */
  onChange: (waiting: boolean) => void;
}) {
  const r = useRunSignIn(runId, createdAt, true);
  const waiting = !!r.waiting && !r.failed;
  React.useEffect(() => onChange(waiting), [waiting, onChange]);
  return null;
}
