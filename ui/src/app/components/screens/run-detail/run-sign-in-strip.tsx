/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The waiting-sign-in strip above the login-sandbox note (approved 088 mock,
// M2 section A): the code, the one link that finishes it, and what each state
// of the read looks like. Nothing here takes focus; there is no dismiss.
import { ExternalLink, KeyRound, Loader2 } from "lucide-react";
import { Button } from "../../ui/button";
import { CopyButton } from "../../wardyn/copy-button";
import { Mono } from "../../wardyn/code-block";
import { RUN_SIGN_IN } from "../../wardyn/copy";
import { STATES } from "../../wardyn/states";
import { DeviceCode } from "../settings/signin-progress";
import { useRunSignIn } from "./use-run-sign-in";

function Opens({ url }: { url: string }) {
  const host = new URL(url).host;
  const line = RUN_SIGN_IN.OPENS(host);
  const at = line.indexOf(host);
  return (
    <p className="text-meta text-muted-foreground">
      {line.slice(0, at)}
      <Mono>{host}</Mono>
      {line.slice(at + host.length)}
    </p>
  );
}

export default function RunSignInStrip({ runId, createdAt }: { runId: string; createdAt: string }) {
  const read = useRunSignIn(runId, createdAt, true);
  const w = read.waiting;
  return (
    <>
      {!w && !read.ended && !read.failed && read.checking && (
        <div role="status" className="flex shrink-0 items-center gap-2 text-xs text-muted-foreground">
          <Loader2 className="size-3.5 animate-spin" aria-hidden />
          {RUN_SIGN_IN.CHECKING}
        </div>
      )}
      {w && (
        <div
          role="status"
          className="flex shrink-0 items-start gap-2 rounded-lg border border-warning/30 bg-warning-subtle px-2.5 py-2"
          data-testid="run-sign-in-strip"
        >
          <KeyRound className="mt-0.5 size-3.5 shrink-0 text-warning" aria-hidden />
          <div className="min-w-0 space-y-2">
            <div>
              <p className="text-xs font-medium text-foreground">{RUN_SIGN_IN.TITLE}</p>
              <p className="text-meta text-muted-foreground">{RUN_SIGN_IN.BODY}</p>
            </div>
            <div className="flex flex-wrap items-center gap-2">
              <DeviceCode code={w.user_code} label={RUN_SIGN_IN.CODE_LABEL} />
              <Button size="sm" asChild>
                <a href={w.verification_url} target="_blank" rel="noopener noreferrer">
                  <ExternalLink className="size-3.5" aria-hidden /> {RUN_SIGN_IN.OPEN}
                </a>
              </Button>
              <CopyButton
                text={w.user_code}
                label={RUN_SIGN_IN.COPY}
                className="gap-1.5 rounded-md border border-border px-2 py-1 text-xs text-muted-foreground hover:text-foreground"
              >
                {RUN_SIGN_IN.COPY}
              </CopyButton>
            </div>
            <Opens url={w.verification_url} />
          </div>
        </div>
      )}
      {read.ended && <p className="shrink-0 text-xs text-muted-foreground">{RUN_SIGN_IN.NO_LONGER}</p>}
      {read.failed && (
        <div className="flex shrink-0 flex-wrap items-center gap-2 text-xs text-warning">
          <p>{RUN_SIGN_IN.READ_FAILED}</p>
          <Button size="sm" variant="outline" onClick={read.retry}>
            {STATES.RETRY}
          </Button>
        </div>
      )}
    </>
  );
}
