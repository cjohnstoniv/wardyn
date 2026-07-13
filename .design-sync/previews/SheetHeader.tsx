/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { SheetHeader } from '@wardyn/ui';

export const Header = () => (
  <SheetHeader>
    <h2 style={{ fontWeight: 600, fontSize: 16 }}>New run</h2>
    <p style={{ opacity: 0.7, fontSize: 13 }}>Configure the barrier and egress for this run.</p>
  </SheetHeader>
);
