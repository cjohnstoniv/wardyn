/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A path as this run's guest sees it, for the screen only (see guestPath in file-items.ts): the draft's machine kind
// says whether it is a macOS VM.
import { useNewRunDraft } from "../../new-run-route";
import { guestPath } from "./file-items";

export function useGuestPath(): (text: string) => string {
  const macOS = useNewRunDraft().intent.machine_kind === "macos_vm";
  return (text) => guestPath(text, macOS);
}
