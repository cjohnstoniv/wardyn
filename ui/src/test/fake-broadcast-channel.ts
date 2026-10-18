/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A BroadcastChannel stand-in for the cross-tab auth sync tests (lib/api/auth-sync.ts). What this tab
// posts is recorded in `posted`; what "another tab" posts is handed in with deliver().
export class FakeBroadcastChannel {
  static all: FakeBroadcastChannel[] = [];
  static posted: { name: string; data: unknown }[] = [];
  private listeners = new Set<(e: MessageEvent) => void>();
  constructor(readonly name: string) {
    FakeBroadcastChannel.all.push(this);
  }
  postMessage(data: unknown): void {
    FakeBroadcastChannel.posted.push({ name: this.name, data: structuredClone(data) });
  }
  addEventListener(type: string, fn: (e: MessageEvent) => void): void {
    if (type === "message") this.listeners.add(fn);
  }
  removeEventListener(type: string, fn: (e: MessageEvent) => void): void {
    if (type === "message") this.listeners.delete(fn);
  }
  close(): void {
    FakeBroadcastChannel.all = FakeBroadcastChannel.all.filter((c) => c !== this);
  }
  /** Another tab's message reaches every open channel of that name in this tab. */
  static deliver(name: string, data: unknown): void {
    for (const c of FakeBroadcastChannel.all) if (c.name === name) for (const l of c.listeners) l({ data } as MessageEvent);
  }
  static reset(): void {
    FakeBroadcastChannel.all = [];
    FakeBroadcastChannel.posted = [];
  }
}
