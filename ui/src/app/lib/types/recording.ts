/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Asciicast recording shapes — fed to the terminal player.
export interface AsciicastHeader {
  version: number;
  width: number;
  height: number;
  title?: string;
}
export type AsciicastEvent = [number, "o", string];
export interface Recording {
  run_id: string;
  header: AsciicastHeader;
  events: AsciicastEvent[];
  /** Raw asciicast v2 text, fed verbatim to asciinema-player for true terminal
   *  emulation (escape sequences interpreted, not printed). */
  cast: string;
}
