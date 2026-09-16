/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// R-1: App.tsx's own attention/approvals badge poll pauses while parked on
// /runs (X3-F13) because the Runs board already fetches the same two counts
// on its own poll — but the board never PUBLISHED them anywhere, so pausing
// froze both nav badges for as long as an operator sat on the board. This is
// the publish side: RunsScreen calls it every time its own fetch resolves;
// App provides the real setter, wired to the same state the paused poll would
// have updated. Default is a no-op so every mount with no
// <AttentionPublisherProvider> above it (every existing test, any future
// page) is inert rather than crashing.
import * as React from "react";

export type AttentionCounts = {
  pendingApprovals: number;
  attentionCount: number;
};

const AttentionPublisherContext = React.createContext<(counts: AttentionCounts) => void>(() => {});

export const AttentionPublisherProvider = AttentionPublisherContext.Provider;

export function usePublishAttention(): (counts: AttentionCounts) => void {
  return React.useContext(AttentionPublisherContext);
}
