/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from 'react';
import { OperatorProvider, RunSignInStrip } from '@wardyn/ui';

export function Waiting() {
  const [ready, setReady] = React.useState(false);
  React.useEffect(() => {
    const original = window.fetch;
    // Only this synthetic run is answered locally; no account data enters the card.
    window.fetch = (input, init) => {
      const url = new URL(input instanceof Request ? input.url : String(input), location.href);
      if (url.pathname === '/api/v1/runs/ds-preview/sign-in') {
        return Promise.resolve(Response.json({
          state: 'waiting', user_code: 'ABCD-EFGH', verification_url: 'https://sso.example.com/',
        }));
      }
      return original(input, init);
    };
    setReady(true);
    return () => { window.fetch = original; };
  }, []);
  return ready && (
    <OperatorProvider operator={false} principal="ds-preview">
      <RunSignInStrip runId="ds-preview" createdAt="2024-05-15T12:00:00Z" />
    </OperatorProvider>
  );
}
