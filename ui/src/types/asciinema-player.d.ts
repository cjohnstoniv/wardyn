/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Minimal ambient declaration for asciinema-player, which ships no types.
// The UI uses create() to mount a player into a DOM element; keep this loose.
declare module "asciinema-player" {
  export interface CreateOptions {
    cols?: number;
    rows?: number;
    autoPlay?: boolean;
    preload?: boolean;
    loop?: boolean | number;
    startAt?: number | string;
    speed?: number;
    idleTimeLimit?: number;
    theme?: string;
    poster?: string;
    fit?: string | false;
    controls?: boolean | "auto";
    [key: string]: unknown;
  }
  export interface Player {
    dispose(): void;
    play(): Promise<void>;
    pause(): void;
    seek(pos: number | string): Promise<void>;
    [key: string]: unknown;
  }
  export function create(
    src: unknown,
    element: HTMLElement,
    opts?: CreateOptions,
  ): Player;
}
