/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Notifications that name a person (an email, a principal) belong to the sign-in that posted them. Sonner's store is
// a module singleton that the sign-in gate shares, so signing out must not leave them for the next person, and an
// action that finishes after the sign-out must not post one. An action takes the generation when it starts; sign-out
// ends the generation and dismisses what it showed.
import { toast } from "sonner";

let generation = 0;
const shown = new Set<string | number>();

/** The sign-in generation an account-scoped action starts under. */
export const signInGeneration = (): number => generation;

/** Posts an account-scoped notification, unless the sign-in the action started under has ended. */
export function accountToast(kind: "success" | "error", message: string, startedUnder: number): void {
  if (startedUnder !== generation) return;
  const id = toast[kind](message);
  if (id !== undefined) shown.add(id);
}

/** Sign-out starts: dismiss every account-scoped notification and end the generation, so a late completion posts nothing. */
export function clearAccountToasts(): void {
  generation++;
  for (const id of shown) toast.dismiss(id);
  shown.clear();
}
