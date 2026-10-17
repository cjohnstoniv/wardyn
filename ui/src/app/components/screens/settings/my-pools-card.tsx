/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Your account -> My pools as a card of its own (FUG-L404). My pools sits inside My runner, and that
// card is not drawn while runners are off for the organisation, which left a person whose pools hold
// only remote-hosted runners nowhere in the console to manage them (and New Run's links to
// /account#my-runner led nowhere). When My runner is not drawn for that reason, this card is, with the
// same section; otherwise the section stays inside My runner and this card is not drawn.
import * as React from "react";
import { useLocation } from "react-router-dom";
import { RUNNERS_DISABLED } from "../../../lib/api/runners";
import { MY_POOLS } from "../../wardyn/copy/runner-pools";
import { CollapsibleCard } from "../../wardyn/collapsible-card";
import { MyPoolsSection } from "./my-pools-section";
import { MyRunnerCard } from "./my-runner-card";
import { useMyPools } from "./use-my-pools";

const NO_RUNNERS: never[] = [];

export function MyPoolsCard() {
  const location = useLocation();
  const ref = React.useRef<HTMLElement | null>(null);
  const [open, setOpen] = React.useState(false);
  const pools = useMyPools(true);
  const shown = pools.status !== "unavailable";
  // The links that used to land on My runner land here, once per navigation and never on a data refresh.
  React.useEffect(() => {
    if (shown && (location.hash === "#my-runner" || location.hash === "#my-pools")) {
      setOpen(true);
      ref.current?.focus({ focusVisible: true } as FocusOptions);
    }
  }, [location.key, shown]); // eslint-disable-line react-hooks/exhaustive-deps -- location.key, not the hash
  if (!shown) return null;
  const summary = pools.status === "ready" ? (pools.pools.length === 0 ? MY_POOLS.SUMMARY_NONE : MY_POOLS.SUMMARY_COUNT(pools.pools.length)) : undefined;
  return (
    <CollapsibleCard
      title={MY_POOLS.TITLE}
      summary={summary}
      id="my-pools-card"
      ref={ref}
      tabIndex={-1}
      open={open}
      onOpenChange={setOpen}
      className="outline-none focus-visible:border-ring focus-visible:ring-ring focus-visible:ring-[3px]"
    >
      <MyPoolsSection bare pools={pools} runners={NO_RUNNERS} />
    </CollapsibleCard>
  );
}

/** My runner, and My pools beside it when My runner is off the page because runners are off. */
export function MyRunnerAndPoolsCards() {
  const [runnersOff, setRunnersOff] = React.useState(false);
  const hidden = React.useCallback((reason: string) => setRunnersOff(reason === RUNNERS_DISABLED), []);
  return (
    <>
      <MyRunnerCard onHidden={hidden} />
      {runnersOff && <MyPoolsCard />}
    </>
  );
}
