/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// One visible line that says why the assistant is not available, for one state. A line, not a tooltip, so it
// reads on touch. Only a super admin is offered the way to turn it on; everyone else reads the reason alone.
// Nothing is rendered when the assistant is ready.
import { Link } from "react-router-dom";
import type { AssistantState } from "../../lib/api/assistant";
import { RAIL_MODEL_ACCESS } from "./model-access-copy";
import { Button } from "../ui/button";
import { ASSISTANT as T } from "./copy/assistant";
import { useModelAccessDoor } from "./model-access-context";
import { useOperator, useOperatorResolved } from "./operator-context";

export type AvailabilityState = AssistantState | "kind_never_sent" | "provider_error";

export interface AssistantAvailabilityProps {
  state: AvailabilityState;
  /** The provider named by the line: what to connect, what the unit denies, what did not answer. */
  providerId?: string;
  providerName?: string;
  /** The label of the record kind kept from the assistant, for `kind_never_sent`. */
  kindLabel?: string;
  /** Opens the provider's own connect flow; the shared door when absent. */
  onConnect?: (providerId: string) => void;
  /** Asks the composer to try the provider again, for `provider_error`. */
  onRetry?: () => void;
}

const SETTINGS_PATH = "/admin/settings";

export function AssistantAvailability({ state, providerId, providerName = "", kindLabel = "", onConnect, onRetry }: AssistantAvailabilityProps) {
  const door = useModelAccessDoor();
  // Fail closed while /me has not answered: the way on is shown to a super admin only.
  const operator = useOperator();
  const resolved = useOperatorResolved();
  const superAdmin = operator && resolved;
  const connect = () => (onConnect ? onConnect(providerId ?? "") : door.openDoor({ for: { provider: providerId ?? "" } }));
  const line = (text: string, action?: React.ReactNode) => (
    <p data-testid={`assistant-availability-${state}`} className="flex flex-wrap items-center gap-x-2 text-meta text-muted-foreground">
      <span>{text}</span>
      {action}
    </p>
  );
  switch (state) {
    case "off":
      return line(
        T.OFF,
        superAdmin && (
          <Link className="underline" to={SETTINGS_PATH}>
            {T.OFF_LINK}
          </Link>
        ),
      );
    case "no_provider":
      return line(T.NO_PROVIDER);
    case "unit_denies":
      return line(T.UNIT_DENIES(providerName));
    case "not_connected":
      return line(
        T.NOT_CONNECTED(providerName),
        <Button size="sm" variant="outline" onClick={connect}>
          {RAIL_MODEL_ACCESS.NO_PROVIDER_CTA}
        </Button>,
      );
    case "kind_never_sent":
      return line(T.KIND_NEVER_SENT(kindLabel));
    case "at_run_limit":
      return line(T.AT_RUN_LIMIT);
    case "provider_error":
      return line(
        T.PROVIDER_ERROR(providerName),
        <Button size="sm" variant="outline" onClick={onRetry}>
          {T.TRY_AGAIN}
        </Button>,
      );
    default:
      return null;
  }
}
