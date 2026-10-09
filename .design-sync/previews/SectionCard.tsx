/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { Chip, NewRunSectionCard, SectionCard } from '@wardyn/ui';

export const Policy = () => (
  <SectionCard title="Policy"><Chip tone="neutral">Custom policy</Chip></SectionCard>
);
export const NewRun = () => (
  <NewRunSectionCard title="Policy"><Chip tone="neutral">Custom policy</Chip></NewRunSectionCard>
);
