/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { UIApp } from "../../../../../lib/types/runs";

export interface CallersEditorProps {
  /** The port's draft, with its `callers` (user:, group: or service: entries) as they stand. */
  app: UIApp;
  /** The port's row; its fields are `nr-app-<field>-<index>`. */
  index: number;
  /** The port's name for a screen reader, or its place when it has none yet. */
  who: string;
  /** Replaces the port's callers. An empty list leaves only the run's owner. */
  onChange: (callers: string[]) => void;
}

/**
 * The seam for "Who can call it". PortsSection renders this under a port's checked "Callable from outside the
 * run"; the callers picker (r17, in place of the rejected free-text field) replaces this body and nothing else.
 * Until then it draws nothing: a port the person marks callable has no callers beyond the run's owner, and any
 * `callers` the draft already holds (from a template or a pasted document) ride to the server unchanged.
 */
export function CallersEditor(_props: CallersEditorProps) {
  return null;
}
