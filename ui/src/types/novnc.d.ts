/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Minimal ambient declaration for @novnc/novnc (MPL-2.0, shipped unmodified), which ships no types.
// The desktop replay hands RFB a fake socket and only watches; keep this to what it uses.
declare module "@novnc/novnc" {
  /** A WebSocket-shaped channel RFB attaches to instead of dialling a URL. */
  export interface RFBChannel {
    binaryType: string;
    protocol: string;
    readyState: string;
    onopen: ((e: unknown) => void) | null;
    onmessage: ((e: { data: ArrayBuffer }) => void) | null;
    onerror: ((e: unknown) => void) | null;
    onclose: ((e: unknown) => void) | null;
    send(data: unknown): void;
    close(): void;
  }
  export default class RFB extends EventTarget {
    constructor(target: HTMLElement, urlOrChannel: string | RFBChannel, options?: Record<string, unknown>);
    viewOnly: boolean;
    scaleViewport: boolean;
    focusOnClick: boolean;
    showDotCursor: boolean;
    disconnect(): void;
  }
}
