/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { Chip } from '@wardyn/ui';

export const Tones = () => (
  <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8 }}>
    <Chip tone="neutral">neutral</Chip>
    <Chip tone="success">ready</Chip>
    <Chip tone="warning">needs setup</Chip>
    <Chip tone="danger">denied</Chip>
    <Chip tone="info">info</Chip>
    <Chip tone="cyan">CC2</Chip>
    <Chip tone="primary">Fence</Chip>
  </div>
);
export const WithDot = () => (
  <Chip tone="success" dot>Claude connected</Chip>
);
