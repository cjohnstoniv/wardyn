/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { Checkbox } from '@wardyn/ui';

export const Unchecked = () => <Checkbox aria-label="unchecked" />;
export const Checked = () => <Checkbox defaultChecked aria-label="checked" />;
export const WithLabel = () => (
  <label style={{ display: 'flex', alignItems: 'center', gap: 8, fontSize: 14 }}>
    <Checkbox defaultChecked /> Require approval before egress
  </label>
);
