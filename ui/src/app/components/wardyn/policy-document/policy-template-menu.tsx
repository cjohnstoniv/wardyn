/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "Start from a template" (#2836, owner option C): one compact toolbar button that opens a menu of the worked specs,
// each label over its hint, instead of five buttons under the editor. Shared by the policy dialog and New Run's
// custom-policy editor, so both draw it the same way and test ids stay the same.
import { ChevronDown } from "lucide-react";
import { Button } from "../../ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "../../ui/dropdown-menu";
import { cn } from "../../ui/utils";
import { POLICY_TEMPLATE_COPY as C } from "../copy/policy-templates";
import type { PolicyTemplate } from "../policy-panel";
import { POLICY_COPY_BUTTON } from "./policy-document";

export function PolicyTemplateMenu({
  templates,
  onPick,
}: {
  templates: readonly PolicyTemplate[];
  onPick: (t: PolicyTemplate) => void;
}) {
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button type="button" variant="outline" size="sm" className={POLICY_COPY_BUTTON} data-testid="policy-template-menu">
          {C.START}
          <ChevronDown className="size-3.5" aria-hidden />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="start" className="max-w-sm">
        {templates.map((t) => (
          <DropdownMenuItem
            key={t.id}
            data-testid={`policy-template-${t.id}`}
            className="flex-col items-start gap-0.5"
            onClick={() => onPick(t)}
          >
            <span className={cn("text-sm font-semibold", t.id === "allow-all" ? "text-warning" : "text-foreground")}>{t.label}</span>
            <span className="text-xs text-muted-foreground">{t.hint}</span>
          </DropdownMenuItem>
        ))}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
