/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Router, type Location, type To } from "react-router-dom";

/** A router that applies each navigation `delayMs` late, the way the app's router answers a keystroke a render late. */
export function LaggingRouter({ children, delayMs = 30 }: { children: React.ReactNode; delayMs?: number }) {
  const [location, setLocation] = React.useState<Location>({ pathname: "/", search: "", hash: "", state: null, key: "start" });
  const navigator = React.useMemo(() => {
    const land = (to: To) => {
      const t = typeof to === "string" ? { search: to.startsWith("?") ? to : "" } : to;
      setTimeout(() => setLocation({ pathname: "/", search: t.search ?? "", hash: "", state: null, key: String(Math.random()) }), delayMs);
    };
    return {
      createHref: (to: To) => (typeof to === "string" ? to : `${to.pathname ?? ""}${to.search ?? ""}`),
      go: () => {},
      push: land,
      replace: land,
    };
  }, [delayMs]);
  return (
    <Router location={location} navigator={navigator}>
      {children}
    </Router>
  );
}
