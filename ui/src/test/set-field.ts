/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Sets a form field's value in one step, the way a user's typing leaves it:
// focused, holding the final text. `user.type` re-renders a controlled form
// after every character, so a long value pays for each keystroke. This
// dispatches one React change event instead (the same fireEvent.change the
// suite already uses): testing-library assigns the value through the
// element's native setter, which bypasses React's value tracker, so the
// controlled input's onChange sees the whole final string. The value replaces
// the field's content rather than appending to it.
import { act, fireEvent } from "@testing-library/react";

export function setField(field: HTMLElement, value: string): void {
  act(() => field.focus());
  fireEvent.change(field, { target: { value } });
}
