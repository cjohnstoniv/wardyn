/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Escalation-only "Ask" help affordance. The composer already emits rich inert
// metadata (why/help/risk/examples/misconceptions) that answers most questions with
// NO round-trip; this popover is the ESCALATION for anything the metadata doesn't
// cover. It calls the advisory /assist endpoint with the current step's structured
// context and renders the answer as INERT text — it has no authority over the
// proposal/policy, so the operator still reviews and approves everything.
import * as React from "react";
import { HelpCircle, Loader2, Send } from "lucide-react";

import { api, HttpError } from "../../../lib/api";
import type { ComposeAssistContext } from "../../../lib/types";
import { Button } from "../../ui/button";
import { Popover, PopoverContent, PopoverTrigger } from "../../ui/popover";
import { Textarea } from "../../ui/textarea";

export function AskPopover({
  context,
  triggerLabel = "Ask something else",
}: {
  context: ComposeAssistContext;
  triggerLabel?: string;
}) {
  const [question, setQuestion] = React.useState("");
  const [answer, setAnswer] = React.useState<string>();
  const [loading, setLoading] = React.useState(false);
  const [error, setError] = React.useState<string>();

  const submit = async () => {
    const q = question.trim();
    if (!q || loading) return;
    setLoading(true);
    setError(undefined);
    setAnswer(undefined);
    try {
      setAnswer(await api.ask({ ...context, question: q }));
    } catch (e) {
      setError(e instanceof HttpError ? e.message : "Couldn't get an answer. Try again.");
    } finally {
      setLoading(false);
    }
  };

  return (
    <Popover>
      <PopoverTrigger asChild>
        <Button
          variant="ghost"
          size="sm"
          className="h-7 gap-1 px-2 text-xs text-muted-foreground hover:text-foreground"
        >
          <HelpCircle className="size-3.5" />
          {triggerLabel}
        </Button>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-96 space-y-2">
        <div className="text-xs font-medium text-foreground">Ask the assistant</div>
        <Textarea
          rows={2}
          value={question}
          onChange={(e) => setQuestion(e.target.value)}
          placeholder="e.g. What does egress mean? Is allowing GitHub safe?"
          onKeyDown={(e) => {
            if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) void submit();
          }}
        />
        <div className="flex justify-end">
          <Button size="sm" onClick={() => void submit()} disabled={loading || !question.trim()}>
            {loading ? <Loader2 className="size-3.5 animate-spin" /> : <Send className="size-3.5" />}
            Ask
          </Button>
        </div>
        {error && <p className="text-xs text-destructive">{error}</p>}
        {answer && (
          <div className="whitespace-pre-wrap rounded-md border border-border bg-muted/40 p-2 text-xs leading-relaxed text-foreground">
            {answer}
          </div>
        )}
        <p className="text-[10px] leading-snug text-muted-foreground">
          Advisory help — it can&apos;t change your sandbox. You still review and approve everything.
        </p>
      </PopoverContent>
    </Popover>
  );
}
