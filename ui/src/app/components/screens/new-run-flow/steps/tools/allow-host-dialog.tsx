/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Allow a host: one more host the run may reach with no credential, sent as
// overrides.hosts.add_allowed. The organisation's ceiling still decides at review.

import * as React from "react";
import { Input } from "../../../../ui/input";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";
import { hostError } from "../../../new-run/custom-component-form-model";
import { ToolDialog, ToolField } from "./tool-dialog";

export function AllowHostDialog({ onAdd, onClose }: { onAdd: (host: string) => void; onClose: () => void }) {
  const id = React.useId();
  const [value, setValue] = React.useState("");
  const [submitted, setSubmitted] = React.useState(false);
  const host = value.trim();
  const error = !submitted ? "" : !host ? NR_TOOLS.HOST_ERR : (hostError(host) ?? "");

  const confirm = () => {
    setSubmitted(true);
    if (host && !hostError(host)) onAdd(host);
  };

  return (
    <ToolDialog title={NR_TOOLS.HOST_CARD_TITLE} intro={NR_TOOLS.HOST_INTRO} firstField={id} confirm={NR_TOOLS.ADD_TO_RUN} onConfirm={confirm} onClose={onClose}>
      <ToolField id={id} label={NR_TOOLS.HOST_LABEL} error={error}>
        <Input
          id={id}
          value={value}
          placeholder={NR_TOOLS.HOST_PLACEHOLDER}
          autoComplete="off"
          spellCheck={false}
          aria-invalid={error ? true : undefined}
          aria-describedby={error ? `${id}-err` : undefined}
          onChange={(e) => { setValue(e.target.value); setSubmitted(false); }}
        />
      </ToolField>
    </ToolDialog>
  );
}
