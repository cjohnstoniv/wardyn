/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// AI Run Composer — interactive CLARIFY step. When the composer needs more detail
// before proposing, it returns clarifying QUESTIONS; this screen collects the
// operator's answers and hands them back as a transcript for the next round.
//
// Per-question shape (composer.Question): `options` non-empty ⇒ choose from them
// (single = radio, multi = checkboxes) PLUS an always-present free-text "Other";
// `options` empty ⇒ a free-text answer. The component is self-contained: it owns
// the in-progress answers and produces ComposeQA[] on submit. Remount it per round
// (key=round) for clean state.
//
// Questions are shown ONE AT A TIME (paginated by `currentIndex`) with a rail
// for context; `answers` still holds the full round so Prev/Next never lose
// input, and submit() (fired from "Continue" on the last question) emits the
// whole round exactly as before.
import * as React from "react";
import { ArrowLeft, Info, Loader2, MessageCircleQuestion, Sparkles } from "lucide-react";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Textarea } from "../../ui/textarea";
import { Checkbox } from "../../ui/checkbox";
import { RadioGroup, RadioGroupItem } from "../../ui/radio-group";
import { Label } from "../../ui/label";
import { Popover, PopoverContent, PopoverTrigger } from "../../ui/popover";
import { AskPopover } from "./ask-popover";
import type { ComposeQA, ComposeQuestion } from "../../../lib/types";

const OTHER = "__other__";

// answerState tracks one question's UI selection. For free-text questions only
// `text` is used; for choice questions `selected` holds picked options and `text`
// the optional "Other" value. `note` is the always-on elaboration box — kept
// separate from `text` since that field already backs the free-text answer AND
// the "Other" value on choice questions.
type AnswerState = { selected: string[]; text: string; note: string };

// answerToText renders one question's selection as the answer string sent to the
// model: picked options joined with ", ", plus any free-text ("Other") value,
// plus the elaboration note (appended after an em dash, or standalone if the
// question is otherwise unanswered).
function answerToText(q: ComposeQuestion, a: AnswerState): string {
  const base =
    q.options.length === 0
      ? a.text.trim()
      : [...a.selected.filter((s) => s !== OTHER), a.text.trim()].filter(Boolean).join(", ");
  const note = (a.note ?? "").trim();
  if (!note) return base;
  return base ? `${base} — ${note}` : note;
}

// answered reports whether a question has a usable answer (gates Continue).
function answered(q: ComposeQuestion, a: AnswerState | undefined): boolean {
  if (!a) return false;
  return answerToText(q, a).length > 0;
}

export function ComposeQandA({
  questions,
  assumptions,
  notes,
  round,
  submitting,
  onSubmit,
  onSkip,
  onBack,
}: {
  questions: ComposeQuestion[];
  assumptions?: string[];
  notes?: string;
  round: number;
  submitting: boolean;
  onSubmit: (answers: ComposeQA[]) => void;
  onSkip: () => void;
  onBack: () => void;
}) {
  const [answers, setAnswers] = React.useState<Record<string, AnswerState>>({});
  const [currentIndex, setCurrentIndex] = React.useState(0);

  const get = (id: string): AnswerState => answers[id] ?? { selected: [], text: "", note: "" };
  const set = (id: string, next: AnswerState) =>
    setAnswers((prev) => ({ ...prev, [id]: next }));

  const current = questions[currentIndex];
  const isLast = currentIndex === questions.length - 1;
  const currentAnswered = answered(current, answers[current.id]);

  const submit = () => {
    if (!questions.every((q) => answered(q, answers[q.id]))) return;
    onSubmit(
      questions.map((q) => ({ question: q.question, answer: answerToText(q, get(q.id)) })),
    );
  };

  const goNext = () => {
    if (!currentAnswered) return;
    if (isLast) submit();
    else setCurrentIndex((i) => i + 1);
  };

  return (
    <div className="space-y-5">
      <div className="flex items-center gap-2 text-sm text-foreground">
        <MessageCircleQuestion className="size-4 text-primary" />
        <span className="font-medium">A few questions before I propose</span>
        {round > 0 && (
          <span className="text-[0.6875rem] text-muted-foreground">round {round + 1}</span>
        )}
      </div>

      {notes && <p className="text-sm text-muted-foreground">{notes}</p>}

      {assumptions && assumptions.length > 0 && (
        <div className="rounded-lg border border-border bg-muted/30 p-3">
          <div className="mb-1 text-[0.6875rem] uppercase tracking-wide text-muted-foreground">
            Working assumptions
          </div>
          <ul className="list-disc space-y-0.5 pl-4 text-xs text-muted-foreground">
            {assumptions.map((a, i) => (
              <li key={i}>{a}</li>
            ))}
          </ul>
        </div>
      )}

      {/* question rail — restores context that one-at-a-time removes */}
      {questions.length > 1 && (
        <ol className="flex flex-wrap gap-x-3 gap-y-1 text-xs">
          {questions.map((q, i) => (
            <li key={q.id}>
              <button
                type="button"
                onClick={() => setCurrentIndex(i)}
                className={
                  i === currentIndex
                    ? "font-semibold text-foreground"
                    : "text-muted-foreground hover:text-foreground"
                }
              >
                {i + 1}. {q.question}
              </button>
            </li>
          ))}
        </ol>
      )}

      <div className="text-[0.6875rem] uppercase tracking-wide text-muted-foreground">
        Question {currentIndex + 1} of {questions.length}
      </div>

      <QuestionRow
        key={current.id}
        q={current}
        value={get(current.id)}
        onChange={(v) => set(current.id, v)}
        notes={notes}
      />

      <div className="flex items-center justify-between border-t border-border pt-4">
        {currentIndex === 0 ? (
          <Button variant="ghost" onClick={onBack} disabled={submitting}>
            <ArrowLeft className="size-4" /> Back
          </Button>
        ) : (
          <Button variant="ghost" onClick={() => setCurrentIndex((i) => i - 1)} disabled={submitting}>
            <ArrowLeft className="size-4" /> Prev
          </Button>
        )}
        <div className="flex items-center gap-2">
          <Button variant="outline" onClick={onSkip} disabled={submitting}>
            Skip &amp; propose anyway
          </Button>
          <Button onClick={goNext} disabled={submitting || !currentAnswered}>
            {submitting ? (
              <Loader2 className="size-4 animate-spin" />
            ) : (
              <Sparkles className="size-4" />
            )}
            {isLast ? "Continue" : "Next"}
          </Button>
        </div>
      </div>
    </div>
  );
}

function QuestionRow({
  q,
  value,
  onChange,
  notes,
}: {
  q: ComposeQuestion;
  value: AnswerState;
  onChange: (v: AnswerState) => void;
  notes?: string;
}) {
  const isChoice = q.options.length > 0;
  const otherSelected = value.selected.includes(OTHER);
  const hasMeta = Boolean(
    q.why || q.help || q.risk || q.examples?.length || q.misconceptions?.length,
  );

  return (
    <div className="rounded-lg border border-border p-3" data-testid="qanda-question">
      <div className="flex items-center gap-1.5">
        <div className="text-sm font-medium text-foreground">{q.question}</div>
        {hasMeta && (
          <Popover>
            <PopoverTrigger asChild>
              <Button
                type="button"
                variant="ghost"
                size="icon"
                className="size-5 p-0 text-muted-foreground hover:text-foreground"
                aria-label={`${q.question} — more info`}
              >
                <Info className="size-3.5" />
              </Button>
            </PopoverTrigger>
            <PopoverContent align="start" className="w-80 space-y-2 text-xs">
              {q.why && <p className="text-foreground">{q.why}</p>}
              {q.help && <p className="text-muted-foreground">{q.help}</p>}
              {q.risk && <p className="text-warning">{q.risk}</p>}
              {q.examples && q.examples.length > 0 && (
                <ul className="list-disc space-y-0.5 pl-4 text-muted-foreground">
                  {q.examples.map((e, i) => (
                    <li key={i}>{e}</li>
                  ))}
                </ul>
              )}
              {q.misconceptions && q.misconceptions.length > 0 && (
                <ul className="list-disc space-y-0.5 pl-4 text-muted-foreground">
                  {q.misconceptions.map((m, i) => (
                    <li key={i}>{m}</li>
                  ))}
                </ul>
              )}
            </PopoverContent>
          </Popover>
        )}
      </div>

      {/* free-text-only question */}
      {!isChoice && (
        <Textarea
          aria-label={q.question}
          value={value.text}
          onChange={(e) => onChange({ ...value, text: e.target.value })}
          rows={2}
          className="mt-2"
          placeholder="Your answer…"
        />
      )}

      {/* single-select: radio + an "Other" free-text */}
      {isChoice && !q.multi && (
        <RadioGroup
          className="mt-2 gap-1.5"
          value={value.selected[0] ?? ""}
          onValueChange={(v) => onChange({ ...value, selected: [v], text: v === OTHER ? value.text : "" })}
        >
          {q.options.map((opt, i) => (
            <div key={i} className="flex items-center gap-2">
              <RadioGroupItem value={opt} id={`${q.id}-opt-${i}`} />
              <Label htmlFor={`${q.id}-opt-${i}`} className="cursor-pointer text-sm font-normal text-foreground">
                {opt}
              </Label>
            </div>
          ))}
          <div className="flex items-center gap-2">
            <RadioGroupItem value={OTHER} id={`${q.id}-other`} />
            <Label htmlFor={`${q.id}-other`} className="cursor-pointer text-sm font-normal text-foreground">
              Other
            </Label>
          </div>
        </RadioGroup>
      )}

      {/* multi-select: checkboxes + an "Other" free-text */}
      {isChoice && q.multi && (
        <div className="mt-2 space-y-1.5">
          {q.options.map((opt, i) => {
            const checked = value.selected.includes(opt);
            return (
              <div key={i} className="flex items-center gap-2">
                <Checkbox
                  id={`${q.id}-opt-${i}`}
                  checked={checked}
                  onCheckedChange={(c) =>
                    onChange({
                      ...value,
                      selected: c === true
                        ? [...value.selected, opt]
                        : value.selected.filter((s) => s !== opt),
                    })
                  }
                />
                <Label htmlFor={`${q.id}-opt-${i}`} className="cursor-pointer text-sm font-normal text-foreground">
                  {opt}
                </Label>
              </div>
            );
          })}
          <div className="flex items-center gap-2">
            <Checkbox
              id={`${q.id}-other`}
              checked={otherSelected}
              onCheckedChange={(c) =>
                onChange({
                  ...value,
                  selected: c === true
                    ? [...value.selected, OTHER]
                    : value.selected.filter((s) => s !== OTHER),
                  text: c === true ? value.text : "",
                })
              }
            />
            <Label htmlFor={`${q.id}-other`} className="cursor-pointer text-sm font-normal text-foreground">
              Other
            </Label>
          </div>
        </div>
      )}

      {/* the "Other" free-text box, shown when Other is picked on a choice question */}
      {isChoice && otherSelected && (
        <Input
          aria-label={`${q.question} — other`}
          value={value.text}
          onChange={(e) => onChange({ ...value, text: e.target.value })}
          className="mt-2"
          placeholder="Describe…"
        />
      )}

      <div className="mt-2 flex items-center justify-between gap-2">
        <AskPopover context={{ step: "clarify", currentQuestion: q.question, notes }} />
      </div>

      {/* always-on elaboration — never reuses `text` (shared with the free-text
          answer and "Other") so it can't clobber the picked answer. */}
      <Textarea
        aria-label={`${q.question} — elaborate`}
        value={value.note}
        onChange={(e) => onChange({ ...value, note: e.target.value })}
        rows={2}
        className="mt-2"
        placeholder="Add detail (optional)…"
      />
    </div>
  );
}
