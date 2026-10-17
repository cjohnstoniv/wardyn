/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { DemoDetail } from '@wardyn/ui';
import { DEMOS } from '../../ui/src/app/components/screens/demos/demo-catalog';

export const RunnerUnavailable = () => (
  <DemoDetail demo={DEMOS[0]} barrierReady={false} onDemoLaunched={() => {}} />
);
