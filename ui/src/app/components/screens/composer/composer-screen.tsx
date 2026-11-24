/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The AI composer. The order is fixed: read the target from the URL, ask whether the assistant is ready, show exactly
// what will be sent, and only on Start make a session. Then the assistant starts as a run under the person's name, the
// chat opens beside its proposals, and the screen says plainly when the assistant cannot work or has ended.
import * as React from "react";
import { Loader2, Sparkles } from "lucide-react";
import { useLocation, useNavigate } from "react-router-dom";
import { assistant, type AssistantAvailability as AvailabilityAnswer, type AssistantPreview, type AssistantSessionView, type AssistantTarget } from "../../../lib/api/assistant";
import { HttpError } from "../../../lib/api/core";
import { composerPurpose, composerTarget } from "../../../lib/composer-url";
import { getErrorMessage } from "../../../lib/format";
import { Button } from "../../ui/button";
import { AssistantAvailability, type AvailabilityState } from "../../wardyn/assistant-availability";
import { ASSISTANT as T } from "../../wardyn/copy/assistant";
import { Notice } from "../../wardyn/notice";
import { Chip } from "../../wardyn/primitives";
import { usePrincipal } from "../../wardyn/operator-context";
import { ComposerChat } from "./composer-chat";
import { ComposerDraftPanel, draftLines } from "./composer-draft-panel";
import { PhoneDraftBar, PhoneDraftSheet, useIsPhone } from "./composer-phone";
import { ComposerPreview, kindLabel } from "./composer-preview";
import { ComposerStarting } from "./composer-starting";
import { EMPTY_COMPOSER, foldEvents, openComposerStream, type ComposerState, type ToolItem } from "./composer-stream";
import { ComposerTrail } from "./composer-trail";

// The server's reasons for a refusal that is one of the assistant's own unavailable states.
const REASON_STATE: Record<string, AvailabilityState> = {
  assistant_off: "off",
  assistant_no_provider: "no_provider",
  assistant_unit_denies: "unit_denies",
  assistant_not_connected: "not_connected",
  assistant_kind_never_sent: "kind_never_sent",
  assistant_at_run_limit: "at_run_limit",
};

interface Unavailable {
  state: AvailabilityState;
  providerId?: string;
  providerName?: string;
  kindLabel?: string;
}

const unavailableFrom = (e: unknown, target: AssistantTarget): Unavailable | null => {
  if (!(e instanceof HttpError)) return null;
  const state = REASON_STATE[e.reason];
  if (!state) return null;
  return { state, providerId: undefined, providerName: e.provider, kindLabel: kindLabel(target.kind === "unit" ? "governance_unit" : target.kind) };
};

// The record kind a unit draft is about, as the organisation's never-sent list names it.
const NEVER_SENT_UNIT = "governance_unit";

type Phase =
  | { name: "loading" }
  | { name: "unavailable"; why: Unavailable }
  | { name: "failed"; message: string }
  | { name: "preview"; preview: AssistantPreview; changed: boolean; busy: boolean; error: string | null }
  | { name: "session"; session: AssistantSessionView; endedBy: Unavailable | null; closed: boolean };

export function ComposerScreen() {
  const location = useLocation();
  const navigate = useNavigate();
  const person = usePrincipal();
  const phone = useIsPhone();
  const target = React.useMemo(() => composerTarget(location.search), [location.search]);
  const purpose = composerPurpose(target);

  const [phase, setPhase] = React.useState<Phase>({ name: "loading" });
  const [fold, setFold] = React.useState<ComposerState>(EMPTY_COMPOSER);
  const [letters, setLetters] = React.useState<Readonly<Record<string, string>>>({});
  const [pollEnded, setPollEnded] = React.useState(false);
  const [streamFailed, setStreamFailed] = React.useState<string | null>(null);
  const [chatError, setChatError] = React.useState<string | null>(null);
  const [sheet, setSheet] = React.useState(false);

  // The one live session, so leaving the screen can end it, and the stream that reads it.
  const live = React.useRef<string | null>(null);
  const closeStream = React.useRef<(() => void) | null>(null);

  const stopStream = React.useCallback(() => {
    closeStream.current?.();
    closeStream.current = null;
  }, []);

  const loadPreview = React.useCallback(
    async (changed: boolean) => {
      try {
        const preview = await assistant.postPreview(purpose, target);
        setLetters(preview.letters);
        setPhase({ name: "preview", preview, changed, busy: false, error: null });
      } catch (e) {
        const why = unavailableFrom(e, target);
        setPhase(why ? { name: "unavailable", why } : { name: "failed", message: getErrorMessage(e) });
      }
    },
    [purpose, target],
  );

  // Step 1: is the assistant ready for this purpose? Not ready: the reason, and nothing else.
  const begin = React.useCallback(
    async (carry: ComposerState) => {
      stopStream();
      // A session that ended is not ended again by leaving; the next Start makes its own.
      live.current = null;
      setFold(carry);
      setPollEnded(false);
      setStreamFailed(null);
      setChatError(null);
      setPhase({ name: "loading" });
      let answer: AvailabilityAnswer;
      try {
        answer = await assistant.getMyAssistant(purpose);
      } catch (e) {
        setPhase({ name: "failed", message: getErrorMessage(e) });
        return;
      }
      if (answer.state !== "ready") {
        setPhase({ name: "unavailable", why: { state: answer.state, providerId: answer.provider_id, providerName: answer.provider_name } });
        return;
      }
      if (target.kind === "unit" && answer.never_sent_kinds?.includes(NEVER_SENT_UNIT)) {
        setPhase({ name: "unavailable", why: { state: "kind_never_sent", kindLabel: kindLabel(NEVER_SENT_UNIT) } });
        return;
      }
      await loadPreview(false);
    },
    [loadPreview, purpose, stopStream, target.kind],
  );

  React.useEffect(() => {
    void begin(EMPTY_COMPOSER);
    return () => {
      closeStream.current?.();
      closeStream.current = null;
      // Leaving the screen ends the session; the person's own End did that already.
      const id = live.current;
      live.current = null;
      if (id) void assistant.endSession(id).catch(() => undefined);
    };
    // The route reads its target once per URL.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [location.search]);

  const start = async () => {
    if (phase.name !== "preview" || phase.busy) return;
    const { preview } = phase;
    setPhase({ ...phase, busy: true, error: null });
    try {
      const session = await assistant.createSession({ purpose, target, preview_digest: preview.digest });
      live.current = session.id;
      setLetters(session.letters ?? preview.letters);
      setPhase({ name: "session", session, endedBy: null, closed: false });
      closeStream.current = openComposerStream({
        sessionId: session.id,
        onEvents: (events) => setFold((f) => foldEvents(f, events)),
        onEnded: () => setPollEnded(true),
        onLetters: (l) => Object.keys(l).length > 0 && setLetters(l),
        onFailed: (e) => setStreamFailed(getErrorMessage(e)),
      });
    } catch (e) {
      if (e instanceof HttpError && e.reason === "assistant_preview_stale") {
        // Nothing starts until the person has read the new preview and pressed Start again.
        await loadPreview(true);
        return;
      }
      const why = unavailableFrom(e, target);
      if (why) setPhase({ name: "unavailable", why });
      else setPhase({ ...phase, busy: false, error: getErrorMessage(e) });
    }
  };

  const send = async (text: string) => {
    if (phase.name !== "session") return;
    setChatError(null);
    try {
      await assistant.postMessage(phase.session.id, text);
    } catch (e) {
      if (e instanceof HttpError && e.status === 409) {
        // The session is over: the assistant was turned off, its provider no longer serves this person, or it ended
        // meanwhile. The chat ends the way an end does, with the assistant's own reason, when there is one, in place of the ended
        // wording, and the draft stays as it was.
        stopStream();
        live.current = null;
        setPhase({ ...phase, endedBy: unavailableFrom(e, target), closed: true });
      } else {
        setChatError(getErrorMessage(e));
      }
      throw e;
    }
  };

  const end = async () => {
    if (phase.name !== "session") return;
    const id = phase.session.id;
    stopStream();
    live.current = null;
    setPhase({ ...phase, closed: true });
    try {
      await assistant.endSession(id);
    } catch {
      // The session is closed here either way; the server also ends it when it goes idle.
    }
  };

  const again = (keepChat: boolean) => void begin({ ...EMPTY_COMPOSER, proposal: fold.proposal, items: keepChat ? fold.items : [] });

  const over = phase.name === "session" && (phase.closed || phase.endedBy !== null || fold.ended || pollEnded || fold.toolsUnavailable || fold.providerError);
  const title = (
    <header className="flex flex-wrap items-center justify-between gap-2.5">
      <h1 className="text-display">{T.COMPOSER_TITLE}</h1>
      {phase.name === "session" && (
        <div className="flex items-center gap-2">
          <Chip tone="primary">
            <Sparkles aria-hidden="true" className="size-3" />
            {T.PROVIDER_CHIP(`${phase.session.provider} · ${phase.session.model}`)}
          </Chip>
          {!over && (
            <Button type="button" variant="outline" size="sm" onClick={() => void end()}>
              {T.END}
            </Button>
          )}
        </div>
      )}
    </header>
  );

  let body: React.ReactNode;
  if (phase.name === "loading") {
    body = <Loader2 aria-busy="true" className="size-5 animate-spin text-muted-foreground" />;
  } else if (phase.name === "failed") {
    body = <Notice tone="danger">{phase.message}</Notice>;
  } else if (phase.name === "unavailable") {
    const { why } = phase;
    body = <AssistantAvailability state={why.state} providerId={why.providerId} providerName={why.providerName} kindLabel={why.kindLabel} onRetry={() => void begin(EMPTY_COMPOSER)} />;
  } else if (phase.name === "preview") {
    body = (
      <ComposerPreview
        preview={phase.preview}
        changed={phase.changed}
        busy={phase.busy}
        error={phase.error}
        onStart={() => void start()}
        onCancel={() => navigate(-1)}
      />
    );
  } else {
    const { session } = phase;
    const ready = fold.ready;
    const tools = fold.items.filter((i): i is ToolItem => i.kind === "tool");
    // What ended the chat, most specific first: the tools line wins over the ordinary ended event that follows it.
    const ending: "tools" | "reason" | "ended" | null = fold.toolsUnavailable
      ? "tools"
      : phase.endedBy
        ? "reason"
        : phase.closed || fold.ended || pollEnded
          ? "ended"
          : null;
    if (!ready && ending === null && !fold.providerError && !fold.items.length) {
      body = <ComposerStarting provider={session.provider} runId={session.run_id} fetching={session.state === "fetching"} />;
    } else {
      const draft = <ComposerDraftPanel proposal={fold.proposal} />;
      const sideBySide = (
        <>
          {phone ? (
            <>
              <PhoneDraftBar lines={draftLines(fold.proposal)} left={fold.proposal?.dropped.length ?? 0} onShow={() => setSheet(true)} />
              <PhoneDraftSheet open={sheet} onClose={() => setSheet(false)}>
                {draft}
              </PhoneDraftSheet>
            </>
          ) : null}
          <div className="grid min-w-0 gap-4 min-[760px]:grid-cols-[minmax(0,420px)_minmax(0,1fr)]">
            <ComposerChat items={fold.items} letters={letters} turns={fold.turns} canSend={ready && ending === null && !fold.providerError} onSend={send}>
              {fold.providerError && ending !== "tools" && ending !== "reason" && (
                <AssistantAvailability state="provider_error" providerName={session.provider} onRetry={() => again(true)} />
              )}
              {ending === "tools" && <Notice tone="warning">{T.TOOLS_UNAVAILABLE}</Notice>}
              {ending === "reason" && phase.endedBy && (
                <AssistantAvailability state={phase.endedBy.state} providerId={phase.endedBy.providerId} providerName={phase.endedBy.providerName} kindLabel={phase.endedBy.kindLabel} />
              )}
              {ending === "ended" && !fold.providerError && <Notice tone="info">{T.ENDED}</Notice>}
              {((ending === "ended" && !fold.providerError) || ending === "reason") && (
                <div>
                  <Button type="button" variant="outline" onClick={() => again(false)}>
                    {T.START_AGAIN}
                  </Button>
                </div>
              )}
              {chatError && <p role="alert" className="text-body text-danger">{chatError}</p>}
              {streamFailed && <p role="alert" className="text-body text-danger">{streamFailed}</p>}
            </ComposerChat>
            {!phone && (
              <div className="min-w-0 space-y-4">
                {draft}
                <ComposerTrail
                  tools={tools}
                  suggestion={fold.suggestion}
                  audit={{
                    person,
                    purpose: purpose === "review" ? T.REVIEW : T.COMPOSE,
                    provider: session.provider,
                    model: session.model,
                    linked: target.id ?? "",
                    runHref: session.run_id ? `/runs/${session.run_id}` : null,
                  }}
                />
              </div>
            )}
          </div>
        </>
      );
      body = sideBySide;
    }
  }

  return (
    <div className="mx-auto flex w-full max-w-[1240px] flex-col gap-4 px-4 py-6">
      {title}
      {body}
    </div>
  );
}

export default ComposerScreen;
