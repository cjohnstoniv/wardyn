/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { RunDetailCommandBar, Tabs, TabsList, TabsTrigger } from '@wardyn/ui';

export const Overview = () => (
  <Tabs defaultValue="overview">
    <RunDetailCommandBar tabs={
      <TabsList className="h-7 bg-transparent p-0">
        {['Overview', 'Approvals', 'Policy', 'Audit', 'Recording', 'Output'].map((label) => (
          <TabsTrigger key={label} value={label.toLowerCase()} className="h-7 text-xs">{label}</TabsTrigger>
        ))}
      </TabsList>
    } />
  </Tabs>
);
