/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { cn } from "../ui/utils";

/** The two admin page columns (#3058): "list" for tables and boards, "form" for settings, editors and reading
 *  pages, so a page's title lands on the same x as every other page of its kind. One top padding for both; the
 *  side gutter is 16 px on a phone and 24 px from sm up. Flows (Setup, Getting started, New run, Sign in) keep
 *  their own layout. */
export function PageContainer({
  width,
  className,
  children,
}: {
  width: "list" | "form";
  className?: string;
  children: React.ReactNode;
}) {
  return (
    <div
      data-page-width={width}
      className={cn(
        "mx-auto w-full px-4 pt-8 pb-6 sm:px-6",
        width === "list" ? "max-w-[1200px]" : "max-w-[900px]",
        className,
      )}
    >
      {children}
    </div>
  );
}
