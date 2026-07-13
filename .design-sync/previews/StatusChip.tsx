/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { StatusChip } from '@wardyn/ui';

export const Statuses = () => (
  <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8 }}>
    <StatusChip status="ready" />
    <StatusChip status="needs-setup" />
    <StatusChip status="connected" />
    <StatusChip status="incompatible" />
    <StatusChip status="checking" />
  </div>
);
