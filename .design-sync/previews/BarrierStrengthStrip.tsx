/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { BarrierStrengthStrip } from '@wardyn/ui';

export const Fence = () => <BarrierStrengthStrip tier="CC1" />;
export const Wall = () => <BarrierStrengthStrip tier="CC2" />;
export const Vault = () => <BarrierStrengthStrip tier="CC3" />;
export const Muted = () => <BarrierStrengthStrip tier="CC3" muted />;
