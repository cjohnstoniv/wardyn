/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { AlertDialog, AlertDialogAction, AlertDialogContent, AlertDialogDescription, AlertDialogTitle } from "./alert-dialog";

afterEach(cleanup);

function open(variant?: "destructive") {
  render(
    <AlertDialog open>
      <AlertDialogContent>
        <AlertDialogTitle>Title</AlertDialogTitle>
        <AlertDialogDescription>Body</AlertDialogDescription>
        <AlertDialogAction variant={variant}>Delete</AlertDialogAction>
      </AlertDialogContent>
    </AlertDialog>,
  );
  return screen.getByRole("button", { name: "Delete" });
}

describe("AlertDialogAction", () => {
  it("AlertDialogAction takes the destructive button variant", () => {
    const btn = open("destructive");
    expect(btn).toHaveClass("bg-destructive", "text-danger-foreground", "hover:bg-destructive/90", "focus-visible:ring-ring");
    expect(btn).not.toHaveClass("bg-primary");
  });

  it("AlertDialogAction with no variant keeps the default look", () => {
    expect(open()).toHaveClass("bg-primary");
  });
});
