/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The conversation: the person's messages and the assistant's text in order, each a plain text node, and one
// collapsed line per tool call. No markdown, no images, no links: a message is characters, and a URL in it stays text.
// Person letters are put back to names here, for display only; what is posted back is exactly what was typed.
import * as React from "react";
import { SendHorizontal } from "lucide-react";
import { Button } from "../../ui/button";
import { cn } from "../../ui/utils";
import { ASSISTANT as T } from "../../wardyn/copy/assistant";
import { restoreLetters, type ChatItem, type TurnUsage } from "./composer-stream";
import { ToolLine, usageAfter } from "./composer-trail";

export interface ComposerChatProps {
  items: readonly ChatItem[];
  letters: Readonly<Record<string, string>>;
  turns: readonly TurnUsage[];
  /** False while the assistant is not ready, or the chat has ended: nothing can be sent. */
  canSend: boolean;
  onSend: (text: string) => Promise<void>;
  /** Lines under the messages: the provider notice, the ended wording, an error. */
  children?: React.ReactNode;
}

export function ComposerChat({ items, letters, turns, canSend, onSend, children }: ComposerChatProps) {
  const [text, setText] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const send = async () => {
    const typed = text;
    if (typed.trim() === "" || busy || !canSend) return;
    setBusy(true);
    try {
      await onSend(typed);
      setText("");
    } catch {
      // The screen shows why; what was typed stays in the box.
    } finally {
      setBusy(false);
    }
  };
  return (
    <section data-testid="composer-chat" className="flex min-w-0 flex-col gap-2.5">
      <ol className="flex flex-col gap-2.5">
        {items.map((it) => (
          <li key={`${it.gen}-${it.kind}-${it.seq}`} className={cn(it.kind === "user" && "self-end")}>
            {it.kind === "tool" ? (
              <ToolLine item={it} usage={usageAfter(turns, it)} />
            ) : (
              <p
                data-testid={`composer-message-${it.kind}`}
                className={cn("max-w-[60ch] whitespace-pre-wrap break-words rounded-lg px-3 py-2 text-body", it.kind === "user" ? "bg-primary/12 text-foreground" : "bg-card text-foreground")}
              >
                {restoreLetters(it.text, letters)}
              </p>
            )}
          </li>
        ))}
      </ol>
      {children}
      <form
        className="flex items-end gap-2 rounded-xl border border-input px-2 py-1.5"
        onSubmit={(e) => {
          e.preventDefault();
          void send();
        }}
      >
        <textarea
          aria-label={T.CHAT_PLACEHOLDER}
          placeholder={T.CHAT_PLACEHOLDER}
          rows={2}
          value={text}
          disabled={!canSend || busy}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
              e.preventDefault();
              void send();
            }
          }}
          className="min-h-9 min-w-0 flex-1 resize-none bg-transparent text-sm outline-none disabled:opacity-50"
        />
        <Button type="submit" disabled={!canSend || busy || text.trim() === ""}>
          <SendHorizontal aria-hidden="true" className="size-4" />
          {T.SEND}
        </Button>
      </form>
    </section>
  );
}
