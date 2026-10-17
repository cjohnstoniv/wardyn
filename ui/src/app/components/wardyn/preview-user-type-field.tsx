/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { GOVERNANCE as GOV } from "../../lib/governance-copy";
import { useUserTypes } from "../../lib/use-user-types";
import { USER_TYPE_STANDARD } from "../../lib/types/user-types";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../ui/select";
import { Field, fieldHintId } from "./form-primitives";

// The user-type picker both claims previews (Governance → Resolved profile,
// Drives → Who gets what) carry (R1-0097). A person's user type is part of what
// a token resolves to, so a preview that always assumed the built-in Standard
// type could never match a custom-type assignment.
//
// Hidden while the only type is the built-in Standard one — a one-option
// dropdown is not a choice (owner, 2026-10-17) — and while the list is loading
// or failed to read, when the preview keeps sending `standard`.
export function usePreviewUserTypes() {
  const { userTypes } = useUserTypes();
  return { userTypes, show: userTypes.some((t) => !t.built_in) };
}

export function PreviewUserTypeField({
  id,
  value,
  onChange,
  userTypes,
}: {
  id: string;
  value: string;
  onChange: (id: string) => void;
  userTypes: { id: string; name: string }[];
}) {
  return (
    <Field label={GOV.PREVIEW_USER_TYPE_LABEL} htmlFor={id} hint={GOV.PREVIEW_USER_TYPE_HINT}>
      <Select value={value || USER_TYPE_STANDARD} onValueChange={onChange}>
        <SelectTrigger id={id} aria-label={GOV.PREVIEW_USER_TYPE_LABEL} aria-describedby={fieldHintId(id)}>
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          {userTypes.map((t) => (
            <SelectItem key={t.id} value={t.id}>
              {t.name}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    </Field>
  );
}
