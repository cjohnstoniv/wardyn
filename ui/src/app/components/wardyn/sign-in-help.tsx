/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #484 — the admin's own "what to do next", under Wardyn's refusal sentence on
// the sign-in page (and in the People step's preview of it). ONE component in
// both homes so the preview is the page.
//
// The text is a React text node and nothing else: no markup, no linkify —
// it is published on the anonymous /healthz, and whatever an admin types,
// `<a href>` included, must read back as the characters they typed. The link
// is the one element with an href, under a fixed label (Q457-7), and only for
// an http(s) address: the server already refuses anything else on write and
// drops it on read, and this page is the last place that could turn a bad
// stored value into a script URL.
import { SIGNIN_HELP_LINK_LABEL } from "../../lib/people-access-copy";

const HTTP_URL = /^https?:\/\//i;

export function SignInHelp({ text, url }: { text?: string; url?: string }) {
  const link = url && HTTP_URL.test(url) ? url : "";
  if (!text && !link) return null;
  return (
    <div data-testid="sign-in-help" className="mt-2 space-y-1 px-1 text-xs text-muted-foreground">
      {text && <p className="break-words">{text}</p>}
      {link && (
        <a
          href={link}
          target="_blank"
          rel="noopener noreferrer"
          className="font-medium text-foreground underline underline-offset-2"
        >
          {SIGNIN_HELP_LINK_LABEL}
        </a>
      )}
    </div>
  );
}
